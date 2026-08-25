package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `This server exposes the active Herdr terminal session's socket API.
Each MCP tool maps directly to one Herdr method: underscores in the tool name correspond to dots in the socket method name (agent_read calls agent.read).

Common agent workflow: worktree_create or workspace_create creates a root_pane.pane_id; pane_split creates another pane_id; agent_start requires that existing pane_id and does not create panes. agent_start waits for interactive readiness when possible. Then call agent_prompt with target (agent name or pane_id) and text; use agent_wait to wait for status and agent_read with source=recent_unwrapped to read output.

Read source values are visible (rendered viewport), recent (scrollback with soft wraps), recent_unwrapped (scrollback with wraps joined; best for logs), and detection (agent detector buffer).
Use session_snapshot or the list methods to discover stable workspace, tab, pane, and agent identifiers before mutating state.
Prefer agent_prompt, agent_wait, and agent_read for agent conversations. pane_send_text and pane_send_keys are lower-level terminal input and can interleave with an agent's active turn.
Close, remove, unlink, uninstall, release, and server-stop methods are destructive. Only call them when the user explicitly intends that state change.
events_subscribe and harness-internal lifecycle reporting are intentionally omitted from this client-facing tool surface.`

// defaultSlowCallThreshold is the point past which a *successful* call still
// earns a log line. This server writes no response bytes until the tool
// returns, so a call that runs this long is one an HTTP intermediary may
// already have given up on -- the client sees a dead request while the server
// sees a success, and only the log connects the two.
const defaultSlowCallThreshold = 60 * time.Second

// Server wraps a dynamically registered MCP server and its source schema.
type Server struct {
	MCP     *mcp.Server
	Schema  *herdr.Schema
	Methods []herdr.MethodDefinition
	Client  *herdr.Client
	Version string
	// Logf receives one line per failed or slow tool call. Tool failures travel
	// to the client inside the result content, and some clients render an
	// errored result without that content at all, so this log is the only place
	// an operator can find out what actually broke.
	Logf func(format string, args ...any)
	// SlowCallThreshold overrides defaultSlowCallThreshold.
	SlowCallThreshold time.Duration
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
			Description: toolDescription(definition.Method, definition.InputSchema),
			InputSchema: definition.InputSchema,
			Annotations: annotations(definition.Method),
		}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return server.call(ctx, definition.Method, definition.InputSchema, request.Params.Arguments), nil
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

func (s *Server) call(ctx context.Context, method string, input map[string]any, arguments json.RawMessage) *mcp.CallToolResult {
	started := time.Now()
	result := s.dispatch(ctx, method, input, arguments)
	s.observe(method, time.Since(started), result)
	return result
}

// observe records the outcomes an operator cannot otherwise see. Successful,
// prompt calls stay silent so the log remains readable under load.
func (s *Server) observe(method string, elapsed time.Duration, result *mcp.CallToolResult) {
	logf := s.Logf
	if logf == nil {
		logf = log.Printf
	}
	threshold := s.SlowCallThreshold
	if threshold <= 0 {
		threshold = defaultSlowCallThreshold
	}
	switch {
	case result.IsError:
		logf("tool %s failed after %s: %s", method, elapsed.Round(time.Microsecond), resultText(result))
	case elapsed >= threshold:
		logf("tool %s succeeded after %s, past the %s mark where a client or proxy may already have abandoned the request",
			method, elapsed.Round(time.Microsecond), threshold)
	}
}

func resultText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return "(no content)"
}

func (s *Server) dispatch(ctx context.Context, method string, input map[string]any, arguments json.RawMessage) *mcp.CallToolResult {
	normalized, notes, err := normalizeArguments(method, input, arguments)
	if err != nil {
		return errorResult(method, err)
	}
	if method == "agent.wait" {
		if err := s.waitThroughLaunch(ctx, normalized); err != nil {
			return errorResult(method, err, notes...)
		}
	}
	result, err := s.Client.Call(ctx, method, normalized)
	if err != nil {
		if strings.HasPrefix(method, "agent.") {
			err = s.enrichAgentError(ctx, method, normalized, err)
		}
		return errorResult(method, err, notes...)
	}
	if method == "agent.start" {
		var readinessNote string
		result, readinessNote, err = s.waitForStartedAgent(ctx, result, normalized)
		if err != nil {
			return errorResult(method, err, notes...)
		}
		if readinessNote != "" {
			notes = append(notes, readinessNote)
		}
	}

	var structured map[string]any
	if err := json.Unmarshal(result, &structured); err != nil {
		return errorResult(method, fmt.Errorf("Herdr returned a non-object result: %w", err))
	}
	content := []mcp.Content{&mcp.TextContent{Text: string(result)}}
	for _, note := range notes {
		content = append(content, &mcp.TextContent{Text: note})
	}
	return &mcp.CallToolResult{
		Content:           content,
		StructuredContent: structured,
	}
}

func errorResult(method string, err error, notes ...string) *mcp.CallToolResult {
	payload, _ := json.Marshal(map[string]any{
		"method": method,
		"error":  err.Error(),
	})
	content := []mcp.Content{&mcp.TextContent{Text: string(payload)}}
	for _, note := range notes {
		content = append(content, &mcp.TextContent{Text: note})
	}
	return &mcp.CallToolResult{
		Content: content,
		IsError: true,
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
