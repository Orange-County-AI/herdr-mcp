package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// machineArgument is the parameter that routes a call to a saved SSH machine.
// It is injected into the tool schemas rather than being a separate set of
// tools: 93 methods times fifteen machines is not a tool surface any client can
// use, and the method a caller wants is the same one either way.
const machineArgument = "machine"

const machineArgumentDescription = "Saved Herdr SSH machine to run this on: its label or profile id, from machine_list. " +
	"Omit it for the local session. Workspace, tab, pane and agent IDs are scoped to one machine, " +
	"so an id discovered locally never addresses a remote pane; list on the machine you intend to drive."

// MachineRouter resolves a machine selector to the transport that reaches it.
type MachineRouter interface {
	Caller(ctx context.Context, selector string) (herdr.Transport, error)
	Status(ctx context.Context) []herdr.RemoteStatus
}

// clientLocalMethods act on the Herdr client attached to this session, not on a
// server's shared state, so routing them to another machine has no meaning:
// there is no attached client on the far end of a forwarded socket to retitle,
// dismiss a popup on, or hand a session to. They keep a plain local-only schema
// so a caller cannot ask for something that would silently do nothing.
var clientLocalMethods = map[string]bool{
	"client.window_title.set":      true,
	"client.window_title.clear":    true,
	"client_shell.surface.set":     true,
	"popup.close":                  true,
	"product_announcement.dismiss": true,
	"release_notes.dismiss":        true,
	"server.live_handoff":          true,
}

func routable(method string) bool { return !clientLocalMethods[method] }

// withMachineArgument returns a copy of input that also accepts "machine".
func withMachineArgument(input map[string]any) map[string]any {
	clone := make(map[string]any, len(input)+1)
	for key, value := range input {
		clone[key] = value
	}
	properties, _ := clone["properties"].(map[string]any)
	merged := make(map[string]any, len(properties)+1)
	for name, schema := range properties {
		merged[name] = schema
	}
	merged[machineArgument] = map[string]any{
		"type":        "string",
		"description": machineArgumentDescription,
	}
	clone["properties"] = merged
	return clone
}

// splitMachine removes the routing parameter from normalized arguments. Herdr's
// socket rejects unknown params, and "machine" is this bridge's, not its.
func splitMachine(params json.RawMessage) (string, json.RawMessage, error) {
	var decoded map[string]any
	if len(params) == 0 || string(params) == "null" {
		return "", params, nil
	}
	if err := json.Unmarshal(params, &decoded); err != nil {
		return "", params, nil
	}
	raw, present := decoded[machineArgument]
	if !present {
		return "", params, nil
	}
	delete(decoded, machineArgument)
	selector, ok := raw.(string)
	if !ok {
		return "", nil, fmt.Errorf("%q must be a string naming a saved machine", machineArgument)
	}
	selector = strings.TrimSpace(selector)
	rest, err := json.Marshal(decoded)
	if err != nil {
		return "", nil, fmt.Errorf("encode arguments without %q: %w", machineArgument, err)
	}
	return selector, rest, nil
}

// callerFor picks the transport a call should travel over.
func (s *Server) callerFor(ctx context.Context, method, selector string) (herdr.Transport, error) {
	if selector == "" {
		return s.Client, nil
	}
	if !routable(method) {
		return nil, fmt.Errorf("%s acts on the Herdr client attached to this session and cannot be routed to machine %q",
			strings.ReplaceAll(method, ".", "_"), selector)
	}
	if s.Machines == nil {
		return nil, fmt.Errorf("this bridge was started without machine routing, so %q cannot be reached; restart herdr-mcp without --no-machines", selector)
	}
	caller, err := s.Machines.Caller(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("machine %q: %w", selector, err)
	}
	return caller, nil
}

const machineListTool = "machine_list"

var machineListSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{},
}

// registerMachineList adds the one tool that is this bridge's own rather than a
// Herdr socket method. Machine profiles live in Herdr's client configuration
// and have no socket method, so without this a caller has no way to discover
// what it may pass as "machine".
func (s *Server) registerMachineList() {
	readOnly := true
	closedWorld := false
	s.MCP.AddTool(&mcp.Tool{
		Name:  machineListTool,
		Title: "Machine List",
		Description: "List the saved Herdr SSH machines this bridge can drive, and which ones it currently holds a connection to. " +
			"Pass a label or id as the \"machine\" argument on any other tool to run it there. Inputs: none.",
		InputSchema: machineListSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   readOnly,
			IdempotentHint: readOnly,
			OpenWorldHint:  &closedWorld,
		},
	}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.listMachines(ctx), nil
	})
}

func (s *Server) listMachines(ctx context.Context) *mcp.CallToolResult {
	if s.Machines == nil {
		return errorResult(machineListTool, fmt.Errorf("machine routing is disabled on this bridge"))
	}
	payload := map[string]any{
		"machines": s.Machines.Status(ctx),
		"note": "Pass a label or id as the \"machine\" argument on any tool. " +
			"Workspace, tab, pane and agent IDs are scoped per machine; omit \"machine\" for the local session.",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errorResult(machineListTool, err)
	}
	var structured map[string]any
	if err := json.Unmarshal(encoded, &structured); err != nil {
		return errorResult(machineListTool, err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: structured,
	}
}
