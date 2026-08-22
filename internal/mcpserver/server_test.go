package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/Orange-County-AI/herdr-mcp/internal/herdr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const bridgeSchema = `{
  "protocol": 20,
  "schema_version": 1,
  "schemas": {
    "request": {
      "oneOf": [{
        "properties": {
          "method": {"const": "pane.read"},
          "params": {"$ref": "#/schemas/request/$defs/PaneReadParams"}
        }
      }],
      "$defs": {
        "PaneReadParams": {
          "type": "object",
          "properties": {"pane_id": {"type": "string"}},
          "required": ["pane_id"]
        }
      }
    }
  }
}`

func TestDynamicToolBridgesToSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan map[string]any, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		var request map[string]any
		_ = json.Unmarshal(line, &request)
		requestSeen <- request
		_, _ = fmt.Fprintln(conn, `{"id":"bridge","result":{"type":"pane_read","read":{"text":"hello"}}}`)
	}()

	schema, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(schema, &herdr.Client{SocketPath: socket}, "test", nil, nil)
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
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "pane_read" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "pane_read",
		Arguments: map[string]any{"pane_id": "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool result = %+v", result)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["type"] != "pane_read" {
		t.Fatalf("structured result = %#v", result.StructuredContent)
	}
	request := <-requestSeen
	if request["method"] != "pane.read" {
		t.Fatalf("socket method = %v", request["method"])
	}
}
