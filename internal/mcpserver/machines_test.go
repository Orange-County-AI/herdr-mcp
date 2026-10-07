package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stubRouter struct {
	asked  string
	caller herdr.Transport
	err    error
}

func (s *stubRouter) Caller(_ context.Context, selector string) (herdr.Transport, error) {
	s.asked = selector
	if s.err != nil {
		return nil, s.err
	}
	return s.caller, nil
}

func (s *stubRouter) Status(context.Context) []herdr.RemoteStatus {
	return []herdr.RemoteStatus{{Label: "minime", Enabled: true}}
}

func (s *stubRouter) Roster(ctx context.Context) (herdr.Roster, error) {
	return herdr.Roster{Machines: s.Status(ctx)}, nil
}

type recordingTransport struct{ methods []string }

func (r *recordingTransport) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	r.methods = append(r.methods, method+" "+string(params))
	return json.RawMessage(`{"type":"pane_read"}`), nil
}

func TestWithMachineArgumentDoesNotMutateTheSource(t *testing.T) {
	original := map[string]any{
		"type":       "object",
		"properties": map[string]any{"pane_id": map[string]any{"type": "string"}},
		"required":   []any{"pane_id"},
	}
	injected := withMachineArgument(original)
	if _, leaked := original["properties"].(map[string]any)[machineArgument]; leaked {
		t.Fatal("withMachineArgument mutated the schema it was given")
	}
	properties := injected["properties"].(map[string]any)
	if _, ok := properties[machineArgument]; !ok {
		t.Fatal("injected schema does not accept machine")
	}
	if _, ok := properties["pane_id"]; !ok {
		t.Fatal("injected schema dropped the method's own properties")
	}
}

// The socket rejects unknown params, so "machine" must never reach Herdr.
func TestSplitMachineStripsTheRoutingArgument(t *testing.T) {
	selector, rest, err := splitMachine(json.RawMessage(`{"pane_id":"w1:p1","machine":" minime "}`))
	if err != nil {
		t.Fatal(err)
	}
	if selector != "minime" {
		t.Fatalf("selector = %q", selector)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rest, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded[machineArgument]; present {
		t.Fatalf("machine survived into the socket params: %s", rest)
	}
	if decoded["pane_id"] != "w1:p1" {
		t.Fatalf("params = %s", rest)
	}
}

func TestSplitMachineLeavesLocalCallsUntouched(t *testing.T) {
	params := json.RawMessage(`{"pane_id":"w1:p1"}`)
	selector, rest, err := splitMachine(params)
	if err != nil || selector != "" {
		t.Fatalf("selector = %q, err = %v", selector, err)
	}
	if string(rest) != string(params) {
		t.Fatalf("rest = %s, want it unchanged", rest)
	}
}

func TestSplitMachineRejectsNonStringSelector(t *testing.T) {
	if _, _, err := splitMachine(json.RawMessage(`{"machine":7}`)); err == nil {
		t.Fatal("a numeric machine was accepted")
	}
}

func TestSplitMachineRejectsExplicitEmptySelector(t *testing.T) {
	for _, arguments := range []string{`{"machine":""}`, `{"machine":"  "}`} {
		if _, _, err := splitMachine(json.RawMessage(arguments)); err == nil {
			t.Fatalf("explicit empty selector fell back to the startup socket: %s", arguments)
		}
	}
}

func TestRoutedToolsKeepPolicyAndStripSelectors(t *testing.T) {
	schema, err := herdr.ParseSchema([]byte(bridgeSchemaWithLinkResolve))
	if err != nil {
		t.Fatal(err)
	}
	local, routed := &recordingTransport{}, &recordingTransport{}
	router := &stubRouter{caller: routed}
	server, err := New(Options{Schema: schema, Client: local, Machines: router, Version: "test", Deny: []string{"pane.link.resolve"}})
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "pane_link_resolve" {
			t.Fatal("routing exposed a denied method")
		}
	}
	for _, selector := range []string{"local:work", "ssh:profile/work"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "pane_read", Arguments: map[string]any{"pane_id": "p1", "machine": selector}})
		if err != nil || result.IsError || router.asked != selector {
			t.Fatalf("selector = %s, result = %+v, err = %v", selector, result, err)
		}
		if got := routed.methods[len(routed.methods)-1]; got != `pane.read {"pane_id":"p1"}` {
			t.Fatalf("routed params = %s", got)
		}
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "pane_link_resolve", Arguments: map[string]any{"pane_id": "p1", "machine": selector}}); err == nil {
			t.Fatal("a denied method was callable on a routed session")
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "pane_read", Arguments: map[string]any{"pane_id": "p1"}})
	if err != nil || result.IsError || len(local.methods) != 1 {
		t.Fatalf("omitted selector did not use startup transport: %+v, %v", result, err)
	}
	router.err = errors.New("session stopped")
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "pane_read", Arguments: map[string]any{"pane_id": "p1", "machine": "local:work"}})
	if err != nil || !result.IsError || len(local.methods) != 1 || len(routed.methods) != 2 {
		t.Fatalf("routing failure fell back or delivered a method: %+v, %v", result, err)
	}
	roster, err := session.CallTool(ctx, &mcp.CallToolParams{Name: machineListTool, Arguments: map[string]any{}})
	if err != nil || roster.IsError || !strings.Contains(resultText(roster), "local_sessions") {
		t.Fatalf("roster = %+v, err = %v", roster, err)
	}
}

func TestCallerForRoutesAndFallsBackToLocal(t *testing.T) {
	local := &recordingTransport{}
	remote := &recordingTransport{}
	router := &stubRouter{caller: remote}
	server := &Server{Client: local, Machines: router}

	caller, err := server.callerFor(context.Background(), "pane.read", "")
	if err != nil || caller != herdr.Transport(local) {
		t.Fatalf("empty selector did not stay local: %v", err)
	}
	caller, err = server.callerFor(context.Background(), "pane.read", "minime")
	if err != nil || caller != herdr.Transport(remote) {
		t.Fatalf("selector was not routed: %v", err)
	}
	if router.asked != "minime" {
		t.Fatalf("router asked for %q", router.asked)
	}
}

// A client-scoped method has no meaning on the far end of a forwarded socket:
// there is no attached client there to retitle. Refusing beats doing nothing.
func TestCallerForRefusesClientLocalMethods(t *testing.T) {
	server := &Server{Client: &recordingTransport{}, Machines: &stubRouter{caller: &recordingTransport{}}}
	_, err := server.callerFor(context.Background(), "client.window_title.set", "minime")
	if err == nil || !strings.Contains(err.Error(), "cannot be routed") {
		t.Fatalf("err = %v", err)
	}
}

func TestCallerForSurfacesRouterFailures(t *testing.T) {
	server := &Server{Client: &recordingTransport{}, Machines: &stubRouter{err: errors.New("herdr is not running on minime")}}
	_, err := server.callerFor(context.Background(), "pane.read", "minime")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "minime") {
		t.Fatalf("error does not name the machine: %v", err)
	}
}

func TestCallerForExplainsDisabledRouting(t *testing.T) {
	server := &Server{Client: &recordingTransport{}}
	_, err := server.callerFor(context.Background(), "pane.read", "minime")
	if err == nil || !strings.Contains(err.Error(), "machine routing") {
		t.Fatalf("err = %v", err)
	}
}
