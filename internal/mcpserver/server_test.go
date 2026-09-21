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
	server, err := New(Options{Schema: schema, Client: &herdr.Client{SocketPath: socket}, Version: "test"})
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

// bridgeSchemaWithLinkResolve is bridgeSchema plus one method, at the SAME
// protocol number. This is the production failure in miniature: Herdr 0.9.1
// added pane.link.resolve inside protocol 22, so a bridge comparing only
// protocols served the old tool list while reporting itself healthy.
const bridgeSchemaWithLinkResolve = `{
  "protocol": 20,
  "schema_version": 1,
  "schemas": {
    "request": {
      "oneOf": [{
        "properties": {
          "method": {"const": "pane.read"},
          "params": {"$ref": "#/schemas/request/$defs/PaneReadParams"}
        }
      }, {
        "properties": {
          "method": {"const": "pane.link.resolve"},
          "params": {"$ref": "#/schemas/request/$defs/PaneLinkResolveParams"}
        }
      }],
      "$defs": {
        "PaneReadParams": {
          "type": "object",
          "properties": {"pane_id": {"type": "string"}},
          "required": ["pane_id"]
        },
        "PaneLinkResolveParams": {
          "type": "object",
          "properties": {"pane_id": {"type": "string"}},
          "required": ["pane_id"]
        }
      }
    }
  }
}`

func TestReloadPicksUpAMethodAddedWithinOneProtocol(t *testing.T) {
	before, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	after, err := herdr.ParseSchema([]byte(bridgeSchemaWithLinkResolve))
	if err != nil {
		t.Fatal(err)
	}
	if before.Protocol != after.Protocol {
		t.Fatalf("fixture protocols differ (%d, %d); the point is that they do not", before.Protocol, after.Protocol)
	}
	if before.Digest == after.Digest {
		t.Fatal("digests match for different documents; drift would be undetectable")
	}

	server, err := New(Options{Schema: before, Client: &recordingTransport{}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if server.ToolCount() != 1 {
		t.Fatalf("tools before reload = %d, want 1", server.ToolCount())
	}

	added, removed, err := server.Reload(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "pane_link_resolve" {
		t.Fatalf("added = %v", added)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v", removed)
	}
	if server.ToolCount() != 2 {
		t.Fatalf("tools after reload = %d, want 2", server.ToolCount())
	}
}

func TestReloadRemovesToolsTheNewSchemaDropped(t *testing.T) {
	before, err := herdr.ParseSchema([]byte(bridgeSchemaWithLinkResolve))
	if err != nil {
		t.Fatal(err)
	}
	after, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{Schema: before, Client: &recordingTransport{}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	added, removed, err := server.Reload(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 || len(removed) != 1 || removed[0] != "pane_link_resolve" {
		t.Fatalf("added = %v, removed = %v", added, removed)
	}
	if server.ToolCount() != 1 {
		t.Fatalf("tools = %d, want 1", server.ToolCount())
	}
}

// Routing must be reflected in the schemas clients see, and machine_list must
// exist, or a caller has no way to discover what "machine" accepts.
func TestMachineRoutingAddsTheArgumentAndTheDiscoveryTool(t *testing.T) {
	schema, err := herdr.ParseSchema([]byte(bridgeSchema))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		Schema:   schema,
		Client:   &recordingTransport{},
		Version:  "test",
		Machines: &stubRouter{caller: &recordingTransport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	properties, _ := server.Methods[0].InputSchema["properties"].(map[string]any)
	if _, ok := properties["machine"]; ok {
		t.Fatal("Methods carries the injected schema; it should stay the raw Herdr one")
	}
	if server.ToolCount() != 1 {
		t.Fatalf("socket tools = %d, want 1", server.ToolCount())
	}
	result := server.listMachines(context.Background())
	if result.IsError {
		t.Fatalf("machine_list failed: %v", result.Content)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("machine_list structured content = %T", result.StructuredContent)
	}
	if _, ok := structured["machines"]; !ok {
		t.Fatalf("machine_list result = %v", structured)
	}
}
