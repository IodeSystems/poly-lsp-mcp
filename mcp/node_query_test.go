package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// queryFixture writes a small multi-file Go module and returns its dir.
func queryFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	write("server.go", `package main

import "net/http"

type Server struct {
	Name   string
	addr   string
	UserID int
}

func (s *Server) Start() error { return nil }

func (s *Server) Stop() error { return nil }

var _ = http.StatusOK
`)
	write("user.go", `package main

type UserStore struct {
	backing string
}

func (u *UserStore) Start() {}

func NewUser() *UserStore { return &UserStore{} }

func TestUserRoundTrip() {}

func TestUserCreate() {}
`)
	return dir
}

// querySession boots an initialized MCP session over the fixture.
func querySession(t *testing.T, dir string) *mcpSession {
	t.Helper()
	s := startSessionFull(t, dir, nil, nil)
	// These tests exercise the LEGACY node_query tool (select= field,
	// grouped-by-file output) — the modern node_query has a different
	// name-shared tool with a different schema/output shape.
	s.srv.SetLegacyTools(true)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

// runQuery calls node_query and decodes the grouped shape.
func runQuery(t *testing.T, s *mcpSession, args map[string]any) []wireEntry {
	t.Helper()
	r := s.callTool("node_query", args)
	if r.IsError {
		t.Fatalf("node_query %v errored: %+v", args, r.Content)
	}
	var out struct {
		Matches []wireEntry `json:"matches"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &out); err != nil {
		t.Fatalf("decode node_query result: %v", err)
	}
	return out.Matches
}

// collectSyms returns "<basefile>#<sym>" for every match, for easy asserts.
func collectSyms(matches []wireEntry) map[string]bool {
	got := map[string]bool{}
	for _, m := range matches {
		for _, h := range m.Hash {
			got[filepath.Base(m.File)+"#"+h.Sym] = true
		}
	}
	return got
}

func TestNodeQueryTypeAndName(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	// method[name=Start] finds both Start methods across files.
	got := collectSyms(runQuery(t, s, map[string]any{"select": "method[name=Start]"}))
	if !got["server.go#Server.Start"] || !got["user.go#UserStore.Start"] {
		t.Errorf("method[name=Start] missing a method: %v", got)
	}
	if got["server.go#Server.Stop"] {
		t.Errorf("method[name=Start] should not match Stop: %v", got)
	}
	// Functions named Start should NOT appear (NewUser is a func, not method).
	if got["user.go#NewUser"] {
		t.Errorf("method selector matched a func: %v", got)
	}
}

func TestNodeQueryChildCombinator(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	got := collectSyms(runQuery(t, s, map[string]any{"select": "struct[name=Server] > field"}))
	// Direct fields of Server only.
	for _, want := range []string{"server.go#Server.Name", "server.go#Server.addr", "server.go#Server.UserID"} {
		if !got[want] {
			t.Errorf("child combinator missing %q: %v", want, got)
		}
	}
	// Fields of a DIFFERENT struct must not leak in.
	if got["user.go#UserStore.backing"] {
		t.Errorf("child combinator leaked another struct's field: %v", got)
	}
	// The struct itself is not a field.
	if got["server.go#Server"] {
		t.Errorf("child combinator returned the parent: %v", got)
	}
}

func TestNodeQueryPrefix(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	got := collectSyms(runQuery(t, s, map[string]any{"select": "func[name^=Test]"}))
	if !got["user.go#TestUserRoundTrip"] || !got["user.go#TestUserCreate"] {
		t.Errorf("func[name^=Test] missing a test func: %v", got)
	}
	if got["user.go#NewUser"] {
		t.Errorf("prefix selector matched NewUser: %v", got)
	}
}

func TestNodeQueryContainsAcrossClasses(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	got := collectSyms(runQuery(t, s, map[string]any{"select": "*[name*=User]"}))
	// Contains "User" across different classes: struct UserStore, field
	// UserID, funcs NewUser / TestUser*.
	for _, want := range []string{
		"server.go#Server.UserID", "user.go#UserStore",
		"user.go#NewUser", "user.go#TestUserRoundTrip", "user.go#TestUserCreate",
	} {
		if !got[want] {
			t.Errorf("*[name*=User] missing %q: %v", want, got)
		}
	}
}

func TestNodeQueryUnion(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	got := collectSyms(runQuery(t, s, map[string]any{"select": "import, const"}))
	if !got["server.go#http"] {
		t.Errorf("union should include the http import: %v", got)
	}
}

func TestNodeQueryHasDescendantSymbol(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	// struct that DECLARES a field named UserID → Server only.
	got := collectSyms(runQuery(t, s, map[string]any{"select": "struct:has(field[name=UserID])"}))
	if !got["server.go#Server"] {
		t.Errorf(":has should match Server (declares UserID): %v", got)
	}
	if got["user.go#UserStore"] {
		t.Errorf(":has matched UserStore, which has no UserID field: %v", got)
	}
}

func TestNodeQueryMalformedGuidedError(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	r := s.callTool("node_query", map[string]any{"select": "type[name=]] >>"})
	if !r.IsError {
		t.Fatalf("expected guided error for malformed selector, got %+v", r.Content)
	}
	// Also assert a clearly-broken attribute op errors, not panics.
	r2 := s.callTool("node_query", map[string]any{"select": "field[name~=x]"})
	if !r2.IsError {
		t.Fatalf("expected guided error for bad operator, got %+v", r2.Content)
	}
}

func TestNodeQueryGroupedShapeAndLimit(t *testing.T) {
	s := querySession(t, queryFixture(t))
	defer s.close()

	// Grouped shape: results grouped by file, each with a `#` list.
	matches := runQuery(t, s, map[string]any{"select": "method"})
	if len(matches) == 0 {
		t.Fatal("expected method matches")
	}
	for _, m := range matches {
		if m.File == "" || len(m.Hash) == 0 {
			t.Errorf("malformed group: %+v", m)
		}
	}

	// limit respected.
	r := s.callTool("node_query", map[string]any{"select": "*", "limit": 2})
	if r.IsError {
		t.Fatalf("limited query errored: %+v", r.Content)
	}
	var out struct {
		Matches      []wireEntry `json:"matches"`
		TotalMatches int         `json:"totalMatches"`
		Truncated    bool        `json:"truncated"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &out)
	if out.TotalMatches != 2 {
		t.Errorf("limit=2 should cap totalMatches at 2, got %d", out.TotalMatches)
	}
	if !out.Truncated {
		t.Errorf("limit=2 over a larger set should set truncated")
	}
}
