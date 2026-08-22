package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const maxResponseBytes = 64 << 20

// Client performs one newline-delimited request/response exchange per socket connection.
type Client struct {
	SocketPath string
	sequence   atomic.Uint64
}

// APIError is an error returned by the Herdr socket server.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// DefaultSocketPath follows Herdr's default-session socket convention.
func DefaultSocketPath() (string, error) {
	if path := os.Getenv("HERDR_SOCKET_PATH"); path != "" {
		return path, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(configDir, "herdr", "herdr.sock"), nil
}

// Call invokes one Herdr socket method and returns its raw result object.
func (c *Client) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if c.SocketPath == "" {
		return nil, fmt.Errorf("Herdr socket path is empty")
	}
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage(`{}`)
	}
	if !json.Valid(params) {
		return nil, fmt.Errorf("arguments for %s are not valid JSON", method)
	}

	request := struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{
		ID:     fmt.Sprintf("herdr-mcp:%d", c.sequence.Add(1)),
		Method: method,
		Params: params,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	body = append(body, '\n')

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to Herdr socket %s: %w", c.SocketPath, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(callTimeout(method, params))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set Herdr socket deadline: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	if _, err := conn.Write(body); err != nil {
		return nil, fmt.Errorf("write %s request: %w", method, err)
	}
	limited := io.LimitReader(conn, maxResponseBytes+1)
	line, readErr := bufio.NewReader(limited).ReadBytes('\n')
	if len(line) > maxResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", method, maxResponseBytes)
	}
	if readErr != nil && len(line) == 0 {
		return nil, fmt.Errorf("read %s response: %w", method, readErr)
	}

	var response struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", method, err)
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		var apiErr APIError
		if err := json.Unmarshal(response.Error, &apiErr); err != nil || apiErr.Message == "" {
			return nil, &APIError{Message: string(response.Error)}
		}
		return nil, &apiErr
	}
	if len(response.Result) == 0 {
		return nil, fmt.Errorf("%s response has neither result nor error", method)
	}
	return response.Result, nil
}

// Ping confirms that the socket and selected Herdr binary use the same protocol.
func (c *Client) Ping(ctx context.Context) (version string, protocol int, err error) {
	result, err := c.Call(ctx, "ping", json.RawMessage(`{}`))
	if err != nil {
		return "", 0, err
	}
	var pong struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
	}
	if err := json.Unmarshal(result, &pong); err != nil {
		return "", 0, fmt.Errorf("decode ping result: %w", err)
	}
	if pong.Protocol <= 0 {
		return "", 0, fmt.Errorf("ping returned invalid protocol %d", pong.Protocol)
	}
	return pong.Version, pong.Protocol, nil
}

func callTimeout(method string, params json.RawMessage) time.Duration {
	if timeout := timeoutFromParams(params); timeout > 0 {
		return timeout + 5*time.Second
	}
	switch method {
	case "agent.wait", "agent.prompt", "events.wait", "pane.wait_for_output":
		return 15 * time.Minute
	case "worktree.create", "worktree.open", "worktree.remove", "workspace.create", "server.live_handoff":
		return 2 * time.Minute
	default:
		return 10 * time.Second
	}
}

func timeoutFromParams(params json.RawMessage) time.Duration {
	var decoded map[string]any
	if json.Unmarshal(params, &decoded) != nil {
		return 0
	}
	if milliseconds, ok := numberAsInt64(decoded["timeout_ms"]); ok && milliseconds > 0 {
		return time.Duration(milliseconds) * time.Millisecond
	}
	if wait, ok := decoded["wait"].(map[string]any); ok {
		if milliseconds, ok := numberAsInt64(wait["timeout_ms"]); ok && milliseconds > 0 {
			return time.Duration(milliseconds) * time.Millisecond
		}
	}
	return 0
}

func numberAsInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), number >= 0 && number == float64(int64(number))
	case json.Number:
		integer, err := number.Int64()
		return integer, err == nil
	default:
		return 0, false
	}
}
