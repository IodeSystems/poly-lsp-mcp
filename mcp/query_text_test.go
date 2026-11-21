package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// F3: a projection `{ name; line }` on a bare-edge selector (which lands
// on the far nodes under F5) carries the far node's name and line. The
// old test projected edge fields (site, conf, kind) on edge rows; with
// F5 the rows are the far nodes, so the projection names the node's own
// fields.
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
	if err := s.QueryText(`#Save::in.call { name; line }`, 0, 0, "", &b); err != nil {
		t.Fatalf("projection query failed: %v", err)
	}
	out := b.String()

	// Header: the two field names, tab-separated, in order.
	if !strings.Contains(out, "name\tline\n") {
		t.Fatalf("TSV header must be name, line; got:\n%s", out)
	}
	// A data row: the far node's name is "Caller", starting at line 5.
	if !strings.Contains(out, "Caller\t5\n") {
		t.Errorf("expected a row with name=Caller, line=5; got:\n%s", out)
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

// f3AdditiveFixture is the shared workspace for the F3 additive-regression
// tests: a Go file with a declaration and a call to it, a TS file, and a
// markdown file — enough tree (file, symbol, ref) for the selectors tested.
func f3AdditiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module q\ngo 1.21\n")
	write("a.go", "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n")
	write("web/x.ts", "export function f(a: number) { return a; }\n")
	write("notes.md", "# hello\n")
	return dir
}

// F3 additive regression, the text view. A selector with no declaration
// block and no :path modifier must answer BYTE-FOR-BYTE what F1's grouped
// tree answers — the expected strings below are exactly that, so any
// renderer change on this path (a stray tab, a reordered row, a dropped
// summary line) fails the test. The same selector plus a block, or plus a
// :path, must NOT match — those are the new forms, and the only ones
// allowed to change the output.
func TestQueryTextNoBlockNoPathIsByteForByteF1(t *testing.T) {
	s := newQueryServer(t, f3AdditiveFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	cases := []struct {
		sel  string
		want string
	}{
		// A plain symbol query: reading line, the groups, the node rows,
		// the summary line.
		{`func`, "read as ─ func: nodes of type `func`\n\na.go\n├─ func Save    3\n└─ func Caller  5-7\n\nweb/x.ts\n└─ func f  1\n\n2 files · 3 matches\n"},
		// An edge query: the reading table, the far node the edge lands on
		// (F5: a bare-edge selector returns the nodes the edge points AT),
		// the summary, and the lexical-edge caveat.
		{`#'a.go#Save'::in.call`, "read as ─\n  #a.go#Save  named `a.go#Save` — a dir, file or symbol id (also matches the `file#symbol` address form)\n  ::in.call   reference edges pointing IN to it (who references this), only of kind call. A generated node — `*` never matches these\n  ⇒ returns the `::in.call` nodes — NOT the `*` nodes, which only constrain them\n\na.go\n└─ func Caller  5-7\n\n1 file · 1 matches\n\nnote: ::in/::out edges are name-keyed (lexical) here — a far end may be a\nsame-named symbol, not the resolved one. The MCP server resolves via child\nLSPs and labels each edge (conf: lsp|lexical|unsettled); `query` does not.\n"},
		// An edge traversal: the reading table, the landed symbol, the
		// same caveat.
		{`#'a.go#Save'::in.call > *`, "read as ─\n  #a.go#Save  named `a.go#Save` — a dir, file or symbol id (also matches the `file#symbol` address form)\n  ::in.call   reference edges pointing IN to it (who references this), only of kind call. A generated node — `*` never matches these\n  >           DIRECT children of the above — one level only, not any descendant\n  *           nodes of ANY type\n  ⇒ returns the `*` nodes — NOT the `*#a.go#Save` nodes, which only constrain them\n\na.go\n└─ func Caller  5-7\n\n1 file · 1 matches\n\nnote: ::in/::out edges are name-keyed (lexical) here — a far end may be a\nsame-named symbol, not the resolved one. The MCP server resolves via child\nLSPs and labels each edge (conf: lsp|lexical|unsettled); `query` does not.\n"},
	}

	for _, c := range cases {
		if got := run(c.sel); got != c.want {
			t.Errorf("no-block %q is not F1's bytes:\n--- got ---\n%q\n--- want ---\n%q", c.sel, got, c.want)
		}
	}

	// The new forms change the output where they are supposed to: the
	// block projects (a TSV, not the grouped tree). :path is invisible
	// in the grouped tree by design — the chain rides the projected
	// fields (checked in the --json test below), so the grouped tree
	// must stay identical with or without it.
	if got := run(`func { name; line }`); got == run(`func`) || !strings.Contains(got, "name\tline\n") {
		t.Errorf("the block form must render the TSV projection, not the grouped tree; got:\n%s", got)
	}
	if got, plain := run(`#Caller::out.call{1,}:path > *`), run(`#Caller::out.call{1,} > *`); got != plain {
		t.Errorf(":path must NOT change the grouped tree (the chain rides the projected fields); got:\n%s\n--- no :path ---\n%s", got, plain)
	}
}

// F3 additive regression, the --json view. A no-block selector's document
// is byte-for-byte F1's: the same fields, the same order (json.Marshal
// sorts map keys, so the bytes ARE the contract), no projection keys
// anywhere. The same selector with a block differs — and only the
// named fields survive.
func TestQueryJSONNoBlockIsByteForByteF1(t *testing.T) {
	s := newQueryServer(t, f3AdditiveFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryJSON(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	// writeJSONDoc pretty-prints with two-space indent, and the
	// document carries budgetExceeded. The bytes are F1's.
	// F5: a bare-edge selector lands on the far node (the caller), not
	// the edge row. The far-node row carries the node's address, file,
	// type, and the conf/via of the edge that reached it.
	want := `{
  "budgetExceeded": false,
  "matches": [
    {
      "@": [
        5,
        7
      ],
      "conf": "lexical",
      "file": "a.go",
      "node": "a.go#Caller",
      "type": "func",
      "via": "a.go@6"
    }
  ],
  "offset": 0,
  "returned": 1,
  "timedOut": false,
  "total": 1,
  "truncated": false
}
`
	if got := run(`#'a.go#Save'::in.call`); got != want {
		t.Errorf("no-block --json is not F1's bytes:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// The block form: the row carries exactly the named fields. The far
	// node is a plain func, so its fields are the node's own.
	var bb bytes.Buffer
	if err := s.QueryJSON(`#'a.go#Save'::in.call { node; file; conf }`, 0, 0, "", &bb); err != nil {
		t.Fatalf("block --json: %v", err)
	}
	var doc struct {
		Matches []map[string]any `json:"matches"`
	}
	if err := json.Unmarshal(bb.Bytes(), &doc); err != nil {
		t.Fatalf("not a JSON document: %v\n%s", err, bb.String())
	}
	if len(doc.Matches) != 1 || len(doc.Matches[0]) != 3 {
		t.Fatalf("the block row must carry exactly the 3 named fields; got: %s", bb.String())
	}
	// conf is an edge field; a far node (a plain func) has none — the
	// edge's conf rides in the no-block row's via/conf, not in a
	// projected field of the node itself.
	if doc.Matches[0]["node"] != "a.go#Caller" || doc.Matches[0]["file"] != "a.go" || doc.Matches[0]["conf"] != "" {
		t.Errorf("block row = %v; want node=a.go#Caller file=a.go conf=(empty)", doc.Matches[0])
	}
}

// F3 additive regression, the MCP node_query view. The handler has no
// declaration-block branch at all — a no-block selector's rows are built
// with the same queryRow F1 uses, and the payload fields are F1's. Pin
// the row's exact keys so a projection leaking into the model's view
// fails loudly: that view is the one a model reads every turn.
func TestNodeQueryNoBlockRowsAreF1Shape(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_query", map[string]any{"selector": `#"main.go#Server.Start":in.call`, "limit": 50})
	if r.IsError {
		t.Fatalf("node_query errored: %s", r.Content[0].Text)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	matches, _ := doc["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("want the one call edge into Server.Start; got: %s", r.Content[0].Text)
	}
	// F5: a bare-edge selector lands on the far node (the caller), not
	// the edge row. The row carries the node's address, file, type, and
	// the conf/via of the edge that reached it.
	row, _ := matches[0].(map[string]any)
	var keys []string
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"@", "conf", "node", "type", "via"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("row keys = %v; want %v (F5 far-node shape — no projection fields)", keys, want)
	}
	if row["node"] != "main.go#CallsStart" || row["type"] != "func" {
		t.Errorf("row = %v; want the far node main.go#CallsStart with type func", row)
	}
	// The payload carries F1's shape: totalMatches, matches, read.
	for _, k := range []string{"matches", "read", "totalMatches"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("payload missing %q; got keys %s", k, r.Content[0].Text)
		}
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
