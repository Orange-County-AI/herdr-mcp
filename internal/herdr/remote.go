package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Remote is one saved machine reached over SSH.
//
// Herdr's own remote support is CLI-level (`herdr --machine <label> pane list`)
// and covers only the subset of methods its subcommands expose, with flags that
// do not match socket parameters. Forwarding the remote unix socket instead
// gives every method in the schema, over the same Client and Queue the local
// session uses -- there is no second code path to keep in step.
//
// The whole connection is the system `ssh` binary so the user's ~/.ssh/config
// (ProxyJump, IdentityFile, agent, Tailscale aliases) is honored exactly as it
// is on the command line.
type Remote struct {
	Machine  Machine
	Version  string
	Protocol int
	Client   *Client
	Queue    *Queue

	ctlPath   string
	localSock string
	// lastUsed is unix nanoseconds, atomic because the pool's reaper reads it
	// on its own goroutine while tool calls are writing it.
	lastUsed atomic.Int64

	cancel   context.CancelFunc
	done     chan struct{}
	teardown sync.Once
	closed   sync.Once
}

// SSH budgets. connectTimeout bounds the control-master handshake, which on a
// sleeping Tailscale Mac genuinely costs ten seconds or more; forwardReady
// bounds how long the forwarded socket has to start answering once the master
// is up, which is local and therefore quick.
const (
	sshConnectTimeout = 20 * time.Second
	sshProbeTimeout   = 30 * time.Second
	sshForwardReady   = 8 * time.Second
	// sshWaitDelay bounds how long Wait blocks on the pipes after the ssh
	// process is gone. exec's output copiers read until EOF, and EOF needs every
	// holder of the pipe to close it -- including a ProxyCommand ssh forked.
	// Without it a timeout is a promise the context cannot keep.
	sshWaitDelay = 2 * time.Second
	// sshCloseTimeout bounds how long Close waits for the teardown it asked for
	// before doing it itself.
	sshCloseTimeout = 10 * time.Second
)

// remoteHerdrShell runs a herdr command in a login shell with the user-local
// install directories forced onto PATH. A non-interactive ssh command gets a
// minimal PATH on most hosts, and herdr installs to ~/.local/bin, so without
// this the probe reports "herdr not installed" on machines that have it.
func remoteHerdrShell(command string) string {
	return `${SHELL:-sh} -lc 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` + command + `'`
}

// sshCommand builds an ssh invocation bounded by ctx. Two details make it safe
// to run from a request path, and neither is the default: the child gets its
// own process group and cancellation kills the GROUP, so a ProxyCommand dies
// with the ssh that spawned it instead of surviving as an orphan; and WaitDelay
// caps how long Wait blocks after that.
func sshCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := cmd.Process.Kill()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return err
	}
	cmd.WaitDelay = sshWaitDelay
	return cmd
}

// remoteStatus is the part of `herdr status server --json` this bridge needs.
type remoteStatus struct {
	Running  bool   `json:"running"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Socket   string `json:"socket"`
}

// probeMachine asks a machine where its Herdr socket is and what it speaks.
//
// The socket path cannot be assumed: it is ~/.config/herdr/herdr.sock on a
// normal host but /dev/shm/herdr/herdr.sock on an agent box, and a named
// session moves it again. Asking is the only way to be right.
func probeMachine(ctx context.Context, machine Machine) (remoteStatus, error) {
	command := "herdr"
	if session := machine.SessionName(); session != "" {
		command += " --session " + shellQuote(session)
	}
	command += " status server --json"

	probeCtx, cancel := context.WithTimeout(ctx, sshProbeTimeout)
	defer cancel()
	// ClearAllForwardings drops any LocalForward the user's config attaches to
	// this host: the probe runs one command and needs no forwarding, and a
	// conflicting forward would fail the whole invocation.
	cmd := sshCommand(probeCtx,
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		machine.Target, remoteHerdrShell(command))
	output, err := cmd.Output()
	if err != nil {
		exitErr, isExit := err.(*exec.ExitError)
		stderr := ""
		if isExit {
			stderr = firstLine(strings.TrimSpace(string(exitErr.Stderr)))
		}
		// Budget exhaustion must be checked before the exit-code branches: a
		// context kill surfaces as an ExitError whose ExitCode is -1, which is
		// neither "not an ExitError" nor 255, and would otherwise be reported as
		// "herdr not installed" on a host we never actually reached.
		if probeCtx.Err() != nil {
			return remoteStatus{}, fmt.Errorf("timed out probing %s after %s", machine.Target, sshProbeTimeout)
		}
		if !isExit || exitErr.ExitCode() == 255 {
			if stderr == "" {
				stderr = err.Error()
			}
			return remoteStatus{}, fmt.Errorf("ssh %s: %s", machine.Target, stderr)
		}
		if len(output) == 0 {
			if stderr == "" {
				stderr = "herdr is not installed or not on PATH"
			}
			return remoteStatus{}, fmt.Errorf("%s: %s", machine.Target, stderr)
		}
		// Non-zero exit but JSON on stdout (server stopped): fall through.
	}
	var status remoteStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return remoteStatus{}, fmt.Errorf("%s did not report Herdr status as JSON; is its Herdr older than 0.9?", machine.Target)
	}
	if !status.Running || status.Socket == "" {
		return remoteStatus{}, fmt.Errorf("Herdr is not running on %s; start it there first (forwarding never starts a remote server)", machine.Target)
	}
	return status, nil
}

// DialRemote opens the SSH control master and forwarded socket for one machine
// and verifies the far end speaks wantProtocol. parent bounds the connection's
// lifetime: cancelling it tears down the master and removes both sockets.
func DialRemote(parent context.Context, machine Machine, wantProtocol int, runtimeDir string) (*Remote, error) {
	status, err := probeMachine(parent, machine)
	if err != nil {
		return nil, err
	}
	if wantProtocol > 0 && status.Protocol != wantProtocol {
		return nil, fmt.Errorf("%s runs Herdr %s speaking protocol %d, but this bridge registered tools from protocol %d; update Herdr on both machines",
			machine.Label, status.Version, status.Protocol, wantProtocol)
	}

	tag := socketTag(machine)
	remote := &Remote{
		Machine:   machine,
		Version:   status.Version,
		Protocol:  status.Protocol,
		ctlPath:   filepath.Join(runtimeDir, "c-"+tag+".sock"),
		localSock: filepath.Join(runtimeDir, "h-"+tag+".sock"),
	}
	remote.Touch()
	// Clear sockets a crashed prior run may have left, so ssh can bind.
	_ = os.Remove(remote.ctlPath)
	_ = os.Remove(remote.localSock)

	dialCtx, cancelDial := context.WithTimeout(parent, sshConnectTimeout)
	defer cancelDial()
	// -fNT backgrounds the master after authentication, so this returns once the
	// forward is up. ControlPersist=yes ties the master's life to this Remote --
	// Close kills it explicitly. A timed persist would let the master exit during
	// any quiet minute and take the forwarded socket with it. ExitOnForwardFailure=no
	// so a conflicting forward from the user's config cannot abort the master;
	// our own forward is verified by the readiness ping below instead.
	output, err := sshCommand(dialCtx,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ExitOnForwardFailure=no",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+remote.ctlPath,
		"-o", "ControlPersist=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-fNT",
		"-L", remote.localSock+":"+status.Socket,
		machine.Target,
	).CombinedOutput()
	if err != nil {
		remote.killMaster()
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("ssh %s: %s", machine.Target, firstLine(detail))
	}

	remote.Client = &Client{SocketPath: remote.localSock}
	if err := remote.waitForSocket(parent, wantProtocol); err != nil {
		remote.killMaster()
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	remote.cancel = cancel
	remote.done = make(chan struct{})
	remote.Queue = NewQueue(ctx, remote.Client, wantProtocol)
	go func() {
		defer close(remote.done)
		<-ctx.Done()
		remote.tearDown()
	}()
	return remote, nil
}

// waitForSocket polls the forwarded socket until it answers ping. The forward
// is established before the far end has necessarily accepted a connection on
// it, so a first call without this races and fails on a healthy machine.
func (r *Remote) waitForSocket(ctx context.Context, wantProtocol int) error {
	deadline := time.Now().Add(sshForwardReady)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		version, protocol, err := r.Client.Ping(pingCtx)
		cancel()
		if err == nil {
			if wantProtocol > 0 && protocol != wantProtocol {
				return fmt.Errorf("%s answered with protocol %d, not the %d this bridge registered tools from", r.Machine.Label, protocol, wantProtocol)
			}
			if version != "" {
				r.Version = version
			}
			r.Protocol = protocol
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return fmt.Errorf("forwarded Herdr socket for %s never answered: %v", r.Machine.Label, lastErr)
}

// Call sends one method to this machine through its own admission-control queue.
func (r *Remote) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	r.Touch()
	defer r.Touch()
	return r.Queue.Call(ctx, method, params)
}

// Touch records that this connection is in use, deferring the idle reaper.
func (r *Remote) Touch() { r.lastUsed.Store(time.Now().UnixNano()) }

// IdleFor reports how long this connection has gone untouched.
func (r *Remote) IdleFor() time.Duration {
	return time.Since(time.Unix(0, r.lastUsed.Load()))
}

// Close tears down the control master and removes the sockets, and does not
// return until that has happened.
//
// Waiting is the whole point. The master is backgrounded with -fNT, so it is
// nobody's child and nothing reaps it: a Close that only signalled and returned
// let the process exit first and left a live SSH connection and two sockets
// behind on every shutdown. Safe to call more than once.
func (r *Remote) Close() {
	r.closed.Do(func() {
		if r.cancel == nil {
			r.tearDown()
			return
		}
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(sshCloseTimeout):
			// The watcher is wedged; tear down here rather than leak. tearDown
			// runs once, so whichever gets there first wins.
			r.tearDown()
		}
	})
}

func (r *Remote) tearDown() {
	r.teardown.Do(func() {
		r.killMaster()
		_ = os.Remove(r.localSock)
		_ = os.Remove(r.ctlPath)
	})
}

// killMaster asks the control master to exit through its own control socket,
// which also releases the forward. A master started with -fNT has no process
// this bridge parents, so `ssh -O exit` is the only handle on it.
func (r *Remote) killMaster() {
	if r.ctlPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = sshCommand(ctx,
		"-o", "BatchMode=yes",
		"-o", "ControlPath="+r.ctlPath,
		"-O", "exit", r.Machine.Target,
	).Run()
}

// socketTag names this machine's sockets. The label alone is not enough: two
// labels that differ only in punctuation or case sanitize to the same string,
// and two Remotes sharing a socket path would silently drive one host through
// the other's tunnel. The profile ID prefix makes the name unique.
func socketTag(machine Machine) string {
	id := machine.ID
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		return sanitizeTag(machine.Label)
	}
	return sanitizeTag(machine.Label) + "-" + id
}

// sanitizeTag keeps socket filenames short and predictable. Unix socket paths
// are capped near 108 bytes, and the runtime directory already eats some of it.
func sanitizeTag(label string) string {
	var builder strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + 32)
		default:
			builder.WriteByte('-')
		}
		if builder.Len() >= 12 {
			break
		}
	}
	if builder.Len() == 0 {
		return "machine"
	}
	return builder.String()
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return text
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
