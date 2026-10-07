package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type localConnection struct {
	queue  *Queue
	cancel context.CancelFunc
}

type verifiedQueue struct{ queue *Queue }

func (v verifiedQueue) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return v.queue.CallVerified(ctx, method, params)
}

func escapeSelector(value string) string { return url.PathEscape(value) }

// connectionKey includes the destination and session: editing a saved profile
// or selecting another session must never reuse a forward to its old socket.
func connectionKey(machine Machine) string {
	return machine.ID + "\x00" + machine.Target + "\x00" + machine.SessionName()
}

func (p *Pool) route(ctx context.Context, selector string) (Transport, error) {
	if selector == "" {
		return nil, fmt.Errorf("no session or machine selector given")
	}
	local, id, name, err := parseSelector(selector)
	if err != nil {
		return nil, err
	}
	if local {
		sessions, err := ListSessions(ctx, p.Binary)
		if err != nil {
			return nil, err
		}
		session, err := selectSession(sessions, name)
		if err != nil {
			return nil, err
		}
		return p.connectLocal(session)
	}
	// Routing requires a fresh list. A cached profile could have been removed,
	// disabled, or edited; using it would dispatch to the wrong destination.
	listCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	machines, err := ListMachines(listCtx, p.Binary)
	if err != nil {
		return nil, err
	}
	if id != "" {
		machine, err := selectMachineID(machines, id)
		if err != nil {
			return nil, err
		}
		if name != "" {
			machine.Session = name
		}
		return p.connect(ctx, machine, name != "")
	}
	sessions, err := ListSessions(ctx, p.Binary)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve bare selector %q safely: %w; use an explicit ssh: selector for a saved machine", selector, err)
	}
	var localMatch bool
	for _, session := range sessions {
		localMatch = localMatch || session.Name == selector
	}
	var machineMatch bool
	for _, machine := range machines {
		machineMatch = machineMatch || machine.ID == selector || machine.Label == selector
	}
	if localMatch && machineMatch {
		return nil, fmt.Errorf("selector %q matches both a local session and a saved machine; use %q or a saved machine's ssh: selector from machine_list", selector, localSelector(selector))
	}
	if localMatch {
		session, err := selectSession(sessions, selector)
		if err != nil {
			return nil, err
		}
		return p.connectLocal(session)
	}
	machine, err := SelectMachine(machines, selector)
	if err != nil {
		return nil, err
	}
	return p.connect(ctx, machine, false)
}

func selectMachineID(machines []Machine, id string) (Machine, error) {
	for _, machine := range machines {
		if machine.ID == id {
			if !machine.Enabled {
				return Machine{}, fmt.Errorf("machine %q is disabled", machine.Label)
			}
			return machine, nil
		}
	}
	return Machine{}, fmt.Errorf("no saved Herdr machine has profile id %q; use machine_list", id)
}

func (p *Pool) connectLocal(session Session) (Transport, error) {
	info, err := os.Stat(session.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("session %q socket %s is missing or inaccessible: %w", session.Name, session.SocketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("session %q path %s is not a socket", session.Name, session.SocketPath)
	}
	socketPath, err := filepath.EvalSymlinks(session.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("resolve session %q socket: %w", session.Name, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.stop.Err(); err != nil {
		return nil, err
	}
	if p.StartupQueue != nil {
		startupInfo, err := os.Stat(p.StartupQueue.Client.SocketPath)
		if err == nil && os.SameFile(info, startupInfo) {
			return verifiedQueue{queue: p.StartupQueue}, nil
		}
	}
	if local := p.locals[socketPath]; local != nil {
		return local.queue, nil
	}
	ctx, cancel := context.WithCancel(p.stop)
	queue := NewQueue(ctx, &Client{SocketPath: socketPath}, p.Protocol)
	queue.VerifyProtocol = true
	if p.Tune != nil {
		p.Tune(queue)
	}
	p.locals[socketPath] = &localConnection{queue: queue, cancel: cancel}
	return queue, nil
}

// SetProtocol invalidates routes verified against the previous tool schema.
func (p *Pool) SetProtocol(protocol int) {
	p.mu.Lock()
	p.Protocol = protocol
	p.mu.Unlock()
	p.Close()
}

type Roster struct {
	LocalSessions      []Session      `json:"local_sessions"`
	Machines           []RemoteStatus `json:"machines"`
	LocalSessionsError string         `json:"local_sessions_error,omitempty"`
	MachinesError      string         `json:"machines_error,omitempty"`
}

func (p *Pool) remoteSessions(ctx context.Context, machine Machine) ([]Session, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	select {
	case p.sessionProbes <- struct{}{}:
		defer func() { <-p.sessionProbes }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return listRemoteSessions(ctx, machine)
}

// Roster discovers local sessions and saved profiles on every request. Remote
// listing is bounded and partial: an unavailable host gets its own error,
// while other hosts and local sessions remain discoverable.
func (p *Pool) Roster(ctx context.Context) (Roster, error) {
	roster := Roster{}
	sessions, err := ListSessions(ctx, p.Binary)
	p.mu.Lock()
	if err != nil {
		roster.LocalSessionsError = err.Error()
		sessions = p.sessions
	} else {
		p.sessions = sessions
	}
	p.mu.Unlock()
	roster.LocalSessions = sessions
	listCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	machines, err := ListMachines(listCtx, p.Binary)
	p.mu.Lock()
	if err != nil {
		roster.MachinesError = err.Error()
		machines = p.machines
	} else {
		p.machines = machines
		p.listedAt = time.Now()
	}
	p.mu.Unlock()
	statuses := make([]RemoteStatus, len(machines))
	var wait sync.WaitGroup
	for index, machine := range machines {
		statuses[index] = p.statusFor(machine)
		if !machine.Enabled {
			continue
		}
		if roster.MachinesError != "" {
			statuses[index].SessionsError = "saved machine discovery is unavailable; remote listing skipped"
			continue
		}
		wait.Add(1)
		go func(index int, machine Machine) {
			defer wait.Done()
			found, err := p.remoteSessions(listCtx, machine)
			if err != nil {
				statuses[index].SessionsError = err.Error()
				return
			}
			statuses[index].Sessions = found
		}(index, machine)
	}
	wait.Wait()
	roster.Machines = statuses
	return roster, nil
}
