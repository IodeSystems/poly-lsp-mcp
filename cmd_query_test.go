package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQueryNoDaemonRunsInProcess: with a daemon present (state file, live
// pid, healthy socket), `query --no-daemon` skips the daemon and answers
// in-process. The daemon's /query endpoint is instrumented: if it is hit,
// the test fails.
func TestQueryNoDaemonRunsInProcess(t *testing.T) {
	// A workspace with one function.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package q\n\nfunc Seven() int { return 7 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.Abs(root)

	// A sandbox daemon home: the state file lives here.
	home := t.TempDir()
	t.Setenv(daemonHomeEnv, home)

	// A socket the state file names, with a server that answers /health
	// (so the probe would pass) and /query (with a marker the in-process
	// answer will not have).
	socket := filepath.Join(t.TempDir(), "d.sock")
	queried := false
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/query", func(w http.ResponseWriter, _ *http.Request) {
		queried = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":"DAEMON-ANSWER","root":""}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// State file: this process's pid (alive) and the socket.
	st := struct {
		PID    int    `json:"pid"`
		Socket string `json:"socket"`
	}{PID: os.Getpid(), Socket: socket}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "daemon.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Redirect os.Stdout: the query command writes its answer there.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	l := &loader{}
	cmd := queryCmd(l)
	cmd.SetArgs([]string{"--no-daemon", "--root", resolved, "#Seven"})
	runErr := cmd.Execute()

	w.Close()
	os.Stdout = old
	var out bytes.Buffer
	_, _ = out.ReadFrom(r)

	if runErr != nil {
		t.Fatalf("query --no-daemon: %v\nstdout: %s", runErr, out.String())
	}
	// The in-process answer names the function.
	if !strings.Contains(out.String(), "Seven") {
		t.Fatalf("in-process answer missing the match:\n%s", out.String())
	}
	// The daemon was NOT consulted.
	if queried {
		t.Fatal("query --no-daemon routed to the daemon")
	}
	if strings.Contains(out.String(), "DAEMON-ANSWER") {
		t.Fatal("answer came from the daemon, not in-process")
	}
}

// daemonHomeEnv is the env var that redirects daemon.ConfigHome.
const daemonHomeEnv = "POLY_LSP_HOME"
