package herdr

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Pool lazily connects saved machines and hands out one queue per machine.
//
// Connections are lazy because a bridge that dialled all fourteen saved
// machines at boot would spend a minute on SSH handshakes to hosts nobody asked
// about, and would fail to start whenever one of them was asleep. They are
// reaped because an idle forward is a live SSH connection and a socket, and a
// machine that went away should not keep either.
type Pool struct {
	// Binary is the Herdr binary consulted for `machine list --json`.
	Binary string
	// Protocol is the schema protocol the tools were registered from. A machine
	// speaking anything else is refused rather than driven with wrong tools.
	Protocol int
	// RuntimeDir holds the control and forwarded sockets.
	RuntimeDir string
	// IdleTimeout disconnects a machine untouched for this long. Zero uses the
	// default; negative keeps connections until shutdown.
	IdleTimeout time.Duration
	// Tune is applied to each machine's queue, so a remote gets the same
	// admission control as the local session.
	Tune func(*Queue)
	// StartupQueue is shared when an explicit local selector names the bridge's
	// own socket. Aliases must not multiply that session's admission budget.
	StartupQueue *Queue
	// Logf receives connection lifecycle lines. Nil uses log.Printf.
	Logf func(format string, args ...any)

	stop          context.Context
	sessionProbes chan struct{}

	mu         sync.Mutex
	remotes    map[string]*Remote
	locals     map[string]*localConnection
	dialing    map[string]chan struct{}
	generation uint64
	machines   []Machine
	sessions   []Session
	listedAt   time.Time
	reaping    bool
}

const (
	defaultIdleTimeout = 15 * time.Minute
	machineListTTL     = 30 * time.Second
	reapInterval       = time.Minute
)

// NewPool builds a pool bound to stop, which tears every connection down when
// cancelled.
func NewPool(stop context.Context, binary string, protocol int, runtimeDir string) *Pool {
	return &Pool{
		Binary:        binary,
		Protocol:      protocol,
		RuntimeDir:    runtimeDir,
		stop:          stop,
		sessionProbes: make(chan struct{}, 4),
		remotes:       map[string]*Remote{},
		locals:        map[string]*localConnection{},
		dialing:       map[string]chan struct{}{},
	}
}

// DefaultRuntimeDir picks a short, writable directory for the forwarded
// sockets. $XDG_RUNTIME_DIR is preferred: a unix socket path is capped near 108
// bytes, systemd user services always have one, and it is cleaned on logout.
func DefaultRuntimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "herdr-mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create runtime directory %s: %w", dir, err)
	}
	return dir, nil
}

// Machines returns saved profiles for health reporting. During a binary
// upgrade it can report the previous list; dispatch always requires a fresh one.
func (p *Pool) Machines(ctx context.Context) ([]Machine, error) {
	p.mu.Lock()
	if time.Since(p.listedAt) < machineListTTL && p.machines != nil {
		machines := p.machines
		p.mu.Unlock()
		return machines, nil
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	machines, err := ListMachines(ctx, p.Binary)
	if err != nil {
		// A stale list beats no list: `herdr machine list` needs the binary, and
		// the binary is missing during exactly the upgrades this bridge exists to
		// survive.
		p.mu.Lock()
		cached := p.machines
		p.mu.Unlock()
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.machines = machines
	p.listedAt = time.Now()
	p.mu.Unlock()
	return machines, nil
}

// Caller resolves a machine selector to its transport, connecting on first use.
// An empty selector is the caller's local session and is not this pool's job.
func (p *Pool) Caller(ctx context.Context, selector string) (Transport, error) {
	return p.route(ctx, selector)
}

// connect returns the live connection for machine, dialling it if needed. Only
// one dial per machine runs at a time: a burst of tool calls naming a cold
// machine would otherwise open a burst of independent SSH handshakes, and
// OpenSSH scores aborted handshakes as auth failures under PerSourcePenalties,
// which gets this host blocked for minutes.
func (p *Pool) connect(ctx context.Context, machine Machine, verifySession bool) (*Remote, error) {
	key := connectionKey(machine)
	for {
		p.mu.Lock()
		if remote, ok := p.remotes[key]; ok {
			remote.Touch()
			p.mu.Unlock()
			return remote, nil
		}
		if waiting, ok := p.dialing[key]; ok {
			p.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		p.dialing[key] = done
		protocol, generation := p.Protocol, p.generation
		p.mu.Unlock()

		// Session discovery shares the dial gate: a burst naming a cold session
		// must not run a burst of independent SSH probes before this gate.
		var remote *Remote
		var err error
		if verifySession {
			var sessions []Session
			sessions, err = p.remoteSessions(ctx, machine)
			if err == nil {
				_, err = selectSession(sessions, machine.Session)
			}
		}
		if err == nil {
			remote, err = DialRemote(p.stop, machine, protocol, p.RuntimeDir)
		}
		p.mu.Lock()
		delete(p.dialing, key)
		close(done)
		if err == nil && (generation != p.generation || p.stop.Err() != nil) {
			p.mu.Unlock()
			remote.Close()
			return nil, fmt.Errorf("routing changed while connecting to %s; retry with the current tools", machine.Label)
		}
		if err == nil {
			if p.Tune != nil {
				p.Tune(remote.Queue)
			}
			p.remotes[key] = remote
			p.startReaperLocked()
		}
		p.mu.Unlock()
		if err != nil {
			return nil, err
		}
		p.logf("machine: connected to %s (herdr %s, protocol %d)", machine.Label, remote.Version, remote.Protocol)
		return remote, nil
	}
}

// Disconnect drops one machine's connection. The next call to it reconnects.
func (p *Pool) Disconnect(id string) bool {
	p.mu.Lock()
	var remotes []*Remote
	for key, remote := range p.remotes {
		if remote.Machine.ID == id {
			remotes = append(remotes, remote)
			delete(p.remotes, key)
		}
	}
	p.mu.Unlock()
	for _, remote := range remotes {
		remote.Close()
	}
	return len(remotes) > 0
}

// RemoteStatus is one connected machine, for /healthz and machine_list.
type RemoteStatus struct {
	ID            string              `json:"id"`
	Label         string              `json:"label"`
	Target        string              `json:"target"`
	Session       string              `json:"session,omitempty"`
	Enabled       bool                `json:"enabled"`
	Connected     bool                `json:"connected"`
	Version       string              `json:"herdr_version,omitempty"`
	Protocol      int                 `json:"protocol,omitempty"`
	IdleForSecs   int                 `json:"idle_for_seconds,omitempty"`
	Availability  *Availability       `json:"herdr,omitempty"`
	Selector      string              `json:"selector"`
	Sessions      []Session           `json:"sessions,omitempty"`
	SessionsError string              `json:"sessions_error,omitempty"`
	Connections   []SessionConnection `json:"connections,omitempty"`
}

// SessionConnection reports each forwarded session without confusing it with
// the saved profile's configured session.
type SessionConnection struct {
	Session      string       `json:"session"`
	Version      string       `json:"herdr_version,omitempty"`
	Protocol     int          `json:"protocol"`
	IdleForSecs  int          `json:"idle_for_seconds,omitempty"`
	Availability Availability `json:"herdr"`
}

// Status describes every saved machine, marking the ones this bridge currently
// holds a connection to. It never dials: reporting must not cost an SSH
// handshake to a sleeping laptop.
func (p *Pool) Status(ctx context.Context) []RemoteStatus {
	machines, err := p.Machines(ctx)
	if err != nil {
		machines = nil
	}
	statuses := make([]RemoteStatus, 0, len(machines))
	for _, machine := range machines {
		statuses = append(statuses, p.statusFor(machine))
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Label < statuses[j].Label })
	return statuses
}

func (p *Pool) statusFor(machine Machine) RemoteStatus {
	status := RemoteStatus{ID: machine.ID, Label: machine.Label, Target: machine.Target,
		Session: machine.SessionName(), Enabled: machine.Enabled, Selector: "ssh:" + escapeSelector(machine.ID)}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, remote := range p.remotes {
		if remote.Machine.ID != machine.ID || remote.Machine.Target != machine.Target {
			continue
		}
		availability := remote.Queue.Availability()
		idle := int(remote.IdleFor() / time.Second)
		status.Connected = true
		status.Connections = append(status.Connections, SessionConnection{
			Session: remote.Machine.SessionName(), Version: remote.Version, Protocol: remote.Protocol,
			IdleForSecs: idle, Availability: availability,
		})
		if remote.Machine.SessionName() == machine.SessionName() {
			status.Version, status.Protocol = remote.Version, remote.Protocol
			status.IdleForSecs, status.Availability = idle, &availability
		}
	}
	sort.Slice(status.Connections, func(i, j int) bool { return status.Connections[i].Session < status.Connections[j].Session })
	return status
}

// Close drops every connection. The pool stays usable afterwards; a later call
// reconnects. Shutdown just never makes one.
func (p *Pool) Close() {
	p.mu.Lock()
	p.generation++
	remotes := make([]*Remote, 0, len(p.remotes))
	for _, remote := range p.remotes {
		remotes = append(remotes, remote)
	}
	p.remotes = map[string]*Remote{}
	for _, local := range p.locals {
		local.cancel()
	}
	p.locals = map[string]*localConnection{}
	p.mu.Unlock()
	for _, remote := range remotes {
		remote.Close()
	}
}

func (p *Pool) startReaperLocked() {
	if p.reaping {
		return
	}
	p.reaping = true
	go p.reap()
}

func (p *Pool) reap() {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop.Done():
			p.Close()
			return
		case <-ticker.C:
		}
		idle := p.IdleTimeout
		if idle == 0 {
			idle = defaultIdleTimeout
		}
		if idle < 0 {
			continue
		}
		p.mu.Lock()
		var stale []*Remote
		for id, remote := range p.remotes {
			if remote.IdleFor() < idle {
				continue
			}
			// An in-flight call still counts as use: a long agent_wait can sit
			// well past the idle timeout without touching lastUsed, and reaping
			// it would kill the socket out from under the caller.
			if status := remote.Queue.Availability(); status.InFlight > 0 || status.Waiting > 0 {
				remote.Touch()
				continue
			}
			stale = append(stale, remote)
			delete(p.remotes, id)
		}
		empty := len(p.remotes) == 0
		if empty {
			p.reaping = false
		}
		p.mu.Unlock()
		for _, remote := range stale {
			p.logf("machine: disconnecting %s after %s idle", remote.Machine.Label, idle)
			remote.Close()
		}
		if empty {
			return
		}
	}
}

func (p *Pool) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
