package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestClientCallRoundTrip(t *testing.T) {
	socket, requests, stop := fakeSocket(t, func(request map[string]any) string {
		return fmt.Sprintf(`{"id":%q,"result":{"type":"pane_list","panes":[]}}`, request["id"])
	})
	defer stop()

	client := &Client{SocketPath: socket}
	result, err := client.Call(context.Background(), "pane.list", json.RawMessage(`{"workspace_id":"w1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "pane_list" {
		t.Fatalf("result = %s", result)
	}
	request := <-requests
	if request["method"] != "pane.list" {
		t.Fatalf("method = %v", request["method"])
	}
	params := request["params"].(map[string]any)
	if params["workspace_id"] != "w1" {
		t.Fatalf("params = %v", params)
	}
}

func TestClientReturnsHerdrErrors(t *testing.T) {
	socket, _, stop := fakeSocket(t, func(request map[string]any) string {
		return fmt.Sprintf(`{"id":%q,"error":{"code":"pane_not_found","message":"pane p1 is gone"}}`, request["id"])
	})
	defer stop()

	client := &Client{SocketPath: socket}
	_, err := client.Call(context.Background(), "pane.get", json.RawMessage(`{"pane_id":"p1"}`))
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != "pane_not_found" {
		t.Fatalf("error = %#v", err)
	}
}

func TestClientHonorsContextCancellation(t *testing.T) {
	socket, _, stop := fakeSocket(t, func(map[string]any) string {
		time.Sleep(250 * time.Millisecond)
		return `{"id":"late","result":{"type":"ok"}}`
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &Client{SocketPath: socket}
	if _, err := client.Call(ctx, "agent.wait", json.RawMessage(`{"target":"agent"}`)); err == nil {
		t.Fatal("cancelled call succeeded")
	}
}

func fakeSocket(t *testing.T, respond func(map[string]any) string) (string, <-chan map[string]any, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal(line, &request) != nil {
			return
		}
		requests <- request
		_, _ = fmt.Fprintln(conn, respond(request))
	}()
	stop := func() {
		_ = listener.Close()
		<-done
	}
	return path, requests, stop
}
