package mcp

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI resolves no edges (no child LSP), so any ::in/::out answer is
// lexical — and the tree renders far ends without a conf column. A
// caveat must fire on edge selectors so a name-keyed guess isn't read as
// a resolved fact, and must NOT fire on plain containment queries (where
// there's nothing lexical to warn about).
func TestQueryTextLexicalEdgeCaveat(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	// An edge selector — with matches — carries the caveat.
	if out := run(`#'Server'::in.type`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("edge query must carry the lexical caveat; got:\n%s", out)
	}
	// The traversal case (`> *` lands on symbols, no ref row in output)
	// still crossed a lexical edge — the caveat must still fire.
	if out := run(`#'Server'::in.return.type > *`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("edge traversal must carry the caveat even when rows are symbols; got:\n%s", out)
	}
	// A plain containment query crossed no edge — no caveat.
	if out := run(`file.go func`); strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("containment query must NOT carry the edge caveat; got:\n%s", out)
	}
	// An empty edge answer is a lexical answer too — caveat still fires.
	if out := run(`#'Nonexistent'::in.call`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("empty edge answer must still carry the caveat; got:\n%s", out)
	}
}

// The zero-result hint was JSON-only, so `poly-lsp-mcp query` answered a dead
// selector with a bare "no matches" — the exact silence the hint exists to
// break, preserved for the one caller who cannot see the payload. An agent was
// told which clause emptied its selector; a person debugging the same selector
// by hand was not.
//
// The case is the one measured in a dun dogfood run: the caller guessed the
// node TYPE (`method` for a plain `func`), which returns a result
// byte-identical to the symbol not existing.
func TestQueryTextRendersTheZeroResultHint(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	// Wrong TAG on a name that exists under another one: the hint must name it.
	out := run(`method[name~=build]`)
	if !strings.Contains(out, "no matches") {
		t.Fatalf("expected an empty answer; got:\n%s", out)
	}
	if !strings.Contains(out, "TAG") {
		t.Errorf("a zero-result CLI answer must say which clause emptied it; got:\n%s", out)
	}

	// A selector with matches carries no hint — it is only for emptiness.
	if out := run(`func`); strings.Contains(out, "emptied it") {
		t.Errorf("a query WITH matches must not carry a zero-result hint; got:\n%s", out)
	}
}

// A selector error is an ANSWER, not a log event, so the CLI must be able to
// tell it apart from a tool failure and print it without the log furniture.
//
// It shipped through log.Fatalf, which stamped the tool's most carefully
// written output with a program name and a microsecond timestamp:
//
//	poly-lsp-mcp 17:39:39.411083 query: unknown pseudo-class ":arg" — did you…
//
// That framing says "diagnostics, scroll past" about the one line that
// answers the question, and it pushes the useful clause past the wrap column
// of a narrow terminal. Hunting for exactly this message, I filtered it out as
// log noise twice running and reported that the command printed nothing.
func TestQueryTextMarksCallerFixableErrors(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))
	var b strings.Builder

	var se *SelectorError
	err := s.QueryText("func:arg", 0, 0, "", &b)
	if !errors.As(err, &se) {
		t.Fatalf("a bad pseudo-class is the caller's to fix, got %T: %v", err, err)
	}
	// The message itself must survive the wrapping intact — the whole point
	// is the guidance, not the classification.
	if !strings.Contains(se.Error(), ":arity") {
		t.Errorf("the suggestion was lost in the wrapping: %q", se.Error())
	}
	// An empty selector and a bad offset are equally the caller's to fix.
	if err := s.QueryText("", 0, 0, "", &b); !errors.As(err, &se) {
		t.Errorf("an empty selector is caller-fixable, got %T", err)
	}
	if err := s.QueryText("func", 0, -1, "", &b); !errors.As(err, &se) {
		t.Errorf("a negative offset is caller-fixable, got %T", err)
	}
}

// F3: a declaration block `{ name; path; line; signature }` at the end of a
// selector switches the renderer from the grouped tree to a flat TSV
// projection: one row per match carrying ONLY the named fields. A match
// missing a field leaves that cell empty rather than failing the query.
func TestQueryTextProjectionTSV(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var b strings.Builder
	if err := s.QueryText(`#Save { name; path; line }`, 0, 0, "", &b); err != nil {
		t.Fatalf("projection query failed: %v", err)
	}
	out := b.String()

	// Header line: the field names tab-separated. There is a "read as"
	// reading above it, so the TSV starts after that.
	if !strings.Contains(out, "name\tpath\tline\n") {
		t.Fatalf("TSV header must be the field names; got:\n%s", out)
	}
	// One data row for the Save func: name=Save, path=a.go, line=3.
	if !strings.Contains(out, "Save\ta.go\t3\n") {
		t.Errorf("expected a row with Save, a.go, 3; got:\n%s", out)
	}
}

// F3: a field the match does not carry (e.g. `signature` on a bare `func`
// with no generated text, or `conf` on a non-ref node) renders as an empty
// cell, not an error.
func TestQueryTextProjectionMissingFieldIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	// `conf` is an edge field; a plain node match has no conf → empty cell.
	var b strings.Builder
	if err := s.QueryText(`#Save { name; conf }`, 0, 0, "", &b); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	out := b.String()
	// Header then a row where the conf cell is empty (trailing tab + newline).
	if !strings.Contains(out, "Save\t\n") {
		t.Errorf("missing field must be an empty cell, not an error; got:\n%q", out)
	}
}

// F3: `{ count }` collapses all matches to a single row with the match count.
func TestQueryTextAggregateCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var b strings.Builder
	if err := s.QueryText(`func { count }`, 0, 0, "", &b); err != nil {
		t.Fatalf("aggregate query failed: %v", err)
	}
	out := b.String()
	// Header is "count", then the value "3".
	if !strings.Contains(out, "count\n3\n") {
		t.Errorf("expected a count row with value 3; got:\n%s", out)
	}
}

// F3: `{ max(length) }` and `{ min(length) }` over a match set that carries
// no path capture (no `:path`) leave the value empty.
func TestQueryTextAggregateLengthNoPathIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc A() {}\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var b strings.Builder
	if err := s.QueryText(`func { max(length) }`, 0, 0, "", &b); err != nil {
		t.Fatalf("aggregate query failed: %v", err)
	}
	out := b.String()
	// Header "max(length)" then an empty value line.
	if !strings.Contains(out, "max(length)\n\n") {
		t.Errorf("max(length) with no path capture must be empty; got:\n%q", out)
	}

	b.Reset()
	if err := s.QueryText(`func { min(length) }`, 0, 0, "", &b); err != nil {
		t.Fatalf("aggregate query failed: %v", err)
	}
	out = b.String()
	if !strings.Contains(out, "min(length)\n\n") {
		t.Errorf("min(length) with no path capture must be empty; got:\n%q", out)
	}
}

// F3: an edge field set `{ site; conf; kind }` on a ref row carries only
// those edge fields. A call reference to Save has a site (file@line), a
// kind ("call"), and a conf (may be empty in the no-LSP CLI path) — the
// projection must name exactly those three columns and resolve each one.
func TestQueryTextProjectionEdgeFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var b strings.Builder
	if err := s.QueryText(`#Save::in.call { site; conf; kind }`, 0, 0, "", &b); err != nil {
		t.Fatalf("edge projection query failed: %v", err)
	}
	out := b.String()

	// Header: the three edge field names, tab-separated, in order.
	if !strings.Contains(out, "site\tconf\tkind\n") {
		t.Fatalf("TSV header must be site, conf, kind; got:\n%s", out)
	}
	// A data row: the site is the file@line of the call, kind is "call".
	// In the no-LSP CLI path conf is "lexical" — the row is
	// "a.go@6\tlexical\tcall".
	if !strings.Contains(out, "a.go@6\tlexical\tcall\n") {
		t.Errorf("expected a row with site=a.go@6, conf=lexical, kind=call; got:\n%s", out)
	}
}

// F3: `{ max(length) }` and `{ min(length) }` over a match set that carries
// real path data (a :path walk) return the extreme path length. A→B→C→D
// gives lengths 1, 2, 3 for B, C, D respectively — max is 3, min is 1.
func TestQueryTextAggregateLengthWithPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc A() { B() }\nfunc B() { C() }\nfunc C() { D() }\nfunc D() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	// max(length): the deepest match (D) has a 3-edge chain.
	var b strings.Builder
	if err := s.QueryText(`#A::out.call{1,}:path > * { max(length) }`, 0, 0, "", &b); err != nil {
		t.Fatalf("max(length) query failed: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "max(length)\n3\n") {
		t.Errorf("max(length) over A→B→C→D must be 3; got:\n%s", out)
	}

	// min(length): the shallowest match (B) has a 1-edge chain.
	b.Reset()
	if err := s.QueryText(`#A::out.call{1,}:path > * { min(length) }`, 0, 0, "", &b); err != nil {
		t.Fatalf("min(length) query failed: %v", err)
	}
	out = b.String()
	if !strings.Contains(out, "min(length)\n1\n") {
		t.Errorf("min(length) over A→B→C→D must be 1; got:\n%s", out)
	}
}

// The CLI answers on stdout and stays silent on stderr unless --verbose; see
// the "answers quietly" fix. Loading the parse cache re-introduced exactly
// the bring-up line that removed, because maybeLoadCache used log.Printf
// rather than the server's quiet-aware logf — and because LoadCache ran
// before SetQuiet.
func TestCacheLoadRespectsQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package q\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache.gob")

	// Write a cache, then load it back with quiet on.
	warm := newQueryServer(t, dir)
	warm.SetCachePath(cache)
	warm.maybeSaveCache()

	var logged bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(old)

	s := newQueryServer(t, dir)
	s.SetQuiet(true)
	s.SetCachePath(cache)
	s.LoadCache()
	if strings.Contains(logged.String(), "loaded") {
		t.Errorf("a quiet server must not narrate its cache load; got %q", logged.String())
	}

	logged.Reset()
	v := newQueryServer(t, dir)
	v.SetQuiet(false)
	v.SetCachePath(cache)
	v.LoadCache()
	if !strings.Contains(logged.String(), "loaded") {
		t.Errorf("--verbose should still report it; got %q", logged.String())
	}
}

// TestQueryHelpDocumentsNewForms proves the "?" reference output documents
// the three F3 forms: the declaration block, the :path repetition modifier,
// and the three aggregates (count, max(length), min(length)).
func TestQueryHelpDocumentsNewForms(t *testing.T) {
	dir := t.TempDir()
	// Minimal fixture: a package main so the indexer has something.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Save() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)

	var b bytes.Buffer
	if err := s.QueryText("?", 0, 0, "", &b); err != nil {
		t.Fatalf("QueryText(\"?\") failed: %v", err)
	}
	out := b.String()

	// Declaration block: the help must mention the { ... } projection form.
	if !strings.Contains(out, "PROJECTION & AGGREGATES") {
		t.Errorf("? reference missing the PROJECTION & AGGREGATES section")
	}
	if !strings.Contains(out, "declaration block") {
		t.Errorf("? reference missing mention of declaration block")
	}

	// :path repetition modifier: must document {m,n}:path.
	if !strings.Contains(out, ":path") {
		t.Errorf("? reference missing :path documentation")
	}

	// Aggregates: count, max(length), min(length).
	if !strings.Contains(out, "AGGREGATES") {
		t.Errorf("? reference missing AGGREGATES section")
	}
	if !strings.Contains(out, "count") {
		t.Errorf("? reference missing count aggregate")
	}
	if !strings.Contains(out, "max(length)") {
		t.Errorf("? reference missing max(length) aggregate")
	}
	if !strings.Contains(out, "min(length)") {
		t.Errorf("? reference missing min(length) aggregate")
	}
}
