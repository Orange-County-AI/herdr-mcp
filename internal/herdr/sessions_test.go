package herdr

import (
	"context"
	"os/exec"
	"testing"
)

func TestListSessionsParsesLiveRoster(t *testing.T) {
	binary := fakeHerdr(t, `{"sessions":[{"name":"work","running":true,"socket_path":"/tmp/work.sock"},{"name":"default","default":true,"running":false,"socket_path":"/tmp/default.sock"}]}`)
	sessions, err := ListSessions(context.Background(), binary)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions = %v, err = %v", sessions, err)
	}
	if sessions[0].Name != "default" || sessions[1].SocketPath != "/tmp/work.sock" || sessions[1].Selector != "local:work" {
		t.Fatalf("sessions = %v", sessions)
	}
}

func TestSessionListRejectsInvalidShapeAndDuplicateNames(t *testing.T) {
	for _, payload := range []string{`[]`, `{}`, `{"sessions":[{"name":""}]}`, `{"sessions":[{"name":"work"},{"name":"work"}]}`} {
		if _, err := decodeSessions([]byte(payload)); err == nil {
			t.Fatalf("invalid roster accepted: %s", payload)
		}
	}
}

func TestRemoteShellPreservesQuotedSessionNames(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	name := "O'Brien $(printf injected); a/b"
	output, err := exec.Command("sh", "-c", remoteHerdrShell("printf '%s' "+shellQuote(name))).Output()
	if err != nil || string(output) != name {
		t.Fatalf("session name changed across login-shell quoting: %q, err = %v", output, err)
	}
}

func FuzzSessionSelectorsRoundTrip(f *testing.F) {
	for _, name := range []string{"work", "local:work", "a/b", "a%2Fb", "O'Brien", "日本語"} {
		f.Add(name, "profile/with:punctuation")
	}
	f.Fuzz(func(t *testing.T, name, id string) {
		if name == "" || id == "" {
			return
		}
		local, machine, session, err := parseSelector(localSelector(name))
		if err != nil || !local || machine != "" || session != name {
			t.Fatalf("local round trip: %q => %v %q %q %v", name, local, machine, session, err)
		}
		local, machine, session, err = parseSelector(remoteSelector(id, name))
		if err != nil || local || machine != id || session != name {
			t.Fatalf("remote round trip: %q/%q => %v %q %q %v", id, name, local, machine, session, err)
		}
	})
}
