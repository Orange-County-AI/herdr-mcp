package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The bridge exists to outlive Herdr. A call that arrives while the socket is
// gone must wait for it to come back, not fail the tool.
func TestQueueParksCallsUntilHerdrReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	queue := NewQueue(context.Background(), &Client{SocketPath: path}, 0)
	queue.OutageGrace = 10 * time.Second
	queue.Logf = func(string, ...any) {}

	type outcome struct {
		result json.RawMessage
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		result, err := queue.Call(context.Background(), "pane.list", nil)
		results <- outcome{result, err}
	}()

	// Let the call fail its dial and park before Herdr appears.
	time.Sleep(150 * time.Millisecond)
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "ping" {
			return `{"type":"pong","version":"test","protocol":20}`
		}
		return `{"type":"pane_list","panes":[]}`
	})
	defer stop()

	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("parked call failed instead of waiting out the outage: %v", got.err)
		}
		if !strings.Contains(string(got.result), "pane_list") {
			t.Fatalf("result = %s", got.result)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("parked call never resumed after Herdr returned")
	}
}

// Retrying a request Herdr already received could apply a mutation twice, so
// only a failed dial is retryable. Here the request lands and the connection
// dies before the answer: exactly one delivery, and an error for the caller.
func TestQueueNeverResendsARequestHerdrAlreadyReceived(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	var delivered atomic.Int64
	stop := serveSocket(t, path, &delivered, nil) // nil response: read, then hang up
	defer stop()

	queue := NewQueue(context.Background(), &Client{SocketPath: path}, 0)
	queue.OutageGrace = 2 * time.Second
	queue.Logf = func(string, ...any) {}

	if _, err := queue.Call(context.Background(), "pane.close", json.RawMessage(`{"pane_id":"w1:p1"}`)); err == nil {
		t.Fatal("call succeeded despite the connection dropping mid-exchange")
	}
	if got := delivered.Load(); got != 1 {
		t.Fatalf("pane.close reached Herdr %d times, want exactly 1", got)
	}
}

// The queue's first job is to stop a burst of tool calls from becoming a burst
// of simultaneous connections that Herdr drops.
func TestQueueBoundsConcurrentCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	var inFlight, peak atomic.Int64
	stop := serveSocket(t, path, nil, func(map[string]any) string {
		current := inFlight.Add(1)
		for {
			highest := peak.Load()
			if current <= highest || peak.CompareAndSwap(highest, current) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		inFlight.Add(-1)
		return `{"type":"ok"}`
	})
	defer stop()

	queue := NewQueue(context.Background(), &Client{SocketPath: path}, 0)
	queue.Concurrency = 3
	queue.Logf = func(string, ...any) {}

	var group sync.WaitGroup
	for index := 0; index < 24; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := queue.Call(context.Background(), "pane.list", nil); err != nil {
				t.Errorf("call failed: %v", err)
			}
		}()
	}
	group.Wait()
	if got := peak.Load(); got > 3 {
		t.Fatalf("peak concurrent calls = %d, want at most 3", got)
	}
}

// Long polls sit idle inside Herdr for minutes. If they shared the working
// lane they would hold every slot and starve ordinary calls.
func TestQueueKeepsLongPollsOutOfTheWorkingLane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	release := make(chan struct{})
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "agent.wait" {
			<-release
		}
		return `{"type":"ok"}`
	})
	defer stop()

	queue := NewQueue(context.Background(), &Client{SocketPath: path}, 0)
	queue.Concurrency = 2
	queue.LongConcurrency = 8
	queue.Logf = func(string, ...any) {}

	for index := 0; index < 4; index++ {
		go func() { _, _ = queue.Call(context.Background(), "agent.wait", json.RawMessage(`{"target":"a"}`)) }()
	}
	time.Sleep(150 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := queue.Call(context.Background(), "pane.list", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pane.list failed while long polls were parked: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pane.list was starved by parked long polls")
	}
	close(release)
}

// An unbounded queue during an outage just makes everyone wait and then fail.
// Past the backlog the newest arrivals are told immediately.
func TestQueueShedsPastItsBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	release := make(chan struct{})
	stop := serveSocket(t, path, nil, func(map[string]any) string {
		<-release
		return `{"type":"ok"}`
	})
	defer stop()

	queue := NewQueue(context.Background(), &Client{SocketPath: path}, 0)
	queue.Concurrency = 1
	queue.Backlog = 1
	queue.Logf = func(string, ...any) {}

	// One call occupies the single slot, one fills the waiting room.
	for index := 0; index < 2; index++ {
		go func() { _, _ = queue.Call(context.Background(), "pane.list", nil) }()
	}
	time.Sleep(200 * time.Millisecond)

	_, err := queue.Call(context.Background(), "pane.list", nil)
	if !errors.Is(err, ErrSaturated) {
		t.Fatalf("third call error = %v, want ErrSaturated", err)
	}
	if !strings.Contains(err.Error(), "pane.list") {
		t.Fatalf("shed error does not name the method: %v", err)
	}
	close(release)
}

// Waiting is not free. When the grace period runs out the caller gets an error
// that says Herdr was the problem and for how long, not a bare dial failure.
func TestQueueGivesUpAfterTheOutageGrace(t *testing.T) {
	queue := NewQueue(context.Background(), &Client{SocketPath: filepath.Join(t.TempDir(), "absent.sock")}, 0)
	queue.OutageGrace = 200 * time.Millisecond
	queue.Logf = func(string, ...any) {}

	started := time.Now()
	_, err := queue.Call(context.Background(), "pane.list", nil)
	if err == nil {
		t.Fatal("call against a missing socket succeeded")
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("call gave up after %s, before its grace period", elapsed)
	}
	if !strings.Contains(err.Error(), "unreachable for") || !strings.Contains(err.Error(), "pane.list") {
		t.Fatalf("error does not attribute the outage: %v", err)
	}
	if status := queue.Availability(); status.Available {
		t.Fatal("availability still reports Herdr as reachable")
	}
}

// A Herdr that came back on a different protocol is serving something these
// tools no longer describe. Calling it anyway would send well-formed requests
// with the wrong meaning, so the queue refuses and names the fix.
func TestQueueRefusesCallsAfterAProtocolChange(t *testing.T) {
	queue := NewQueue(context.Background(), &Client{SocketPath: "unused"}, 20)
	queue.Logf = func(string, ...any) {}
	queue.MarkObserved(21)

	_, err := queue.Call(context.Background(), "pane.list", nil)
	if err == nil || !strings.Contains(err.Error(), "restart herdr-mcp") {
		t.Fatalf("error = %v, want a protocol-mismatch error naming the fix", err)
	}
	if status := queue.Availability(); !status.ProtocolMismatch {
		t.Fatal("availability does not report the protocol mismatch")
	}
}

// serveSocket answers every connection. A nil respond function reads the
// request and hangs up, standing in for Herdr dying mid-exchange. delivered, if
// set, counts requests that actually reached the server.
func serveSocket(t *testing.T, path string, delivered *atomic.Int64, respond func(map[string]any) string) func() {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var request map[string]any
				if json.Unmarshal(line, &request) != nil {
					return
				}
				if delivered != nil {
					delivered.Add(1)
				}
				if respond == nil {
					return
				}
				_, _ = fmt.Fprintf(conn, "{\"id\":%q,\"result\":%s}\n", request["id"], respond(request))
			}()
		}
	}()
	return func() {
		_ = listener.Close()
		connections.Wait()
	}
}
