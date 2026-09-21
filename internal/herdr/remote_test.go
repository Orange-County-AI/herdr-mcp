package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A host that cannot be reached must say so, not be mistaken for one that
// answered without Herdr installed. The two used to look alike because ssh's
// connect failure and a remote "command not found" are both non-zero exits.
func TestProbeMachineReportsAnUnreachableHost(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the ssh binary")
	}
	// .invalid is reserved by RFC 2606 and never resolves, so this is a
	// connection failure with no network dependency and no real host touched.
	machine := Machine{ID: "deadbeef", Label: "nowhere", Target: "herdr-mcp-test.invalid", Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := probeMachine(ctx, machine)
	if err == nil {
		t.Fatal("probing an unresolvable host succeeded")
	}
	if !strings.Contains(err.Error(), "herdr-mcp-test.invalid") {
		t.Fatalf("error does not name the host: %v", err)
	}
	if strings.Contains(err.Error(), "not installed") {
		t.Fatalf("an unreachable host was reported as missing Herdr: %v", err)
	}
}

func TestDialRemoteFailsBeforeLeavingSockets(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the ssh binary")
	}
	dir := t.TempDir()
	machine := Machine{ID: "deadbeef", Label: "nowhere", Target: "herdr-mcp-test.invalid", Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := DialRemote(ctx, machine, 22, dir); err == nil {
		t.Fatal("dialling an unresolvable host succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("a failed dial left sockets behind: %v", names)
	}
}

func TestDefaultRuntimeDirIsShortEnoughForAUnixSocket(t *testing.T) {
	dir, err := DefaultRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	// A unix socket path is capped near 108 bytes in the kernel, and a socket
	// name adds roughly 25 of them.
	longest := filepath.Join(dir, "h-"+strings.Repeat("x", 12)+"-12345678.sock")
	if len(longest) > 100 {
		t.Fatalf("runtime dir %s leaves no room for a socket name (%d bytes)", dir, len(longest))
	}
}

func TestPoolRejectsAnUnknownMachineWithoutDialling(t *testing.T) {
	binary := fakeHerdr(t, `[{"id":"0fb972d6","label":"minime","target":"minime","session":"default","enabled":true}]`)
	pool := NewPool(context.Background(), binary, 22, t.TempDir())
	_, err := pool.Caller(context.Background(), "nowhere")
	if err == nil || !strings.Contains(err.Error(), "minime") {
		t.Fatalf("err = %v; want the known labels listed", err)
	}
}

func TestPoolStatusListsSavedMachinesWithoutConnecting(t *testing.T) {
	binary := fakeHerdr(t, `[
	  {"id":"0fb972d6","label":"minime","target":"minime","session":"default","enabled":true},
	  {"id":"bbb22233","label":"retired","target":"retired","session":"default","enabled":false}
	]`)
	pool := NewPool(context.Background(), binary, 22, t.TempDir())
	statuses := pool.Status(context.Background())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	for _, status := range statuses {
		if status.Connected {
			t.Fatalf("Status dialled %s; reporting must never cost a handshake", status.Label)
		}
	}
	if statuses[0].Label != "minime" || statuses[1].Label != "retired" {
		t.Fatalf("statuses are not label-sorted: %v", statuses)
	}
	if statuses[1].Enabled {
		t.Fatal("a disabled profile is reported as enabled")
	}
}

// A machine list that cannot be refreshed must fall back to the last one: the
// Herdr binary is missing during exactly the upgrades this bridge survives.
func TestPoolKeepsTheLastMachineListWhenTheBinaryFails(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "herdr")
	good := "#!/bin/sh\ncat <<'JSON'\n[{\"id\":\"0fb972d6\",\"label\":\"minime\",\"target\":\"minime\",\"session\":\"default\",\"enabled\":true}]\nJSON\n"
	if err := os.WriteFile(binary, []byte(good), 0o700); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(context.Background(), binary, 22, dir)
	if _, err := pool.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pool.listedAt = time.Time{} // force a refresh past the cache TTL
	machines, err := pool.Machines(context.Background())
	if err != nil {
		t.Fatalf("a failed refresh discarded the cached list: %v", err)
	}
	if len(machines) != 1 || machines[0].Label != "minime" {
		t.Fatalf("machines = %v", machines)
	}
}
