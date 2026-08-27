package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
// Every call dials its own connection, so concurrent calls never queue behind
// each other.
type Client struct {
	SocketPath string
	// CallTimeout overrides the per-method socket budget. Production leaves it
	// nil and gets callTimeout; tests set it so a timeout case can be asserted
	// on its message rather than on the clock.
	CallTimeout func(method string, params json.RawMessage) time.Duration
	sequence    atomic.Uint64
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

// UnavailableError reports that a call never reached Herdr: the dial itself
// failed, so not one byte of the request was written.
//
// That distinction is what makes a retry safe. Once the request is on the wire
// a failure is ambiguous -- Herdr may have applied pane.close and died before
// answering -- and resending it could apply the mutation twice. A failed dial
// carries no such ambiguity, so this is the only error the queue retries.
type UnavailableError struct {
	SocketPath string
	Err        error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("connect to Herdr socket %s: %v", e.SocketPath, e.Err)
}

func (e *UnavailableError) Unwrap() error { return e.Err }

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
		return nil, &UnavailableError{SocketPath: c.SocketPath, Err: err}
	}
	defer conn.Close()

	budget := c.callTimeout(method, params)
	started := time.Now()
	deadline := started.Add(budget)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set Herdr socket deadline: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	if _, err := conn.Write(body); err != nil {
		return nil, callFailure(ctx, method, time.Since(started), budget, err)
	}
	limited := io.LimitReader(conn, maxResponseBytes+1)
	line, readErr := bufio.NewReader(limited).ReadBytes('\n')
	if len(line) > maxResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", method, maxResponseBytes)
	}
	if readErr != nil && len(line) == 0 {
		return nil, callFailure(ctx, method, time.Since(started), budget, readErr)
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

// callFailure names the real cause of a socket write/read failure.
//
// Cancelling the context closes the connection out from under the in-flight
// read, so the raw error is "use of closed network connection" -- which reads
// as a Herdr fault when in fact the caller is the one that walked away. An
// expired socket deadline is likewise reported as a bare "i/o timeout" that a
// caller cannot tell apart from Herdr refusing the request. Both are the same
// opaque failure from the outside, so both get attributed here.
func callFailure(ctx context.Context, method string, elapsed, budget time.Duration, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		reason := "was cancelled by the caller"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			reason = "hit the caller's deadline"
		}
		return fmt.Errorf("%s %s after %s; Herdr may still be running it: %w",
			method, reason, elapsed.Round(time.Millisecond), ctxErr)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("Herdr did not answer %s within %s", method, budget.Round(time.Millisecond))
	}
	return fmt.Errorf("read %s response: %w", method, err)
}

func (c *Client) callTimeout(method string, params json.RawMessage) time.Duration {
	if c.CallTimeout != nil {
		return c.CallTimeout(method, params)
	}
	return callTimeout(method, params)
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
