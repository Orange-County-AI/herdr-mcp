package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestWaitForStartedAgentReturnsInteractiveAgent(t *testing.T) {
	socket := testSocket(t, func(method string) string {
		if method != "agent.get" {
			t.Fatalf("method = %q, want agent.get", method)
		}
		return `{"type":"agent_info","agent":{"name":"reviewer","pane_id":"w1:p2","interactive_ready":true}}`
	})
	server := &Server{Client: &herdr.Client{SocketPath: socket}}
	started := json.RawMessage(`{"type":"agent_started","agent":{"name":"reviewer","launch_pending":true}}`)
	result, note, err := server.waitForStartedAgent(context.Background(), started, json.RawMessage(`{"name":"reviewer"}`))
	if err != nil || note != "" {
		t.Fatalf("result = %s, note = %q, err = %v", result, note, err)
	}
	if !strings.Contains(string(result), `"interactive_ready":true`) {
		t.Fatalf("result = %s", result)
	}
}

func TestWaitThroughLaunchWaitsBeforeAgentWait(t *testing.T) {
	methods := make(chan string, 2)
	socket := testSocket(t, func(method string) string {
		methods <- method
		switch method {
		case "agent.list":
			return `{"type":"agent_list","agents":[{"name":"reviewer","pane_id":"w1:p2","launch_pending":true}]}`
		case "agent.get":
			return `{"type":"agent_info","agent":{"name":"reviewer","pane_id":"w1:p2","interactive_ready":true}}`
		default:
			t.Fatalf("unexpected method %q", method)
			return ""
		}
	})
	server := &Server{Client: &herdr.Client{SocketPath: socket}}
	if err := server.waitThroughLaunch(context.Background(), json.RawMessage(`{"target":"reviewer","timeout_ms":1000}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-methods; got != "agent.list" {
		t.Fatalf("first method = %q", got)
	}
	if got := <-methods; got != "agent.get" {
		t.Fatalf("second method = %q", got)
	}
}

func testSocket(t *testing.T, response func(string) string) string {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				line, _ := bufio.NewReader(connection).ReadBytes('\n')
				var request struct {
					Method string `json:"method"`
				}
				_ = json.Unmarshal(line, &request)
				_, _ = fmt.Fprintf(connection, `{"id":"test","result":%s}`+"\n", response(request.Method))
			}()
		}
	}()
	return listener.Addr().String()
}
