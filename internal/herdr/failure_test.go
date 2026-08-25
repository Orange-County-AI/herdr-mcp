package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A caller that walks away mid-call closes the connection out from under the
// read. The raw net error blames the socket; the caller must be named instead,
// or an operator reads a client disconnect as a Herdr outage.
func TestCallAttributesCallerCancellation(t *testing.T) {
	socket, _, stop := fakeSocket(t, func(map[string]any) string {
		time.Sleep(400 * time.Millisecond)
		return `{"id":"late","result":{"type":"ok"}}`
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := (&Client{SocketPath: socket}).Call(ctx, "agent.wait", json.RawMessage(`{"target":"a"}`))
	if err == nil {
		t.Fatal("cancelled call succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not unwrap to context.Canceled: %v", err)
	}
	if !strings.Contains(err.Error(), "agent.wait") {
		t.Fatalf("error does not name the method: %v", err)
	}
	if !strings.Contains(err.Error(), "cancelled by the caller") {
		t.Fatalf("error does not attribute the cancellation to the caller: %v", err)
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		t.Fatalf("error still blames the socket for a caller-side cancellation: %v", err)
	}
}

// An expired socket budget must say Herdr went quiet and for how long. A bare
// "i/o timeout" is indistinguishable from Herdr refusing the request.
func TestCallNamesHerdrSilenceOnTimeout(t *testing.T) {
	socket, _, stop := fakeSocket(t, func(map[string]any) string {
		time.Sleep(400 * time.Millisecond)
		return `{"id":"late","result":{"type":"ok"}}`
	})
	defer stop()

	client := &Client{
		SocketPath:  socket,
		CallTimeout: func(string, json.RawMessage) time.Duration { return 40 * time.Millisecond },
	}
	_, err := client.Call(context.Background(), "pane.read", json.RawMessage(`{"pane_id":"p1"}`))
	if err == nil {
		t.Fatal("call past its budget succeeded")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Herdr's silence was misreported as a caller-side end: %v", err)
	}
	for _, want := range []string{"Herdr did not answer", "pane.read", "40ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

// Concurrent calls each dial their own connection. The invariant is that they
// genuinely overlap on the socket: a client that serialised behind one shared
// connection or a mutex would still return correct results, just one at a
// time, so correctness alone cannot detect it. The fake reports how many
// connections were ever open at once instead.
func TestConcurrentCallsOverlapOnTheSocket(t *testing.T) {
	socket, highWater, stop := fakeConcurrentSocket(t)
	defer stop()

	client := &Client{SocketPath: socket}
	const callers = 12
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := client.Call(context.Background(), "pane.list", json.RawMessage(`{}`))
			errs <- err
		}()
	}
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent call %d failed: %v", i, err)
		}
	}
	if peak := highWater(); peak < 2 {
		t.Fatalf("calls serialised: at most %d connection(s) were open at once, want overlap across %d callers", peak, callers)
	}
}

// fakeConcurrentSocket serves every connection it is offered and reports the
// high-water mark of simultaneously open connections, so a test can tell
// overlap apart from fast serial execution.
func fakeConcurrentSocket(t *testing.T) (string, func() int, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu       sync.Mutex
		open     int
		peak     int
		active   sync.WaitGroup
		accepted = make(chan struct{})
	)
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			active.Add(1)
			go func() {
				defer active.Done()
				defer conn.Close()
				mu.Lock()
				open++
				if open > peak {
					peak = open
				}
				mu.Unlock()
				defer func() {
					mu.Lock()
					open--
					mu.Unlock()
				}()

				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var request map[string]any
				if json.Unmarshal(line, &request) != nil {
					return
				}
				// Hold the connection open so overlapping callers are still
				// resident when the next one arrives.
				time.Sleep(60 * time.Millisecond)
				_, _ = fmt.Fprintf(conn, "{\"id\":%q,\"result\":{\"type\":\"pane_list\",\"panes\":[]}}\n", request["id"])
			}()
		}
	}()
	highWater := func() int {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
	return path, highWater, func() {
		_ = listener.Close()
		<-accepted
		active.Wait()
	}
}
