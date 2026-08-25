package mcpserver

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// A client may render an errored tool result without its content, leaving the
// server log as the only record of what broke. So the cause has to be in it.
func TestFailedCallIsLoggedWithItsCause(t *testing.T) {
	log := &recorder{}
	server := &Server{Logf: log.logf}

	server.observe("pane.read", 12*time.Millisecond,
		errorResult("pane.read", fmt.Errorf("pane_not_found: pane p1 is gone")))

	line := log.joined()
	if line == "" {
		t.Fatal("a failed tool call produced no server-side log line")
	}
	for _, want := range []string{"pane.read", "failed", "pane_not_found: pane p1 is gone"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %s", want, line)
		}
	}
}

// A call slower than the threshold succeeded here but may have been abandoned
// upstream, so it is logged even though nothing errored.
func TestSlowSuccessIsLogged(t *testing.T) {
	log := &recorder{}
	server := &Server{Logf: log.logf, SlowCallThreshold: 50 * time.Millisecond}

	server.observe("agent.wait", 90*time.Millisecond,
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"type":"agent_info"}`}}})

	line := log.joined()
	if line == "" {
		t.Fatal("a call past the slow threshold produced no server-side log line")
	}
	for _, want := range []string{"agent.wait", "90ms", "abandoned"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %s", want, line)
		}
	}
}

// Logging every success would bury the two cases above under normal traffic.
func TestFastSuccessIsNotLogged(t *testing.T) {
	log := &recorder{}
	server := &Server{Logf: log.logf, SlowCallThreshold: time.Minute}

	server.observe("pane.list", 3*time.Millisecond,
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"type":"pane_list"}`}}})

	if line := log.joined(); line != "" {
		t.Fatalf("a fast successful call was logged: %s", line)
	}
}

// The threshold must come from the field when set, so the slow path is not
// reachable only by waiting a real minute.
func TestSlowThresholdIsConfigurable(t *testing.T) {
	log := &recorder{}
	server := &Server{Logf: log.logf, SlowCallThreshold: time.Hour}

	server.observe("agent.wait", 5*time.Minute,
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "{}"}}})

	if line := log.joined(); line != "" {
		t.Fatalf("call under the configured threshold was logged: %s", line)
	}
}
