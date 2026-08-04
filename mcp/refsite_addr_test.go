package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A ref-site address is "<file>@<line>" (or "<file>@<start>-<end>") — the
// address ::grep, edge and ::signature/::body rows hand back, and the one
// the tool description tells the model to feed straight into node_read /
// node_edit. It has no dotted sym, which is what made it collide with the
// "sym == \"\" means the WHOLE FILE" test: node_read returned page 1 of the
// file instead of the addressed line, an oldText edit matched the first
// occurrence anywhere in the file, and delete:true removed the FILE. These
// pin the address form end to end so that predicate cannot regress.

// readNode calls node_read and decodes the payload.
func readNode(t *testing.T, s *mcpSession, args map[string]any) (map[string]any, bool) {
	t.Helper()
	r := s.callTool("node_read", args)
	var m map[string]any
	if len(r.Content) > 0 {
		_ = json.Unmarshal([]byte(r.Content[0].Text), &m)
	}
	return m, r.IsError
}

// The address a ::grep row reports must read back as THAT line. Round-trips
// through node_query so the test breaks if either half drifts.
func TestRefSiteAddrReadsTheAddressedLine(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `path=main.go ::grep('fmt.Println')`})
	if len(q.Matches) != 1 || q.Matches[0].Node != "main.go@10" {
		t.Fatalf("::grep should address the hit line as main.go@10; got %+v", q.Matches)
	}

	m, isErr := readNode(t, s, map[string]any{"node": q.Matches[0].Node})
	if isErr {
		t.Fatalf("node_read %s errored: %+v", q.Matches[0].Node, m)
	}
	if got := m["text"]; got != "\tfmt.Println(ctx)" {
		t.Errorf("node_read of a ref site returns that line's text; got %q", got)
	}
	if got := m["node"]; got != "main.go@10" {
		t.Errorf("the payload echoes the ref-site address; got %v", got)
	}
	at, _ := m["@"].([]any)
	if len(at) != 2 || at[0].(float64) != 10 || at[1].(float64) != 10 {
		t.Errorf("@ should be [10,10]; got %v", m["@"])
	}
	// The regression shape: a whole-file BROWSE payload, which carries
	// startLine/totalLines and begins at line 1.
	if _, browsed := m["totalLines"]; browsed {
		t.Errorf("a ref site is an addressed read, not a whole-file browse; got %+v", m)
	}
}

// The span form addresses a range, not just its first line.
func TestRefSiteAddrReadsASpan(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	m, isErr := readNode(t, s, map[string]any{"node": "main.go@10-11"})
	if isErr {
		t.Fatalf("node_read main.go@10-11 errored: %+v", m)
	}
	want := "\tfmt.Println(ctx)\n\treturn nil"
	if got := m["text"]; got != want {
		t.Errorf("span read should cover both lines; got %q want %q", got, want)
	}
}

// startLine/lineLimit browse a whole FILE. On an addressed line they are a
// category error, and the message must name what was actually addressed —
// "a symbol" was wrong for a ref site and sent the model looking for one.
func TestRefSiteAddrRejectsLineWindowArgs(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "main.go@10", "startLine": 1})
	if !r.IsError {
		t.Fatal("startLine on a ref-site address should be refused")
	}
	if msg := r.Content[0].Text; !strings.Contains(msg, "source line/span") {
		t.Errorf("the error should say the address is a line/span, not a symbol; got %q", msg)
	}
}

// The most-repeated error in a measured session (3 of 7) was startLine on a
// large symbol: the caller wants PART of something big, and "drop them" only
// says how to stop being wrong — the observed recovery was reading a 500-line
// method whole, or paging a file window. Name the form that answers it.
func TestWholeNodeReadErrorOffersTheSpanForm(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "main.go#Server.Start", "startLine": 10})
	if !r.IsError {
		t.Fatal("startLine on a symbol should still be refused")
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "SPAN address") {
		t.Errorf("the error should name the span form; got %q", msg)
	}
	// Runnable as printed, and it must be THIS node's span.
	if !strings.Contains(msg, `"main.go@9-12"`) {
		t.Errorf("the span should be the addressed node's own lines; got %q", msg)
	}
	m, isErr := readNode(t, s, map[string]any{"node": "main.go@9-12"})
	if isErr {
		t.Fatalf("the suggested address must work as printed: %+v", m)
	}
	if txt, _ := m["text"].(string); !strings.Contains(txt, "func (s *Server) Start") {
		t.Errorf("it should read the lines it named; got %q", txt)
	}
}

// An oldText edit is scoped to the ADDRESSED LINE. "init" appears on both
// line 16 and line 18: whole-file scoping saw two occurrences (a spurious
// ambiguity error, or worse, a hit on the wrong line).
func TestRefSiteAddrEditIsScopedToThatLine(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "main.go@18", "oldText": "init", "newText": "initTwo",
	})
	if r.IsError {
		t.Fatalf("edit of main.go@18 errored: %s", r.Content[0].Text)
	}

	got, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(got), "\n")
	if lines[17] != "func initTwo() { _ = 2 }" {
		t.Errorf("line 18 should be edited; got %q", lines[17])
	}
	if lines[15] != "func init() { _ = 1 }" {
		t.Errorf("line 16 holds the earlier \"init\" and must be untouched; got %q", lines[15])
	}
}

// delete:true on a ref site empties THAT LINE. It used to reach
// applyWholeFileDelete and remove main.go outright — the one silent
// data-loss path in the address form.
func TestRefSiteAddrDeleteEmptiesTheLineNotTheFile(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{"node": "main.go@10", "delete": true})
	if r.IsError {
		t.Fatalf("delete of main.go@10 errored: %s", r.Content[0].Text)
	}

	got, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("main.go must still exist after deleting one of its lines: %v", err)
	}
	lines := strings.Split(string(got), "\n")
	// The newline sits outside the span, so the line empties in place
	// rather than closing up — same splice as every other span delete.
	if lines[9] != "" {
		t.Errorf("line 10 should be empty; got %q", lines[9])
	}
	if lines[8] != "func (s *Server) Start(ctx string, retries int) error {" || lines[10] != "\treturn nil" {
		t.Errorf("neighbours must survive; got %q / %q", lines[8], lines[10])
	}
}

// A whole-file address still means the whole file — the predicate change
// must not narrow the browse/write/delete paths it was guarding.
func TestWholeFileAddrStillBrowsesAndDeletes(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	m, isErr := readNode(t, s, map[string]any{"node": "main.go", "startLine": 5, "lineLimit": 3})
	if isErr {
		t.Fatalf("whole-file browse errored: %+v", m)
	}
	if m["startLine"].(float64) != 5 || m["endLine"].(float64) != 7 {
		t.Errorf("a file address still browses by line window; got %+v", m)
	}

	if r := s.callTool("node_edit", map[string]any{"node": "notes.md", "delete": true}); r.IsError {
		t.Fatalf("whole-file delete errored: %s", r.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Errorf("a file address with delete:true removes the file; stat err=%v", err)
	}
}
