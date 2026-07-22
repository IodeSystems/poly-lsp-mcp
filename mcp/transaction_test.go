package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

func txnDecode(t *testing.T, r toolResp) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	return m
}

// The staging MECHANICS without an LSP: commit:false stages to disk (so
// later edits see it), pending counts, and rollback:true reverts every
// touched file to the pre-batch state.
func TestEditBatchStageAndRollback(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	path := filepath.Join(dir, "main.go")
	orig, _ := os.ReadFile(path)

	// Stage two edits on the same file.
	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": "func Free(only int) {}", "newText": "func Free(only, extra int) {}",
		"commit": false,
	})
	m := txnDecode(t, r)
	if m["staged"] != true || m["pending"].(float64) != 1 {
		t.Fatalf("first stage: want staged pending=1; got %+v", m)
	}
	// Disk already holds the staged change (later edits resolve against it).
	if b, _ := os.ReadFile(path); string(b) == string(orig) {
		t.Fatal("a staged edit must be on disk so later edits/resolves see it")
	}

	r = s.callTool("node_edit", map[string]any{
		"node": "main.go#CallsStart", "oldText": "s := &Server{}", "newText": "s := &Server{Name: \"x\"}",
		"commit": false,
	})
	if m := txnDecode(t, r); m["pending"].(float64) != 2 {
		t.Fatalf("second stage: want pending=2; got %+v", m)
	}

	// rollback discards both, restoring the pre-batch bytes.
	r = s.callTool("node_edit", map[string]any{"rollback": true})
	m = txnDecode(t, r)
	if m["rolledBack"] != true || m["discarded"].(float64) != 2 {
		t.Fatalf("rollback: want rolledBack discarded=2; got %+v", m)
	}
	if b, _ := os.ReadFile(path); string(b) != string(orig) {
		t.Fatalf("rollback did not restore the file:\n got %q\nwant %q", b, orig)
	}

	// rollback with nothing open is a no-op, not an error.
	if m := txnDecode(t, s.callTool("node_edit", map[string]any{"rollback": true})); m["rolledBack"] != false {
		t.Errorf("rollback with no batch should report rolledBack:false; got %+v", m)
	}
}

// The atomic transaction against real gopls: two edits that are each a
// BROKEN intermediate on their own commit as one clean unit; and a lone
// breaking edit is rejected WITH the instructive help.
func TestEditBatchAtomicCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package x\n\nfunc F() int { return 1 }\n\nfunc Caller() int { return F() }\n"
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// 1) A lone signature change breaks Caller → rejected, reverted, WITH help.
	r := s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "func F() int", "newText": "func F(x int) int",
	})
	if !r.IsError {
		t.Fatalf("a breaking edit must be rejected (isError); got %+v", r.Content)
	}
	m := txnDecode(t, r)
	if m["rejected"] != true || m["help"] == nil {
		t.Fatalf("rejection must carry the instructive help; got %+v", m)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Fatal("rejected edit must revert the file")
	}

	// 2) The SAME change staged with its counterpart commits atomically.
	s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "func F() int", "newText": "func F(x int) int", "commit": false,
	})
	s.callTool("node_edit", map[string]any{
		"node": "a.go#Caller", "oldText": "return F()", "newText": "return F(1)", "commit": false,
	})
	r = s.callTool("node_edit", map[string]any{"commit": true}) // the noop commit
	m = txnDecode(t, r)
	if m["committed"] != true {
		t.Fatalf("the union is valid — the batch must commit; got %+v", m)
	}
	b, _ := os.ReadFile(path)
	if got := string(b); got == src || !containsAll(got, "func F(x int) int", "return F(1)") {
		t.Fatalf("both staged edits must be committed; got %q", got)
	}
}

// A SEMANTIC refactor (return-type change) staged alongside a ::body rewrite
// commits as one atomic unit — each is a broken intermediate alone (the body
// returns the wrong type / the sig expects the old type), the union is clean.
func TestEditBatchStagesSemanticRefactor(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package x\n\nfunc F() int {\n\treturn 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Stage the return-type refactor (semantic op — previously refused).
	r := s.callTool("node_edit", map[string]any{"node": "a.go#F", "return": "string", "commit": false})
	if m := txnDecode(t, r); m["staged"] != true {
		t.Fatalf("a params/return refactor must be stageable now; got %+v", m)
	}
	// Stage the body rewrite to match.
	s.callTool("node_edit", map[string]any{"node": `#'a.go#F'::body`, "newText": "\treturn \"hi\"", "commit": false})
	// Commit the union.
	r = s.callTool("node_edit", map[string]any{"commit": true})
	if m := txnDecode(t, r); m["committed"] != true {
		t.Fatalf("sig+body union is valid — must commit; got %+v", m)
	}
	b, _ := os.ReadFile(path)
	if got := string(b); !containsAll(got, "func F() string", `return "hi"`) {
		t.Fatalf("both the refactor and the body edit must land; got %q", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains1(s, sub) {
			return false
		}
	}
	return true
}

func contains1(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
