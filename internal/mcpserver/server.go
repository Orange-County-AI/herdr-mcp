package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `This server exposes the active Herdr terminal session's socket API.
Each MCP tool maps directly to one Herdr method: underscores in the tool name correspond to dots in the socket method name (agent_read calls agent.read).
Use session_snapshot or the list methods to discover stable workspace, tab, pane, and agent identifiers before mutating state.
Prefer agent_prompt, agent_wait, and agent_read for agent conversations. pane_send_text and pane_send_keys are lower-level terminal input and can interleave with an agent's active turn.
Close, remove, unlink, uninstall, release, and server-stop methods are destructive. Only call them when the user explicitly intends that state change.
events_subscribe is omitted because its streaming socket lifetime cannot be represented by one MCP tool call; use events_wait or pane_wait_for_output instead.`

// Server wraps a dynamically registered MCP server and its source schema.
type Server struct {
	MCP     *mcp.Server
	Schema  *herdr.Schema
	Methods []herdr.MethodDefinition
	Client  *herdr.Client
	Version string
}

// New registers one MCP tool for every selected method in the Herdr request schema.
func New(schema *herdr.Schema, client *herdr.Client, version string, allow, deny []string) (*Server, error) {
	methods, err := schema.Methods(allow, deny)
	if err != nil {
		return nil, err
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "herdr-mcp",
		Title:   "Herdr socket API",
		Version: version,
	}, &mcp.ServerOptions{Instructions: instructions})

	server := &Server{
		MCP:     mcpServer,
		Schema:  schema,
		Methods: methods,
		Client:  client,
		Version: version,
	}
	for _, method := range methods {
		definition := method
		mcpServer.AddTool(&mcp.Tool{
			Name:        definition.ToolName,
			Title:       toolTitle(definition.Method),
			Description: fmt.Sprintf("Invoke Herdr socket method %q. The input schema comes from the selected Herdr binary (protocol %d, schema %d).", definition.Method, schema.Protocol, schema.SchemaVersion),
			InputSchema: definition.InputSchema,
			Annotations: annotations(definition.Method),
		}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return server.call(ctx, definition.Method, request.Params.Arguments), nil
		})
	}
	return server, nil
}

// HTTPHandler returns the Streamable HTTP transport suitable for /mcp.
func (s *Server) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCP }, &mcp.StreamableHTTPOptions{
		// The HTTP listener is loopback-only and normally reached through a
		// Cloudflare Tunnel, whose public Host header is expected.
		DisableLocalhostProtection: true,
	})
}

func (s *Server) call(ctx context.Context, method string, arguments json.RawMessage) *mcp.CallToolResult {
	result, err := s.Client.Call(ctx, method, arguments)
	if err != nil {
		payload, _ := json.Marshal(map[string]any{
			"method": method,
			"error":  err.Error(),
		})
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			IsError: true,
		}
	}

	var structured map[string]any
	if err := json.Unmarshal(result, &structured); err != nil {
		payload, _ := json.Marshal(map[string]any{
			"method": method,
			"error":  "Herdr returned a non-object result: " + err.Error(),
		})
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			IsError: true,
		}
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(result)}},
		StructuredContent: structured,
	}
}

func toolTitle(method string) string {
	parts := strings.FieldsFunc(method, func(r rune) bool { return r == '.' || r == '_' })
	for index, part := range parts {
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func annotations(method string) *mcp.ToolAnnotations {
	closedWorld := false
	readOnly := isReadOnly(method)
	annotation := &mcp.ToolAnnotations{
		ReadOnlyHint:   readOnly,
		IdempotentHint: readOnly,
		OpenWorldHint:  &closedWorld,
	}
	if !readOnly {
		destructive := isDestructive(method)
		annotation.DestructiveHint = &destructive
	}
	return annotation
}

func isReadOnly(method string) bool {
	if method == "ping" || method == "session.snapshot" {
		return true
	}
	for _, suffix := range []string{
		".list", ".get", ".read", ".explain", ".info", ".current", ".snapshot",
		".export", ".wait", ".wait_for_output", ".neighbor", ".edges", ".process_info",
		".agent_manifests", ".action.list", ".log.list",
	} {
		if strings.HasSuffix(method, suffix) {
			return true
		}
	}
	return false
}

func isDestructive(method string) bool {
	if method == "server.stop" || method == "server.live_handoff" {
		return true
	}
	for _, suffix := range []string{
		".close", ".remove", ".unlink", ".uninstall", ".disable",
		".clear_agent_authority", ".release_agent",
	} {
		if strings.HasSuffix(method, suffix) {
			return true
		}
	}
	return false
}
