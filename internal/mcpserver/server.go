package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
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
events_subscribe and harness-internal lifecycle reporting are intentionally omitted from this client-facing tool surface.

Saved SSH machines: most tools take an optional machine argument naming one of Herdr's saved machines by label or profile id. Call machine_list to see them; omit machine for the local session. Each machine is an independent Herdr server, so workspace, tab, pane and agent IDs are scoped to it: two machines can both have w1:p1 or an agent named reviewer. Discover IDs on the machine you intend to drive, never reuse a local one there. Connections are made over SSH on first use and a failed remote call never falls back to the local session, so a connection error does not prove a mutation was not applied -- inspect remote state before retrying. Tools that act on the attached client (window title, popup, announcements, live handoff) are local-only and take no machine argument.`

// defaultSlowCallThreshold is the point past which a *successful* call still
// earns a log line. This server writes no response bytes until the tool
// returns, so a call that runs this long is one an HTTP intermediary may
// already have given up on -- the client sees a dead request while the server
// sees a success, and only the log connects the two.
const defaultSlowCallThreshold = 60 * time.Second

// Caller is the Herdr transport tool calls dispatch through. Production passes
// a *herdr.Queue so calls are admission-controlled and survive an outage; tests
// pass a bare *herdr.Client.
type Caller interface {
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
}

// Server wraps a dynamically registered MCP server and its source schema.
type Server struct {
	MCP     *mcp.Server
	Schema  *herdr.Schema
	Methods []herdr.MethodDefinition
	Client  Caller
	Version string
	// Logf receives one line per failed or slow tool call. Tool failures travel
	// to the client inside the result content, and some clients render an
	// errored result without that content at all, so this log is the only place
	// an operator can find out what actually broke.
	Logf func(format string, args ...any)
	// SlowCallThreshold overrides defaultSlowCallThreshold.
	SlowCallThreshold time.Duration
	// Machines routes a call carrying a "machine" argument to that saved SSH
	// machine. Nil disables routing and leaves the tool schemas local-only.
	Machines MachineRouter

	// mu guards the fields Reload swaps while calls are in flight.
	mu      sync.Mutex
	allow   []string
	deny    []string
	toolSet map[string]struct{}
}

// Options configures a bridge. Only Schema, Client and Version are required.
type Options struct {
	Schema  *herdr.Schema
	Client  Caller
	Version string
	Allow   []string
	Deny    []string
	// Machines, when set, adds a "machine" argument to every routable tool and
	// dispatches those calls to the named saved SSH machine.
	Machines MachineRouter
}

// New registers one MCP tool for every selected method in the Herdr request schema.
func New(options Options) (*Server, error) {
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "herdr-mcp",
		Title:   "Herdr socket API",
		Version: options.Version,
	}, &mcp.ServerOptions{Instructions: instructions})

	server := &Server{
		MCP:      mcpServer,
		Client:   options.Client,
		Version:  options.Version,
		Machines: options.Machines,
		allow:    options.Allow,
		deny:     options.Deny,
		toolSet:  map[string]struct{}{},
	}
	if err := server.register(options.Schema); err != nil {
		return nil, err
	}
	if server.Machines != nil {
		server.registerMachineList()
	}
	return server, nil
}

// register swaps the tool surface to the one this schema describes. It is the
// body of both construction and Reload, so a hot reload cannot drift from a
// cold start.
func (s *Server) register(schema *herdr.Schema) error {
	methods, err := schema.Methods(s.allow, s.deny)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		definition := method
		if s.Machines != nil && routable(definition.Method) {
			definition.InputSchema = withMachineArgument(definition.InputSchema)
		}
		fresh[definition.ToolName] = struct{}{}
		s.MCP.AddTool(&mcp.Tool{
			Name:        definition.ToolName,
			Title:       toolTitle(definition.Method),
			Description: toolDescription(definition.Method, definition.InputSchema),
			InputSchema: definition.InputSchema,
			Annotations: annotations(definition.Method),
		}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, definition.Method, definition.InputSchema, request.Params.Arguments), nil
		})
	}
	// Anything the previous schema had and this one does not must go, or the
	// bridge keeps advertising a method the running Herdr no longer answers.
	var removed []string
	for name := range s.toolSet {
		if _, kept := fresh[name]; !kept {
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		s.MCP.RemoveTools(removed...)
	}
	s.toolSet = fresh
	s.Schema = schema
	s.Methods = methods
	return nil
}

// Reload re-registers the tool surface from a newly read schema and reports
// what changed.
//
// This exists because a protocol number is not a staleness signal: Herdr added
// a method inside protocol 22, and a bridge that only compares protocols served
// the old tool list indefinitely while reporting itself healthy. Re-reading the
// schema and swapping the tools is the fix; the MCP tools/list_changed
// notification the SDK emits tells connected clients to look again.
func (s *Server) Reload(schema *herdr.Schema) (added, removed []string, err error) {
	s.mu.Lock()
	previous := make(map[string]struct{}, len(s.toolSet))
	for name := range s.toolSet {
		previous[name] = struct{}{}
	}
	s.mu.Unlock()

	if err := s.register(schema); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.toolSet {
		if _, existed := previous[name]; !existed {
			added = append(added, name)
		}
	}
	for name := range previous {
		if _, kept := s.toolSet[name]; !kept {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, nil
}

// ToolCount reports how many tools are registered right now.
func (s *Server) ToolCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.toolSet)
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
	selector, normalized, err := splitMachine(normalized)
	if err != nil {
		return errorResult(method, err, notes...)
	}
	caller, err := s.callerFor(ctx, method, selector)
	if err != nil {
		return errorResult(method, err, notes...)
	}
	if method == "agent.wait" {
		if err := s.waitThroughLaunch(ctx, caller, normalized); err != nil {
			return errorResult(method, err, notes...)
		}
	}
	result, err := caller.Call(ctx, method, normalized)
	if err != nil {
		if strings.HasPrefix(method, "agent.") {
			err = s.enrichAgentError(ctx, caller, method, normalized, err)
		}
		return errorResult(method, err, notes...)
	}
	if method == "agent.start" {
		var readinessNote string
		result, readinessNote, err = s.waitForStartedAgent(ctx, caller, result, normalized)
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
