package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Session is one entry from `herdr session list --json`. Socket paths come
// from Herdr rather than assuming a config directory or a default session.
type Session struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
	Selector   string `json:"selector"`
}

const discoveryTimeout = 5 * time.Second

func localSelector(name string) string { return "local:" + url.PathEscape(name) }

func remoteSelector(id, session string) string {
	return "ssh:" + url.PathEscape(id) + "/" + url.PathEscape(session)
}

// parseSelector keeps the two namespaces separate even when a name contains
// punctuation used by the selector grammar. Bare selectors are handled later.
func parseSelector(selector string) (local bool, machine, session string, err error) {
	var encoded string
	switch {
	case strings.HasPrefix(selector, "local:"):
		local = true
		encoded = strings.TrimPrefix(selector, "local:")
	case strings.HasPrefix(selector, "ssh:"):
		parts := strings.SplitN(strings.TrimPrefix(selector, "ssh:"), "/", 2)
		machine, err = url.PathUnescape(parts[0])
		if err != nil || machine == "" {
			return false, "", "", fmt.Errorf("invalid SSH selector %q; use ssh:<profile-id>/<session>", selector)
		}
		if len(parts) == 1 {
			return false, machine, "", nil
		}
		encoded = parts[1]
	default:
		return false, "", "", nil
	}
	session, err = url.PathUnescape(encoded)
	if err != nil || session == "" || strings.Contains(encoded, "/") {
		return false, "", "", fmt.Errorf("invalid session selector %q; use a selector from machine_list", selector)
	}
	return local, machine, session, nil
}

func decodeSessions(output []byte) ([]Session, error) {
	var roster struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(output, &roster); err != nil {
		return nil, fmt.Errorf("decode session list: %w", err)
	}
	if roster.Sessions == nil {
		return nil, fmt.Errorf("session list has no sessions array; update Herdr to a release supporting session list --json")
	}
	seen := map[string]bool{}
	for _, session := range roster.Sessions {
		if session.Name == "" || seen[session.Name] {
			return nil, fmt.Errorf("session list has an empty or duplicate session name %q", session.Name)
		}
		seen[session.Name] = true
	}
	sort.Slice(roster.Sessions, func(i, j int) bool { return roster.Sessions[i].Name < roster.Sessions[j].Name })
	return roster.Sessions, nil
}

// ListSessions reads current local sessions, including stopped sessions so a
// caller can distinguish a stopped server from an unknown selector.
func ListSessions(ctx context.Context, binary string) ([]Session, error) {
	if binary == "" {
		binary = "herdr"
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "session", "list", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("%s session list --json: %w", binary, err)
	}
	sessions, err := decodeSessions(output)
	for index := range sessions {
		sessions[index].Selector = localSelector(sessions[index].Name)
	}
	return sessions, err
}

// listRemoteSessions only runs a read-only CLI command. It never starts Herdr
// or opens a forwarded socket, and a sleeping machine has a bounded cost.
func listRemoteSessions(ctx context.Context, machine Machine) ([]Session, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	output, err := sshCommand(ctx,
		"-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=3", "-o", "StrictHostKeyChecking=accept-new",
		machine.Target, remoteHerdrShell("herdr session list --json")).Output()
	if err != nil {
		return nil, fmt.Errorf("list sessions on %s: %w", machine.Target, err)
	}
	sessions, err := decodeSessions(output)
	for index := range sessions {
		sessions[index].Selector = remoteSelector(machine.ID, sessions[index].Name)
	}
	return sessions, err
}

func selectSession(sessions []Session, name string) (Session, error) {
	for _, session := range sessions {
		if session.Name == name {
			if !session.Running {
				return Session{}, fmt.Errorf("session %q is stopped; start it in Herdr first", name)
			}
			if session.SocketPath == "" {
				return Session{}, fmt.Errorf("session %q has no socket path", name)
			}
			return session, nil
		}
	}
	return Session{}, fmt.Errorf("no Herdr session matches %q; use machine_list to see current sessions", name)
}
