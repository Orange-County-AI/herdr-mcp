package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Queue. Concurrency is deliberately low and backlog deliberately
// high: the point is to make Herdr see a steady trickle of work instead of a
// burst it drops, while callers wait rather than fail.
const (
	defaultConcurrency     = 8
	defaultLongConcurrency = 64
	defaultBacklog         = 256
	defaultOutageGrace     = 2 * time.Minute
	probeInterval          = 500 * time.Millisecond
	probeTimeout           = 2 * time.Second
)

// longPollMethods block inside Herdr waiting for something to happen. They hold
// a connection open for up to fifteen minutes while consuming no Herdr capacity,
// so charging them against the same budget as real work would let a handful of
// idle waiters starve every other call.
var longPollMethods = map[string]bool{
	"agent.wait":           true,
	"agent.prompt":         true,
	"events.wait":          true,
	"pane.wait_for_output": true,
}

// ErrSaturated is returned when a lane's backlog is already full. Shedding here
// is deliberate: an unbounded queue during an outage guarantees that every
// caller waits and then times out, which is strictly worse than telling the
// newest arrivals immediately.
var ErrSaturated = errors.New("herdr-mcp queue is saturated")

// Queue is the admission-control layer between MCP tool calls and the Herdr
// socket. It does three things the raw Client cannot:
//
//  1. bounds how many requests are in flight against Herdr at once, so a burst
//     of tool calls does not overrun Herdr's accept backlog;
//  2. parks callers while Herdr is unreachable and releases them when it comes
//     back, turning a restart into latency instead of a wall of failures; and
//  3. keeps a single prober on the socket during an outage, so N waiting
//     callers do not become N reconnect attempts per second.
//
// Only a failed dial is retried. See UnavailableError for why anything past
// that point is not safe to resend.
type Queue struct {
	Client *Client
	// Concurrency bounds simultaneous non-blocking calls. Zero uses the default.
	Concurrency int
	// LongConcurrency bounds simultaneous long-poll calls. Zero uses the default.
	LongConcurrency int
	// Backlog bounds callers waiting for a slot in either lane, per lane. Zero
	// uses the default.
	Backlog int
	// OutageGrace is how long a single call waits for Herdr to come back before
	// giving up. It is measured from that call's own arrival, so a caller that
	// shows up an hour into an outage still gets a full grace period rather than
	// an instant failure. Zero uses the default; negative disables waiting.
	OutageGrace time.Duration
	// Logf receives outage transitions. Nil uses log.Printf.
	Logf func(format string, args ...any)

	initOnce sync.Once
	short    *lane
	long     *lane

	mu        sync.Mutex
	down      bool
	downSince time.Time
	revived   chan struct{}
	probing   bool

	// protocol is the last protocol a successful probe observed, and mismatch
	// records that it disagreed with the schema the tools were built from.
	expectProtocol int
	lastProtocol   atomic.Int64
	mismatch       atomic.Bool

	stop context.Context
}

// Availability is the queue's view of Herdr, for /healthz and operator logs.
type Availability struct {
	Available        bool   `json:"available"`
	Protocol         int    `json:"protocol,omitempty"`
	ExpectedProtocol int    `json:"expected_protocol,omitempty"`
	ProtocolMismatch bool   `json:"protocol_mismatch,omitempty"`
	DownForSeconds   int    `json:"down_for_seconds,omitempty"`
	Waiting          int    `json:"waiting"`
	InFlight         int    `json:"in_flight"`
	Detail           string `json:"detail,omitempty"`
}

// NewQueue wires a queue to a client. stop bounds the lifetime of the outage
// prober; expectProtocol is the protocol of the schema the tools were built
// from, and a probe that disagrees with it marks the bridge mismatched rather
// than letting it serve tools that no longer match the running Herdr.
func NewQueue(stop context.Context, client *Client, expectProtocol int) *Queue {
	return &Queue{Client: client, expectProtocol: expectProtocol, stop: stop}
}

func (q *Queue) init() {
	q.initOnce.Do(func() {
		if q.stop == nil {
			q.stop = context.Background()
		}
		q.short = newLane(positive(q.Concurrency, defaultConcurrency), positive(q.Backlog, defaultBacklog))
		q.long = newLane(positive(q.LongConcurrency, defaultLongConcurrency), positive(q.Backlog, defaultBacklog))
	})
}

// Call runs one Herdr method through admission control, waiting out an outage
// where it can. It satisfies the same contract as Client.Call.
func (q *Queue) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	q.init()
	if q.mismatch.Load() {
		return nil, fmt.Errorf("herdr-mcp built its tools from protocol %d but the running Herdr reports protocol %d; restart herdr-mcp so it re-reads the schema",
			q.expectProtocol, q.lastProtocol.Load())
	}

	lane := q.short
	if longPollMethods[method] {
		lane = q.long
	}
	if err := lane.acquire(ctx); err != nil {
		if errors.Is(err, ErrSaturated) {
			return nil, fmt.Errorf("%w: %d calls already waiting for Herdr; retry %s shortly", ErrSaturated, lane.backlog, method)
		}
		return nil, err
	}
	defer lane.release()

	return q.callWaitingOutOutage(ctx, method, params)
}

func (q *Queue) callWaitingOutOutage(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	grace := q.OutageGrace
	if grace == 0 {
		grace = defaultOutageGrace
	}
	deadline := time.Now().Add(grace)

	for {
		result, err := q.Client.Call(ctx, method, params)
		if err == nil {
			q.markUp(0)
			return result, nil
		}

		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			// The request reached Herdr. Whatever happened next is the caller's
			// answer, not something to retry behind their back.
			return nil, err
		}
		q.markDown(unavailable)
		if ctx.Err() != nil || grace < 0 || !time.Now().Before(deadline) {
			return nil, q.outageError(method, err)
		}
		if err := q.waitForRevival(ctx, deadline); err != nil {
			return nil, q.outageError(method, err)
		}
	}
}

func (q *Queue) outageError(method string, cause error) error {
	q.mu.Lock()
	since := q.downSince
	down := q.down
	q.mu.Unlock()
	if !down {
		return cause
	}
	return fmt.Errorf("Herdr has been unreachable for %s, so %s was not delivered: %w",
		time.Since(since).Round(time.Second), method, cause)
}

// waitForRevival parks until the prober sees Herdr answer, the caller leaves,
// or this call's grace period runs out.
func (q *Queue) waitForRevival(ctx context.Context, deadline time.Time) error {
	q.mu.Lock()
	revived := q.revived
	down := q.down
	q.mu.Unlock()
	if !down || revived == nil {
		// Herdr came back between the failed call and this wait. Retry at once.
		return nil
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-revived:
		return nil
	case <-timer.C:
		return context.DeadlineExceeded
	case <-ctx.Done():
		return ctx.Err()
	case <-q.stop.Done():
		return q.stop.Err()
	}
}

func (q *Queue) markDown(cause error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.down {
		return
	}
	q.down = true
	q.downSince = time.Now()
	q.revived = make(chan struct{})
	q.logf("herdr: socket unreachable (%v); parking calls and probing every %s", cause, probeInterval)
	if !q.probing {
		q.probing = true
		go q.probe()
	}
}

func (q *Queue) markUp(protocol int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if protocol > 0 {
		q.lastProtocol.Store(int64(protocol))
		if q.expectProtocol > 0 && protocol != q.expectProtocol {
			if q.mismatch.CompareAndSwap(false, true) {
				q.logf("herdr: protocol changed from %d to %d; tools are stale until herdr-mcp restarts", q.expectProtocol, protocol)
			}
		} else {
			q.mismatch.Store(false)
		}
	}
	if !q.down {
		return
	}
	q.logf("herdr: socket answered again after %s; releasing parked calls", time.Since(q.downSince).Round(time.Millisecond))
	q.down = false
	close(q.revived)
	q.revived = nil
}

// probe is the single reconnect attempt shared by every parked caller. It runs
// only while Herdr is down and exits as soon as it answers.
func (q *Queue) probe() {
	defer func() {
		q.mu.Lock()
		q.probing = false
		q.mu.Unlock()
	}()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.stop.Done():
			return
		case <-ticker.C:
		}
		q.mu.Lock()
		down := q.down
		q.mu.Unlock()
		if !down {
			return
		}
		ctx, cancel := context.WithTimeout(q.stop, probeTimeout)
		_, protocol, err := q.Client.Ping(ctx)
		cancel()
		if err == nil {
			q.markUp(protocol)
			return
		}
	}
}

// Availability reports what /healthz and operators need: whether Herdr is
// reachable, for how long it has not been, and how much work is stacked up.
func (q *Queue) Availability() Availability {
	q.init()
	q.mu.Lock()
	down := q.down
	since := q.downSince
	q.mu.Unlock()

	status := Availability{
		Available:        !down,
		Protocol:         int(q.lastProtocol.Load()),
		ExpectedProtocol: q.expectProtocol,
		ProtocolMismatch: q.mismatch.Load(),
		Waiting:          q.short.waitingNow() + q.long.waitingNow(),
		InFlight:         q.short.inFlightNow() + q.long.inFlightNow(),
	}
	if down {
		status.DownForSeconds = int(time.Since(since).Round(time.Second) / time.Second)
		status.Detail = "Herdr socket unreachable; calls are parked until it returns"
	}
	if status.ProtocolMismatch {
		status.Detail = "Herdr protocol no longer matches the registered tools; restart herdr-mcp"
	}
	return status
}

// MarkObserved lets startup feed a successful ping into the queue so the first
// tool call does not have to rediscover that Herdr is up.
func (q *Queue) MarkObserved(protocol int) {
	q.init()
	q.markUp(protocol)
}

// MarkUnreachable lets startup record that Herdr was already down, so the
// prober is running before the first tool call arrives.
func (q *Queue) MarkUnreachable(cause error) {
	q.init()
	q.markDown(cause)
}

func (q *Queue) logf(format string, args ...any) {
	if q.Logf != nil {
		q.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// lane is a counting semaphore with a bounded waiting room.
type lane struct {
	slots   chan struct{}
	backlog int
	waiting atomic.Int64
}

func newLane(concurrency, backlog int) *lane {
	return &lane{slots: make(chan struct{}, concurrency), backlog: backlog}
}

func (l *lane) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}
	if l.waiting.Add(1) > int64(l.backlog) {
		l.waiting.Add(-1)
		return ErrSaturated
	}
	defer l.waiting.Add(-1)
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *lane) release() { <-l.slots }

func (l *lane) waitingNow() int  { return int(l.waiting.Load()) }
func (l *lane) inFlightNow() int { return len(l.slots) }

func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
