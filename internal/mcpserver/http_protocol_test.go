package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
)

// A modern client must be able to discover the protocol and immediately list
// tools without an initialize handshake or a persisted MCP session.
func TestHTTPModernDiscoveryAndToolList(t *testing.T) {
	schema, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{Schema: schema, Client: &recordingTransport{}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.HTTPHandler()
	request := func(method string) map[string]json.RawMessage {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"http-protocol-test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`, method)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", method)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", method, response.Code, response.Body.String())
		}
		payload := response.Body.Bytes()
		if strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
			payload = nil
			scanner := bufio.NewScanner(strings.NewReader(response.Body.String()))
			for scanner.Scan() {
				if data, ok := strings.CutPrefix(scanner.Text(), "data:"); ok && strings.TrimSpace(data) != "" {
					payload = []byte(strings.TrimSpace(data))
					break
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
		}
		var reply struct {
			Result map[string]json.RawMessage `json:"result"`
			Error  json.RawMessage            `json:"error"`
		}
		if err := json.Unmarshal(payload, &reply); err != nil {
			t.Fatalf("%s: invalid JSON-RPC response: %v", method, err)
		}
		if len(reply.Error) != 0 {
			t.Fatalf("%s: %s", method, reply.Error)
		}
		return reply.Result
	}

	discovery := request("server/discover")
	var versions []string
	if err := json.Unmarshal(discovery["supportedVersions"], &versions); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(versions, "2026-07-28") {
		t.Fatalf("modern protocol not advertised: %v", versions)
	}
	listed := request("tools/list")
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(listed["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == "pane_read" {
			return
		}
	}
	t.Fatalf("pane_read unavailable to modern client: %s", listed["tools"])
}
