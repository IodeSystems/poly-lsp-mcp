package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// indexNote returns the data of the bring-up notification, once it has
// arrived (a tool call has returned, so it has).
func indexNote(t *testing.T, s *mcpSession) map[string]any {
	t.Helper()
	return loggerNote(t, s, indexLogger)
}

func loggerNote(t *testing.T, s *mcpSession, logger string) map[string]any {
	t.Helper()
	notes := s.serverNotes("notifications/message")
	for _, n := range notes {
		var p struct {
			Logger string         `json:"logger"`
			Data   map[string]any `json:"data"`
		}
		if json.Unmarshal(n.Params, &p) == nil && p.Logger == logger {
			return p.Data
		}
	}
	t.Fatalf("no %s notification among %d", logger, len(notes))
	return nil
}

// initialize replies before the index exists; the first tool call waits for
// it; the notification that ends the bring-up carries the cost report.
func TestAsyncInitializeRepliesBeforeTheIndex(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.go": "package p\n\nfunc F() {}\n"})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetAsyncInit(true) })
	defer s.close()

	resp := s.request("initialize", map[string]any{})
	var got struct {
		Index map[string]any `json:"index"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Index["state"] != "building" {
		t.Errorf("initialize should say the index is building: %s", resp.Result)
	}
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "#F"})
	if r.IsError || len(r.Content) == 0 || !strings.Contains(r.Content[0].Text, "F") {
		t.Fatalf("the first tool call should wait for the index and answer: %+v", r)
	}
	note := indexNote(t, s)
	if note["state"] != "ready" || note["index"] == nil {
		t.Errorf("bring-up notification: %v", note)
	}
}

// Over budget: the notification and every tool call carry the trace and
// both remedies — get under budget, or raise it — so the client can retry.
func TestIndexBudgetFailsWithTrace(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.go":            "package p\n",
		"vendored/x/b.go": "package x\n",
		"vendored/x/c.go": "package x\n",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) {
		srv.SetAsyncInit(true)
		if err := srv.SetIndexBudget("1ops"); err != nil {
			t.Fatal(err)
		}
	})
	defer s.close()
	s.request("initialize", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "file"})
	if !r.IsError {
		t.Fatalf("a tool call over the limit should be refused: %+v", r)
	}
	text := r.Content[0].Text
	for _, want := range []string{"index budget 1ops spent", "vendored/x/", "--exclude", "--index-budget 4ops"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal should contain %q:\n%s", want, text)
		}
	}
	// On the pure-Go parser the fast build is part of the remedy; on cgo it
	// would be noise.
	if hinted := strings.Contains(text, ts.FastPathHint); hinted != (ts.Runtime() == "pure-go") {
		t.Errorf("fast-path hint present = %v on runtime %s:\n%s", hinted, ts.Runtime(), text)
	}
	note := indexNote(t, s)
	if note["state"] != "failed" || note["budget"] != "1ops" || note["read"] != float64(1) || note["remedy"] == nil {
		t.Errorf("failure notification: %v", note)
	}
}

// --exclude drops a directory from the index without touching .gitignore.
func TestExcludesDropDirectories(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.go":            "package p\n\nfunc Keep() {}\n",
		"vendored/x/b.go": "package x\n\nfunc Drop() {}\n",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetExcludes([]string{"vendored/"}) })
	defer s.close()
	s.request("initialize", map[string]any{})
	if n := s.srv.IndexCost().Files; n != 1 {
		t.Errorf("indexed %d files, want 1 (vendored/ excluded)", n)
	}
	r := s.callTool("node_query", map[string]any{"selector": "#Drop"})
	if len(r.Content) > 0 && strings.Contains(r.Content[0].Text, "vendored") {
		t.Errorf("an excluded directory answered a query: %s", r.Content[0].Text)
	}
}

// After bring-up the server says which languages the workspace holds and,
// for a missing child LSP, what it would add and how to install it — the
// hint a client relays to the user.
func TestSetupNotificationNamesMissingLSP(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	t.Setenv("PATH", t.TempDir()) // no gopls
	// The fallback dirs (internal/lspbin) must not find a real gopls either.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PNPM_HOME", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetAsyncInit(true) })
	defer s.close()
	s.request("initialize", map[string]any{})
	s.callTool("node_query", map[string]any{"selector": "#A"})

	note := loggerNote(t, s, setupLogger)
	langs, _ := note["languages"].([]any)
	if note["missing"] != float64(1) || len(langs) != 1 {
		t.Fatalf("setup notification: %v", note)
	}
	g := langs[0].(map[string]any)
	install, _ := g["install"].(map[string]any)
	if g["lang"] != "go" || g["adds"] == nil || install == nil || install["cmd"] != "go install golang.org/x/tools/gopls@latest" {
		t.Errorf("go entry should carry adds and install: %v", g)
	}
}
