package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goWorkspace writes a single-file Go module and returns its dir.
func goWorkspace(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const nestedGoSrc = `package main

type Server struct {
	Name string
	addr string
}

func (s *Server) Start() error { return nil }

func Free() {}

func init() { _ = 1 }

func init() { _ = 2 }
`

func TestStructureNestedSymbolsDottedClassAndRange(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, nestedGoSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": "main.go", "depth": 2})
	if r.IsError {
		t.Fatalf("structure errored: %+v", r.Content)
	}
	var f wireEntry
	json.Unmarshal([]byte(r.Content[0].Text), &f)

	want := map[string]string{
		"Server":       "struct",
		"Server.Name":  "field",
		"Server.addr":  "field",
		"Server.Start": "method",
		"Free":         "func",
		"init[1]":      "func",
		"init[2]":      "func",
	}
	for sym, class := range want {
		got := findSym(f, sym)
		if got == nil {
			t.Errorf("missing symbol %q; have %+v", sym, f.Hash)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
		if len(got.At) != 2 || got.At[0] < 1 || got.At[1] < got.At[0] {
			t.Errorf("%q @ malformed: %+v", sym, got.At)
		}
	}
}

func TestStructureDepthIsDotCount(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, nestedGoSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// depth 1 = top-level only (no dots): Server present, members absent.
	r := s.callTool("structure", map[string]any{"path": "main.go", "depth": 1})
	var f wireEntry
	json.Unmarshal([]byte(r.Content[0].Text), &f)
	if findSym(f, "Server") == nil {
		t.Errorf("depth 1 should include top-level Server; have %+v", f.Hash)
	}
	if findSym(f, "Server.Name") != nil {
		t.Errorf("depth 1 should NOT include one-dot Server.Name; have %+v", f.Hash)
	}

	// depth 2 = one nesting level: Server.Name now present.
	r = s.callTool("structure", map[string]any{"path": "main.go", "depth": 2})
	json.Unmarshal([]byte(r.Content[0].Text), &f)
	if findSym(f, "Server.Name") == nil {
		t.Errorf("depth 2 should include Server.Name; have %+v", f.Hash)
	}
}

func TestNodeReadByNodeAddress(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, nestedGoSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_read", map[string]any{"node": "main.go#Server.Start"})
	if r.IsError {
		t.Fatalf("node_read by node errored: %+v", r.Content)
	}
	var p struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)
	if !strings.Contains(p.Text, "func (s *Server) Start() error") {
		t.Errorf("node address didn't resolve to Server.Start decl; got %q", p.Text)
	}
}

func TestNodeAddressDisambiguationRoundTrip(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, nestedGoSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	read := func(node string) string {
		r := s.callTool("node_read", map[string]any{"node": node})
		if r.IsError {
			t.Fatalf("node_read %s errored: %+v", node, r.Content)
		}
		var p struct {
			Text string `json:"text"`
		}
		json.Unmarshal([]byte(r.Content[0].Text), &p)
		return p.Text
	}
	if got := read("main.go#init[1]"); !strings.Contains(got, "_ = 1") {
		t.Errorf("init[1] should be the first init; got %q", got)
	}
	if got := read("main.go#init[2]"); !strings.Contains(got, "_ = 2") {
		t.Errorf("init[2] should be the second init; got %q", got)
	}
	// bare name == the first / only one.
	if got := read("main.go#init"); !strings.Contains(got, "_ = 1") {
		t.Errorf("bare init should equal init[1]; got %q", got)
	}
}

func TestNodeEditByNodeAddressReplacesDecl(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Free",
		"newText": "func Free() { _ = 0 }",
	})
	if r.IsError {
		t.Fatalf("node_edit by node errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Free() { _ = 0 }") {
		t.Errorf("node_edit didn't replace Free decl:\n%s", got)
	}
	// Sibling init funcs untouched.
	if !strings.Contains(string(got), "_ = 1") || !strings.Contains(string(got), "_ = 2") {
		t.Errorf("node_edit clobbered siblings:\n%s", got)
	}
}

func TestNodeAddressUnknownSymGuidedErrorNoWrite(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Server.Sffart",
		"newText": "// oops",
	})
	if !r.IsError {
		t.Fatalf("expected guided error for unknown node, got %+v", r.Content)
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "did you mean") {
		t.Errorf("error should guide with candidates; got %q", msg)
	}
	if !strings.Contains(msg, "Server.Start") {
		t.Errorf("candidate list should name the near sibling Server.Start; got %q", msg)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(before) != string(after) {
		t.Errorf("file was modified despite resolution failure")
	}
}
