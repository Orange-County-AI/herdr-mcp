package httptransport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAuthenticatedMCPSessionLifecycle(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	transport, err := New(bearerConfig(listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	sdkServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	mcp.AddTool(sdkServer, &mcp.Tool{Name: "echo", Description: "test echo"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		Text string `json:"text"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: input.Text}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdkServer }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, EventStore: mcp.NewMemoryEventStore(nil)})
	server := &http.Server{Handler: transport.Guard(handler, nil)}
	done := make(chan error, 1)
	go func() { done <- transport.Serve(server, listener) }()
	t.Cleanup(func() { server.Close(); <-done })
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	endpoint := "http://" + listener.Addr().String() + "/mcp"
	request := func(method, body, session, auth, event string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if event != "" {
			req.Header.Set("Last-Event-ID", event)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	read := func(response *http.Response) string {
		t.Helper()
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"test"}}}`
	for _, token := range []string{"", wrongSecret} {
		response := request("POST", initialize, "", token, "")
		read(response)
		if response.StatusCode != 401 {
			t.Fatal("initialization bypass")
		}
	}
	response := request("POST", initialize, "", testSecret, "")
	session := response.Header.Get("Mcp-Session-Id")
	initialized := read(response)
	if response.StatusCode != 200 || session == "" || !strings.Contains(initialized, "protocolVersion") {
		t.Fatalf("initialization failed: status=%d session=%q body=%s", response.StatusCode, session, initialized)
	}
	notification := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`
	for _, body := range []string{notification, list, call} {
		for _, token := range []string{"", wrongSecret} {
			response := request("POST", body, session, token, "")
			read(response)
			if response.StatusCode != 401 {
				t.Fatal("session id substituted for auth")
			}
		}
		response := request("POST", body, session, testSecret, "")
		result := read(response)
		want := 200
		if body == notification {
			want = 202
		}
		if response.StatusCode != want {
			t.Fatalf("MCP POST status=%d body=%s", response.StatusCode, result)
		}
		if body == list && !strings.Contains(result, "echo") {
			t.Fatal("discovery failed")
		}
		if body == call && !strings.Contains(result, "hello") {
			t.Fatal("call failed")
		}
	}
	// Open and resume the actual SSE session using a tool-list notification.
	for _, event := range []string{"", "_standalone:0"} {
		for _, token := range []string{"", wrongSecret} {
			response := request("GET", "", session, token, event)
			read(response)
			if response.StatusCode != 401 {
				t.Fatal("stream authentication bypass")
			}
		}
	}
	stream := request("GET", "", session, testSecret, "")
	if stream.StatusCode != 200 || !strings.Contains(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream status=%d", stream.StatusCode)
	}
	sdkServer.RemoveTools("echo")
	reader := bufio.NewReader(stream.Body)
	eventID := ""
	for i := 0; i < 8; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			break
		}
	}
	stream.Body.Close()
	if eventID == "" {
		t.Fatal("missing resumption event id")
	}
	resumed := request("GET", "", session, testSecret, eventID)
	// Closing the client body cancels the server stream asynchronously. Retry
	// the SDK's transient conflict until that cancellation reaches the server.
	deadline := time.Now().Add(3 * time.Second)
	for resumed.StatusCode == http.StatusConflict && time.Now().Before(deadline) {
		read(resumed)
		time.Sleep(10 * time.Millisecond)
		resumed = request("GET", "", session, testSecret, eventID)
	}
	if resumed.StatusCode != 200 {
		t.Fatalf("resumption status=%d body=%s", resumed.StatusCode, read(resumed))
	}
	resumed.Body.Close()
	for _, token := range []string{"", wrongSecret} {
		response := request("DELETE", "", session, token, "")
		read(response)
		if response.StatusCode != 401 {
			t.Fatal("unauthorized session deletion")
		}
	}
	response = request("DELETE", "", session, testSecret, "")
	read(response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	response = request("POST", list, session, testSecret, "")
	read(response)
	if response.StatusCode != 404 {
		t.Fatal("deleted session still usable")
	}
}

func TestCloudflareGuardCompatibility(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer jwks.Close()
	cfg := Config{Listen: "127.0.0.1:8091", AccessTeam: jwks.URL, AccessAudience: "test-audience", AllowedHosts: []string{"mcp.tunnel.test"}, LookupEnv: noEnv}
	transport, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: jwks.URL, Audience: jwt.ClaimStrings{"test-audience"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))})
	token.Header["kid"] = "test"
	assertion, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	h := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	for _, method := range []string{"GET", "POST", "DELETE", "OPTIONS", "HEAD"} {
		for _, value := range []string{"", "malformed", assertion} {
			req := httptest.NewRequest(method, "http://127.0.0.1:8091/mcp", nil)
			req.Host = "mcp.tunnel.test"
			req.Header.Set("Origin", "https://mcp.tunnel.test")
			req.Header.Set("Cf-Access-Jwt-Assertion", value)
			req.Header.Set("Authorization", "Bearer "+testSecret)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			want := 401
			if value == assertion {
				want = 204
			}
			if rec.Code != want {
				t.Fatalf("CF status=%d want=%d", rec.Code, want)
			}
		}
	}
	for _, origin := range []string{"https://attacker.test", "http://mcp.tunnel.test"} {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8091/mcp", nil)
		req.Host = "mcp.tunnel.test"
		req.Header.Set("Origin", origin)
		req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatal("CF Origin bypass")
		}
	}
}

func TestAccessHealthWithRewrittenLoopbackHostIsMinimal(t *testing.T) {
	transport, err := New(Config{Listen: "127.0.0.1:8091", AccessTeam: "https://test.cloudflareaccess.com", AccessAudience: "test", LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	h := transport.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("private diagnostics")) }), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	req := httptest.NewRequest("GET", "http://127.0.0.1:8091/healthz", nil)
	req.RemoteAddr = "127.0.0.1:4123"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatal("rewritten tunnel Host exposed diagnostics")
	}
}

func TestLiveCredentialRotationRequiresRestartAndNewSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(testSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	defer client.CloseIdleConnections()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Listen: listener.Addr().String(), BearerTokenFile: path, LookupEnv: noEnv}
	endpoint := "http://" + cfg.Listen + "/mcp"
	start := func(listener net.Listener) *http.Server {
		t.Helper()
		policy, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		sdk := mcp.NewServer(&mcp.Implementation{Name: "rotation-test", Version: "test"}, nil)
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
		server := &http.Server{Handler: policy.Guard(handler, nil)}
		done := make(chan error, 1)
		go func() { done <- policy.Serve(server, listener) }()
		t.Cleanup(func() {
			server.Close()
			for session := range sdk.Sessions() {
				session.Close()
			}
			<-done
		})
		return server
	}
	old := start(listener)
	request := func(method, body, session, token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, endpoint, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"rotation","version":"test"}}}`
	initial := request("POST", initialize, "", testSecret)
	session := initial.Header.Get("Mcp-Session-Id")
	io.Copy(io.Discard, initial.Body)
	initial.Body.Close()
	if initial.StatusCode != 200 || session == "" {
		t.Fatal("initial session failed")
	}
	notification := request("POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, session, testSecret)
	notification.Body.Close()
	if err := os.WriteFile(path, []byte(wrongSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	denied := request("POST", initialize, "", wrongSecret)
	denied.Body.Close()
	if denied.StatusCode != 401 {
		t.Fatal("file replacement activated new secret without restart")
	}
	stream := request("GET", "", session, testSecret)
	if stream.StatusCode != 200 {
		t.Fatal("old credential stopped working before restart")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(stream.Body)
	stream.Body.Close()
	if readErr == nil {
		t.Fatal("active stream survived server stop")
	}
	// A bridge restart invalidates its connections as well as its MCP sessions.
	// Discard the old pool before POSTing to a new server at the same address.
	client.CloseIdleConnections()
	listener, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	start(listener)
	denied = request("POST", initialize, "", testSecret)
	denied.Body.Close()
	if denied.StatusCode != 401 {
		t.Fatal("old credential survived restart")
	}
	stale := request("GET", "", session, wrongSecret)
	stale.Body.Close()
	if stale.StatusCode != 404 {
		t.Fatal("old session survived restart")
	}
	fresh := request("POST", initialize, "", wrongSecret)
	io.Copy(io.Discard, fresh.Body)
	fresh.Body.Close()
	if fresh.StatusCode != 200 || fresh.Header.Get("Mcp-Session-Id") == "" || fresh.Header.Get("Mcp-Session-Id") == session {
		t.Fatal("fresh initialization failed after rotation")
	}
}
