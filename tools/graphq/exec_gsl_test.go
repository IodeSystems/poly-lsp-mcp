package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGSLFixture creates a tiny Go workspace:
//
//	package q
//	func Save() {}
//	func Caller() { Save() }
//	func Other()  { Save() }
//
// so the graph has Save with two callers.
func writeGSLFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n\nfunc Other() {\n\tSave()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// (b) GSL executor: fixture graph + known selector → known normalized set.
// This proves the in-process mcp.New + QueryJSON path with no model and no
// network — the load-bearing part of the harness.

func TestGSLRunQueryCallers(t *testing.T) {
	root := writeGSLFixture(t)
	srv, err := newGSLServer(root)
	if err != nil {
		t.Fatalf("newGSLServer: %v", err)
	}
	h := &harness{server: srv, limit: 100, edgeQueryCap: 100}

	// The call edges into Save: each row is a call SITE (a.go@N) whose from:
	// is the caller symbol.
	matches, se, err := h.runQuery(`#'a.go#Save'::in.call`)
	if se != nil {
		t.Fatalf("selector error: %v", se.Err)
	}
	if err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	// Collect the caller symbols (the from: far ends).
	callers := map[string]bool{}
	for _, m := range matches {
		for _, f := range m.From {
			callers[f] = true
		}
	}
	if !callers["a.go#Caller"] || !callers["a.go#Other"] {
		t.Fatalf("callers = %v, want a.go#Caller and a.go#Other", callers)
	}
}

func TestGSLRunQuerySymbol(t *testing.T) {
	root := writeGSLFixture(t)
	srv, err := newGSLServer(root)
	if err != nil {
		t.Fatalf("newGSLServer: %v", err)
	}
	h := &harness{server: srv, limit: 100, edgeQueryCap: 100}

	matches, se, err := h.runQuery(`#'a.go#Save'`)
	if se != nil {
		t.Fatalf("selector error: %v", se.Err)
	}
	if err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if len(matches) == 0 || matches[0].Node != "a.go#Save" {
		t.Fatalf("matches = %+v, want a.go#Save", matches)
	}
}

func TestGSLSelectorErrorTriggersRetry(t *testing.T) {
	root := writeGSLFixture(t)
	srv, err := newGSLServer(root)
	if err != nil {
		t.Fatalf("newGSLServer: %v", err)
	}
	h := &harness{server: srv, limit: 100, edgeQueryCap: 100}

	// A selector with an unknown pseudo-class is a SelectorError, not an
	// operational error — the caller should feed it back as the single retry.
	_, se, err := h.runQuery(`#Save:bogusclass`)
	if err != nil {
		t.Fatalf("expected a SelectorError, got operational: %v", err)
	}
	if se == nil {
		t.Fatal("expected a non-nil SelectorError for an unknown pseudo-class")
	}
}

// (rows) BuildRows derives nodes and edges from the same in-process server.
func TestBuildRowsFromFixture(t *testing.T) {
	root := writeGSLFixture(t)
	srv, err := newGSLServer(root)
	if err != nil {
		t.Fatalf("newGSLServer: %v", err)
	}
	h := &harness{server: srv, limit: 200, edgeQueryCap: 200}

	rows, err := h.BuildRows([]string{"Save"})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	// Nodes should include Save, Caller, Other.
	ids := map[string]bool{}
	for _, n := range rows.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"a.go#Save", "a.go#Caller", "a.go#Other"} {
		if !ids[want] {
			t.Errorf("nodes missing %s (have %v)", want, ids)
		}
	}
	// Edges should include the two call edges into Save.
	edgeSet := map[string]bool{}
	for _, e := range rows.Edges {
		edgeSet[e.From+"\x00"+e.To+"\x00"+e.Kind] = true
	}
	if !edgeSet["a.go#Caller\x00a.go#Save\x00call"] {
		t.Errorf("edges missing Caller->Save call (have %v)", edgeSet)
	}
	if !edgeSet["a.go#Other\x00a.go#Save\x00call"] {
		t.Errorf("edges missing Other->Save call (have %v)", edgeSet)
	}
}
