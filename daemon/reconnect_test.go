package daemon

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dyingDaemon is a daemon that goes away mid-call: it answers the
// handshake, the open and the event stream, and drops the connection of
// every /call — what a proxy sees when its daemon hands over or dies
// while a call is in flight.
func dyingDaemon(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"root":"/x","names":1}`))
	})
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Hello{Build: BuildID(), Protocol: Protocol})
	})
	mux.HandleFunc("/session/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(socket), calls
}

// A read-only call cut by the daemon going away is sent once more, to the
// daemon the proxy reconnects to. An edit is not: it may have applied, so
// the agent is told to re-read and retry.
func TestProxyRetriesOnlyReadOnlyCallsAcrossALostDaemon(t *testing.T) {
	root, resolved := goWorkspace(t)
	good, stop := startTestDaemon(t, resolved)
	defer stop()

	for _, tc := range []struct {
		tool, args string
		retried    bool
	}{
		{"node_query", `{"selector":"#F2"}`, true},
		{"node_edit", `{"node":"f2.go#F2","oldText":"return 2","newText":"return 22"}`, false},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			bad, calls := dyingDaemon(t)
			var n atomic.Int32
			connect := func() (*Client, error) {
				if n.Add(1) == 1 {
					return bad, nil
				}
				return NewClient(good.socket), nil
			}
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			done := make(chan error, 1)
			go func() { done <- RunProxy(connect, root, OpenOptions{}, inR, outW); _ = outW.Close() }()
			h := newHarness(t, inW, outR)
			defer func() { h.request("shutdown", nil); _ = inW.Close(); <-done }()
			h.reply(h.request("initialize", map[string]any{}), 5*time.Second)

			var args map[string]any
			_ = json.Unmarshal([]byte(tc.args), &args)
			text, isErr := callText(t, h.reply(h.request("tools/call", map[string]any{"name": tc.tool, "arguments": args}), 30*time.Second))
			if calls.Load() != 1 {
				t.Fatalf("the dying daemon got %d calls", calls.Load())
			}
			if tc.retried {
				if isErr || !strings.Contains(text, "F2") {
					t.Fatalf("read-only call not retried on the new daemon: isErr=%v %q", isErr, text)
				}
				return
			}
			if !isErr || !strings.Contains(text, "may or may not have been applied") || !strings.Contains(text, "upgrading") {
				t.Fatalf("cut edit: isErr=%v %q", isErr, text)
			}
			if b, _ := os.ReadFile(filepath.Join(root, "f2.go")); strings.Contains(string(b), "22") {
				t.Fatal("the cut edit was sent again")
			}
			// The proxy reconnected: the next call is served.
			text, isErr = callText(t, h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F2"}}), 30*time.Second))
			if isErr || !strings.Contains(text, "F2") {
				t.Fatalf("after the reconnect: isErr=%v %q", isErr, text)
			}
		})
	}
}
