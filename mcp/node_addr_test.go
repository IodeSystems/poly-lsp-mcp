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
	s.srv.SetLegacyTools(true)
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
	s.srv.SetLegacyTools(true)
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

// TestNodeAddressDisambiguationRoundTrip pins the LEGACY surface's
// addressing, including its "bare name == the first one"
// normalization. That normalization is the silent-wrong-node bug the
// MODERN surface fixes by erroring instead — see
// TestModernBareAmbiguousAddressErrors. Both behaviors are intended,
// each on its own surface.
func TestNodeAddressDisambiguationRoundTrip(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, nestedGoSrc), nil, nil)
	defer s.close()
	s.srv.SetLegacyTools(true)
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

// A whole-node rewrite is just the ordinary oldText+newText modify with
// oldText set to the node's entire current text — there is deliberately
// no separate "replace the whole span" mode to reach for.
func TestNodeEditByNodeAddressRewritesWholeDecl(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Free",
		"oldText": "func Free() {}",
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

// Every edit restates what it actually did, in a sentence.
//
// From a live benchmark: asked to "write a file named plan.md", the model
// called node_edit{node:"plan"}. The result already said
// `"file":"plan","created":true` — it skimmed past both and reported success,
// losing the task. Structured fields are easy to not-read; "created plan" is a
// sentence you have to actively misread. This can't force the model to check,
// but it puts the wrong target where it costs nothing to notice.
func TestNodeEditNoteStatesWhatHappened(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	note := func(r toolResp) string {
		s.t.Helper()
		if r.IsError {
			t.Fatalf("unexpected error: %s", r.Content[0].Text)
		}
		var p struct {
			Note string `json:"note"`
		}
		json.Unmarshal([]byte(r.Content[0].Text), &p)
		return p.Note
	}

	// create — the case that lost the task. The note names the target it
	// ACTUALLY wrote, so "plan" vs "plan.md" is legible on the spot.
	got := note(s.callTool("node_edit", map[string]any{
		"node": "plan", "newText": "# Plan\n",
	}))
	if got != "created plan (+7)" {
		t.Errorf("create note = %q, want %q", got, "created plan (+7)")
	}

	// update a symbol — names the range, not the whole file, because a range
	// rewrite didn't touch the whole file.
	got = note(s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": "func Free() {}", "newText": "func Free() { _ = 0 }",
	}))
	if !strings.HasPrefix(got, "updated main.go:") || !strings.Contains(got, "-14 +21") {
		t.Errorf("update note = %q, want \"updated main.go:<lines>, -14 +21\"", got)
	}

	// delete
	got = note(s.callTool("node_edit", map[string]any{"node": "plan", "delete": true}))
	if got != "deleted plan (-7)" {
		t.Errorf("delete note = %q, want %q", got, "deleted plan (-7)")
	}
}

// newText WITHOUT oldText means "create". Against a node that already
// resolves it must error rather than replace, because that shape is
// indistinguishable from a stale read: the caller is telling us what
// the node should be without telling us what they think it currently
// is, so we cannot detect an intervening out-of-band write.
func TestNodeEditNewTextAloneRefusesToClobberExisting(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Free",
		"newText": "func Free() { _ = 0 }",
	})
	if !r.IsError {
		t.Fatalf("newText alone silently replaced an existing node; want an error")
	}
	if got := r.Content[0].Text; !strings.Contains(got, "supply oldText") {
		t.Errorf("error should point at oldText, got: %s", got)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Free() {}") {
		t.Errorf("refused edit still wrote to disk:\n%s", got)
	}
}

// The stale-read case the CAS exists for: oldText describes a version of
// the node that is no longer on disk, so the edit must fail loudly
// instead of overwriting whatever landed in between.
func TestNodeEditStaleOldTextErrorsAndShowsCurrentText(t *testing.T) {
	dir := goWorkspace(t, nestedGoSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Free",
		"oldText": "func Free() { _ = 99 }", // never existed
		"newText": "func Free() { _ = 0 }",
	})
	if !r.IsError {
		t.Fatalf("stale oldText was accepted; want an error")
	}
	// The error carries the node's real current text so the retry costs
	// one turn rather than an extra node_read round trip.
	if got := r.Content[0].Text; !strings.Contains(got, "func Free() {}") {
		t.Errorf("stale-read error should show current text, got: %s", got)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Free() {}") {
		t.Errorf("failed CAS still wrote to disk:\n%s", got)
	}
}

// Ambiguity is an error everywhere in this surface, oldText included.
func TestNodeEditAmbiguousOldTextErrorsWithOccurrences(t *testing.T) {
	dir := goWorkspace(t, "package main\n\nfunc Dup() {\n\tx := 1\n\tx = 1\n}\n")
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node":    "main.go#Dup",
		"oldText": "1",
		"newText": "2",
	})
	if !r.IsError {
		t.Fatalf("ambiguous oldText was applied; want an error")
	}
	if got := r.Content[0].Text; !strings.Contains(got, "occurs 2 times") {
		t.Errorf("want an occurrence count, got: %s", got)
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
