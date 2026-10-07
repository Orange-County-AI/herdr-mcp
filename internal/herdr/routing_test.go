package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func routingHerdr(t *testing.T, machines, sessions string) string {
	t.Helper()
	dir := t.TempDir()
	for name, payload := range map[string]string{"machines": machines, "sessions": sessions} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\ncase \"$1\" in\nmachine) cat " + shellQuote(filepath.Join(dir, "machines")) + ";;\nsession) cat " + shellQuote(filepath.Join(dir, "sessions")) + ";;\n*) exit 2;;\nesac\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func sessionJSON(name, socket string, running bool) string {
	encoded, _ := json.Marshal(map[string]any{"sessions": []Session{{Name: name, Running: running, SocketPath: socket}}})
	return string(encoded)
}

func TestPoolDiscoversSessionsAndMachinesAddedAfterStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	binary := routingHerdr(t, `[]`, `{"sessions":[]}`)
	pool := NewPool(ctx, binary, 22, t.TempDir())
	defer pool.Close()
	roster, err := pool.Roster(ctx)
	if err != nil || len(roster.LocalSessions) != 0 || len(roster.Machines) != 0 {
		t.Fatalf("initial roster = %v, err = %v", roster, err)
	}
	path := filepath.Join(t.TempDir(), "herdr.sock")
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "ping" {
			return `{"version":"test","protocol":22}`
		}
		return `{"session":"new","agents":[]}`
	})
	defer stop()
	if err := os.WriteFile(filepath.Join(filepath.Dir(binary), "sessions"), []byte(sessionJSON("new", path, true)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(binary), "machines"), []byte(`[{"id":"later","label":"later","enabled":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	roster, err = pool.Roster(ctx)
	if err != nil || len(roster.LocalSessions) != 1 || len(roster.Machines) != 1 || roster.Machines[0].Selector != "ssh:later" {
		t.Fatalf("updated roster = %v, err = %v", roster, err)
	}
	var previous Transport
	pool.Tune = func(q *Queue) { q.Concurrency = 1; q.LongConcurrency = 2; q.Backlog = 3; q.OutageGrace = time.Second }
	for _, selector := range []string{"new", "local:new"} {
		caller, err := pool.Caller(ctx, selector)
		if err != nil {
			t.Fatal(err)
		}
		queue := caller.(*Queue)
		if queue.Concurrency != 1 || queue.LongConcurrency != 2 || queue.Backlog != 3 || queue.OutageGrace != time.Second {
			t.Fatalf("queue tuning was lost: %+v", queue.Availability())
		}
		if previous != nil && previous != caller {
			t.Fatal("aliases use different admission queues")
		}
		previous = caller
		result, err := caller.Call(ctx, "agent.list", nil)
		if err != nil || !strings.Contains(string(result), `"session":"new"`) {
			t.Fatalf("result = %s, err = %v", result, err)
		}
	}
}

func TestPoolRefusesCollisionStoppedMissingAndUnknownSessions(t *testing.T) {
	for _, test := range []struct {
		selector, name, socket string
		running                bool
		want                   string
	}{
		{"work", "work", "/missing.sock", true, "both a local session"},
		{"id", "id", "/missing.sock", true, "both a local session"},
		{"local:work", "work", "/missing.sock", false, "stopped"},
		{"local:work", "work", "/missing.sock", true, "missing"},
		{"local:unknown", "work", "/missing.sock", true, "no Herdr session"},
		{"ssh:unknown/work", "work", "/missing.sock", true, "no saved Herdr machine"},
		{"local:", "work", "/missing.sock", true, "invalid session selector"},
	} {
		t.Run(test.selector+test.want, func(t *testing.T) {
			binary := routingHerdr(t, `[{"id":"id","label":"work","target":"unused","enabled":true}]`, sessionJSON(test.name, test.socket, test.running))
			pool := NewPool(context.Background(), binary, 22, t.TempDir())
			defer pool.Close()
			caller, err := pool.Caller(context.Background(), test.selector)
			if caller != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("caller = %v, err = %v; want %s", caller, err, test.want)
			}
		})
	}
}

func TestRoutedQueueRefusesProtocolChangeBeforeDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var protocol atomic.Int64
	protocol.Store(22)
	var delivered atomic.Int64
	path := filepath.Join(t.TempDir(), "herdr.sock")
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "ping" {
			return fmt.Sprintf(`{"version":"test","protocol":%d}`, protocol.Load())
		}
		delivered.Add(1)
		return `{"ok":true}`
	})
	defer stop()
	binary := routingHerdr(t, `[]`, sessionJSON("work", path, true))
	pool := NewPool(ctx, binary, 22, t.TempDir())
	defer pool.Close()
	caller, err := pool.Caller(ctx, "local:work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caller.Call(ctx, "pane.list", nil); err != nil {
		t.Fatal(err)
	}
	protocol.Store(23)
	if _, err := caller.Call(ctx, "pane.close", nil); err == nil || !strings.Contains(err.Error(), "protocol mismatch") {
		t.Fatalf("mismatched call err = %v", err)
	}
	if delivered.Load() != 1 {
		t.Fatal("a mutation reached the mismatched session")
	}
	protocol.Store(22)
	if _, err := caller.Call(ctx, "pane.list", nil); err != nil {
		t.Fatalf("repaired session did not recover without a bridge restart: %v", err)
	}
	protocol.Store(23)
	pool.SetProtocol(23)
	caller, err = pool.Caller(ctx, "local:work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caller.Call(ctx, "pane.list", nil); err != nil {
		t.Fatalf("reloaded schema did not clear mismatch: %v", err)
	}
}

func TestRemoteSessionRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	paths := map[string]string{}
	for _, name := range []string{"work", "other"} {
		path := filepath.Join(dir, name+".sock")
		paths[name] = path
		stop := serveSocket(t, path, nil, func(request map[string]any) string {
			if request["method"] == "ping" {
				return `{"version":"test","protocol":22}`
			}
			return fmt.Sprintf(`{"session":%q}`, name)
		})
		defer stop()
	}
	remoteSessions, _ := json.Marshal(map[string]any{"sessions": []Session{
		{Name: "work", SocketPath: paths["work"], Running: true},
		{Name: "other", SocketPath: paths["other"], Running: true},
		{Name: "stopped", SocketPath: "/unused", Running: false},
	}})
	sshDir := t.TempDir()
	remoteListPath := filepath.Join(sshDir, "sessions.json")
	initialSessions, _ := json.Marshal(map[string]any{"sessions": []Session{
		{Name: "work", SocketPath: paths["work"], Running: true},
		{Name: "stopped", SocketPath: "/unused", Running: false},
	}})
	if err := os.WriteFile(remoteListPath, initialSessions, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
previous=''
for arg in "$@"; do
  if [ "$previous" = '-L' ]; then ln -s "${arg#*:}" "${arg%%:*}"; exit; fi
  previous="$arg"
done
case "$*" in
  *'session list --json'*) cat ` + shellQuote(remoteListPath) + `;;
  *'status server --json'*)
    case "$*" in *other*) socket=` + shellQuote(paths["other"]) + `;; *) socket=` + shellQuote(paths["work"]) + `;; esac
    printf '{"running":true,"version":"test","protocol":22,"socket":"%s"}\n' "$socket";;
  *'-O exit'*) exit 0;;
  *) exit 255;;
esac
`
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := routingHerdr(t, `[{"id":"id","label":"host","target":"host","session":"work","enabled":true}]`, `{"sessions":[]}`)
	pool := NewPool(ctx, binary, 22, t.TempDir())
	defer pool.Close()
	roster, err := pool.Roster(ctx)
	if err != nil || len(roster.Machines) != 1 || len(roster.Machines[0].Sessions) != 2 || roster.Machines[0].SessionsError != "" {
		t.Fatalf("roster = %+v, err = %v", roster, err)
	}
	if err := os.WriteFile(remoteListPath, remoteSessions, 0o600); err != nil {
		t.Fatal(err)
	}
	roster, err = pool.Roster(ctx)
	if err != nil || len(roster.Machines[0].Sessions) != 3 {
		t.Fatalf("new remote session not discovered live: %+v, %v", roster, err)
	}
	if _, err := pool.Caller(ctx, "ssh:id/other"); err != nil {
		t.Fatal(err)
	}
	status := pool.Status(ctx)
	if len(status) != 1 || !status[0].Connected || len(status[0].Connections) != 1 || status[0].Connections[0].Session != "other" {
		t.Fatalf("health omitted the non-configured session connection: %+v", status)
	}
	roster, err = pool.Roster(ctx)
	if err != nil || !roster.Machines[0].Connected || len(roster.Machines[0].Connections) != 1 {
		t.Fatalf("roster omitted the non-configured session connection: %+v, %v", roster, err)
	}
	for selector, name := range map[string]string{"host": "work", "id": "work", "ssh:id": "work", "ssh:id/work": "work", "ssh:id/other": "other"} {
		caller, err := pool.Caller(ctx, selector)
		if err != nil {
			t.Fatalf("%s: %v", selector, err)
		}
		result, err := caller.Call(ctx, "agent.list", nil)
		if err != nil || string(result) != fmt.Sprintf(`{"session":%q}`, name) {
			t.Fatalf("%s: result = %s, err = %v", selector, result, err)
		}
	}
	if len(pool.remotes) != 2 {
		t.Fatalf("connections = %d, want one per session", len(pool.remotes))
	}
	if _, err := pool.Caller(ctx, "ssh:id/stopped"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped remote err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(binary), "machines"), []byte(`[{"id":"id","label":"host","target":"host","enabled":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Caller(ctx, "ssh:id/other"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled profile reused a cached connection: %v", err)
	}
	if !pool.Disconnect("id") || len(pool.remotes) != 0 {
		t.Fatal("Disconnect did not drop every session for the profile")
	}
}

func TestRosterKeepsLocalSessionsWhenRemoteDiscoveryFails(t *testing.T) {
	sshDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := routingHerdr(t, `[{"id":"id","label":"sleeping","target":"sleeping","enabled":true}]`, sessionJSON("local", "/unused", false))
	pool := NewPool(context.Background(), binary, 22, t.TempDir())
	roster, err := pool.Roster(context.Background())
	if err != nil || len(roster.LocalSessions) != 1 || len(roster.Machines) != 1 || roster.Machines[0].SessionsError == "" {
		t.Fatalf("roster = %+v, err = %v", roster, err)
	}
}

func TestSocketTagSeparatesSessionsAndEditedTargets(t *testing.T) {
	first := Machine{ID: "same", Label: "host", Target: "host", Session: "work"}
	second := first
	second.Session = "other"
	third := first
	third.Target = "different"
	if socketTag(first) == socketTag(second) || socketTag(first) == socketTag(third) {
		t.Fatal("different destinations share a forwarded socket")
	}
}

func TestRosterReportsCachedDiscoveryWithoutRoutingThroughIt(t *testing.T) {
	binary := routingHerdr(t, `[{"id":"id","label":"host","target":"host","enabled":false}]`, sessionJSON("work", "/unused", true))
	pool := NewPool(context.Background(), binary, 22, t.TempDir())
	if _, err := pool.Roster(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	roster, err := pool.Roster(context.Background())
	if err != nil || len(roster.LocalSessions) != 1 || len(roster.Machines) != 1 || roster.LocalSessionsError == "" || roster.MachinesError == "" {
		t.Fatalf("cached roster lost its data or error markers: %+v, %v", roster, err)
	}
	for _, selector := range []string{"work", "local:work", "ssh:id"} {
		if caller, err := pool.Caller(context.Background(), selector); err == nil || caller != nil {
			t.Fatalf("selector %q routed through a stale discovery cache", selector)
		}
	}
	// A failed machine-list refresh must not hide a fresh local session list.
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = machine ]; then exit 1; fi\nprintf '%s\\n' "+shellQuote(sessionJSON("new", "/unused", false))+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	roster, err = pool.Roster(context.Background())
	if err != nil || len(roster.LocalSessions) != 1 || roster.LocalSessions[0].Name != "new" || roster.LocalSessionsError != "" || roster.MachinesError == "" {
		t.Fatalf("partial refresh discarded local discovery: %+v, %v", roster, err)
	}
}

func TestStartupAliasSharesAdmissionQueue(t *testing.T) {
	t.Run("direct", func(t *testing.T) { testStartupAdmission(t, false) })
	t.Run("symlink", func(t *testing.T) { testStartupAdmission(t, true) })
}

func testStartupAdmission(t *testing.T, symlink bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var delivered atomic.Int64
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "ping" {
			return `{"version":"test","protocol":22}`
		}
		if delivered.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return `{"ok":true}`
	})
	defer stop()
	startupPath := path
	if symlink {
		startupPath = filepath.Join(filepath.Dir(path), "startup.sock")
		if err := os.Symlink(path, startupPath); err != nil {
			t.Fatal(err)
		}
	}
	queue := NewQueue(ctx, &Client{SocketPath: startupPath}, 22)
	queue.Concurrency, queue.Backlog = 1, 1
	binary := routingHerdr(t, `[]`, sessionJSON("work", path, true))
	pool := NewPool(ctx, binary, 22, t.TempDir())
	pool.StartupQueue = queue
	defer pool.Close()
	alias, err := pool.Caller(ctx, "local:work")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := queue.Call(ctx, "pane.read", nil); finished <- err }()
	<-entered
	callCtx, cancelCall := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelCall()
	_, err = alias.Call(callCtx, "pane.read", nil)
	close(release)
	if err == nil || delivered.Load() != 1 {
		t.Fatalf("routed alias bypassed startup admission: deliveries = %d, err = %v", delivered.Load(), err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if queue.VerifyProtocol {
		t.Fatal("routed alias changed verification policy for omitted selectors")
	}
}

func TestHealthDiscoveryCacheDoesNotHideRosterUpdates(t *testing.T) {
	binary := routingHerdr(t, `[{"id":"first","label":"first","enabled":false}]`, `{"sessions":[]}`)
	pool := NewPool(context.Background(), binary, 22, t.TempDir())
	if _, err := pool.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(binary), "machines"), []byte(`[{"id":"new","label":"new","enabled":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	health, err := pool.Machines(context.Background())
	if err != nil || len(health) != 1 || health[0].ID != "first" {
		t.Fatalf("health unexpectedly bypassed its cache: %+v, %v", health, err)
	}
	roster, err := pool.Roster(context.Background())
	if err != nil || len(roster.Machines) != 1 || roster.Machines[0].ID != "new" {
		t.Fatalf("health cache hid a new machine from the roster: %+v, %v", roster, err)
	}
	if _, err := pool.Caller(context.Background(), "ssh:new"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("routing did not resolve the freshly saved machine: %v", err)
	}
}

func TestRoutedQueueRejectsMismatchedRevival(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	queue := NewQueue(ctx, &Client{SocketPath: path}, 22)
	queue.VerifyProtocol = true
	queue.OutageGrace = 3 * time.Second
	queue.Logf = func(string, ...any) {}
	finished := make(chan error, 1)
	go func() { _, err := queue.Call(ctx, "pane.close", nil); finished <- err }()
	time.Sleep(150 * time.Millisecond)
	var delivered atomic.Int64
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		if request["method"] == "ping" {
			return `{"version":"test","protocol":23}`
		}
		delivered.Add(1)
		return `{"ok":true}`
	})
	defer stop()
	if err := <-finished; err == nil || !strings.Contains(err.Error(), "protocol mismatch") || delivered.Load() != 0 {
		t.Fatalf("mismatched revival delivered mutation: deliveries = %d, err = %v", delivered.Load(), err)
	}
}
