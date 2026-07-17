package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Tests for the MODERN 3-tool surface (the default one New() leaves
// the server in). Legacy-surface coverage lives in server_test.go /
// node_addr_test.go behind SetLegacyTools(true).

// modernFixture is a small polyglot workspace exercising the whole
// unified tree: dirs, files, symbols, arguments, references.
const modernGoMain = `package main

import "fmt"

type Server struct {
	Name string
}

func (s *Server) Start(ctx string, retries int) error {
	fmt.Println(ctx)
	return nil
}

func Free(only int) {}

func init() { _ = 1 }

func init() { _ = 2 }

func CallsStart() {
	s := &Server{}
	_ = s.Start("x", 1)
}
`

const modernTSSrc = `export class UserService {
  name: string;
  getUser(id: string, verbose: boolean) { return id; }
}

export function topLevel(alpha: number) {
  return alpha;
}
`

// startModern boots a session on a fixture workspace with the DEFAULT
// (modern) tool surface.
func startModern(t *testing.T) (*mcpSession, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	write("main.go", modernGoMain)
	write("web/some_file.ts", modernTSSrc)
	write("notes.md", "# hello\n")

	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s, dir
}

type queryResult struct {
	TotalMatches int    `json:"totalMatches"`
	Returned     int    `json:"returned"`
	Truncated    bool   `json:"truncated"`
	Note         string `json:"note"`
	Matches      []struct {
		Node  string `json:"node"`
		Class string `json:"type"`
		At    []int  `json:"@"`
		Hits  []struct {
			Line   int      `json:"line"`
			Text   string   `json:"text"`
			Before []string `json:"before"`
			After  []string `json:"after"`
		} `json:"hits"`
	} `json:"matches"`
}

func query(t *testing.T, s *mcpSession, args map[string]any) queryResult {
	t.Helper()
	r := s.callTool("node_query", args)
	if r.IsError {
		t.Fatalf("node_query %v errored: %s", args, r.Content[0].Text)
	}
	var q queryResult
	if err := json.Unmarshal([]byte(r.Content[0].Text), &q); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	return q
}

func queryErr(t *testing.T, s *mcpSession, args map[string]any) string {
	t.Helper()
	r := s.callTool("node_query", args)
	if !r.IsError {
		t.Fatalf("node_query %v should have errored, got %s", args, r.Content[0].Text)
	}
	return r.Content[0].Text
}

func nodes(q queryResult) []string {
	out := make([]string, 0, len(q.Matches))
	for _, m := range q.Matches {
		out = append(out, m.Node)
	}
	return out
}

func hasNode(q queryResult, want string) bool {
	for _, n := range nodes(q) {
		if n == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------- tree shape

func TestModernQueryRootTour(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// The canonical tour: top-level dirs + files, nothing deeper.
	q := query(t, s, map[string]any{"selector": ":root > *"})
	for _, want := range []string{"main.go", "web", "notes.md", "go.mod"} {
		if !hasNode(q, want) {
			t.Errorf(":root > * missing %q; got %v", want, nodes(q))
		}
	}
	for _, m := range q.Matches {
		if m.Class != "dir" && m.Class != "file" {
			t.Errorf(":root > * returned a non dir/file node: %+v", m)
		}
	}
	// The single .project node is the root itself, not a child.
	if hasNode(q, "web/some_file.ts") {
		t.Errorf(":root > * must not reach nested files; got %v", nodes(q))
	}
}

func TestModernQueryRootMatchesProject(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": ":root"})
	if q.TotalMatches != 1 {
		t.Fatalf(":root should match exactly one .project node, got %d (%v)", q.TotalMatches, nodes(q))
	}
	if q.Matches[0].Class != "project" || q.Matches[0].Node != filepath.Base(dir) {
		t.Errorf(":root = %+v, want the .project node id %q", q.Matches[0], filepath.Base(dir))
	}
	// .project selects the same single node :root does.
	if p := query(t, s, map[string]any{"selector": "project"}); p.TotalMatches != 1 {
		t.Errorf(".project should match the same one node, got %d", p.TotalMatches)
	}
}

func TestModernQueryArgumentNodes(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": "argument", "limit": 50})
	for _, want := range []string{
		"main.go#Server.Start.ctx", "main.go#Server.Start.retries", "main.go#Free.only",
		"web/some_file.ts#UserService.getUser.id", "web/some_file.ts#topLevel.alpha",
	} {
		if !hasNode(q, want) {
			t.Errorf(".argument missing %q; got %v", want, nodes(q))
		}
	}
}

// ---------------------------------------------------------- pseudo-classes

func TestModernQueryAncestorChain(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Containment is the chain itself (the :has_parent replacement):
	// every function in one file, named by BASENAME (not relpath) —
	// both are legitimate ids.
	q := query(t, s, map[string]any{"selector": `#"some_file.ts" func`})
	if !hasNode(q, "web/some_file.ts#topLevel") {
		t.Errorf("expected topLevel; got %v", nodes(q))
	}
	for _, n := range nodes(q) {
		if !strings.HasPrefix(n, "web/some_file.ts#") {
			t.Errorf("chain leaked outside the file: %q", n)
		}
	}

	// The descendant combinator reaches ANY depth — a method nested in
	// a class is still "in" the file.
	q = query(t, s, map[string]any{"selector": `#"web/some_file.ts" method`})
	if !hasNode(q, "web/some_file.ts#UserService.getUser") {
		t.Errorf("descendant should reach into the class; got %v", nodes(q))
	}
	// '>' narrows to the direct parent: getUser's direct parent is the
	// class, not the file.
	q = query(t, s, map[string]any{"selector": `file > method`})
	if hasNode(q, "web/some_file.ts#UserService.getUser") {
		t.Errorf("getUser's direct parent is the class, not the file; got %v", nodes(q))
	}
	q = query(t, s, map[string]any{"selector": `class > method`})
	if !hasNode(q, "web/some_file.ts#UserService.getUser") {
		t.Errorf("getUser's direct parent IS the class; got %v", nodes(q))
	}
}

func TestModernQueryAny(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// The :has replacement — ∃ a descendant. A method declaring an
	// argument named `retries`.
	q := query(t, s, map[string]any{"selector": "method:any(argument#retries)"})
	if !hasNode(q, "main.go#Server.Start") || q.TotalMatches != 1 {
		t.Errorf("expected only Server.Start; got %v", nodes(q))
	}
	// A file with a class descendant.
	q = query(t, s, map[string]any{"selector": "file:any(class)"})
	if !hasNode(q, "web/some_file.ts") {
		t.Errorf("expected the ts file; got %v", nodes(q))
	}
	// Leading '>' narrows the relative selector to children.
	q = query(t, s, map[string]any{"selector": "file:any(> method)"})
	if hasNode(q, "web/some_file.ts") {
		t.Errorf("getUser is not a DIRECT child of the file; got %v", nodes(q))
	}
}

func TestModernQueryParents(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Reverse lookup, inverted into a move: who calls Start?
	q := query(t, s, map[string]any{"selector": `#"main.go#Server.Start":parents(*)`, "limit": 50})
	if !hasNode(q, "main.go#CallsStart") {
		t.Errorf("expected CallsStart among referrers; got %v", nodes(q))
	}
}

func TestModernQueryContains(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// :contains is scoped to the node's OWN source.
	q := query(t, s, map[string]any{"selector": `func:contains("_ = 2")`, "limit": 50})
	if !hasNode(q, "main.go#init[2]") {
		t.Errorf("expected init[2]; got %v", nodes(q))
	}
	if hasNode(q, "main.go#init[1]") {
		t.Errorf("init[1] doesn't contain \"_ = 2\"; got %v", nodes(q))
	}
	// Same matcher as grep: -i works here too.
	if q := query(t, s, map[string]any{"selector": `method:contains("-i FMT.PRINTLN")`}); !hasNode(q, "main.go#Server.Start") {
		t.Errorf("case-insensitive :contains failed; got %v", nodes(q))
	}
}

func TestModernQueryRepetitionFromRoot(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// {m,n} repeats child-joined; {0,…}'s skip path keeps the anchor.
	q := query(t, s, map[string]any{"selector": ":root > *{0,1}", "limit": 50})
	if !hasNode(q, "main.go") {
		t.Errorf("{0,1} should include top-level files; got %v", nodes(q))
	}
	classes := map[string]bool{}
	for _, m := range q.Matches {
		classes[m.Class] = true
	}
	if !classes["project"] {
		t.Errorf("{0,n} must include the anchor itself (skip path); got %v", nodes(q))
	}
	// Two child steps reach a top-level file's symbols.
	q = query(t, s, map[string]any{"selector": ":root > *{2}", "limit": 100})
	if !hasNode(q, "main.go#Server") {
		t.Errorf("*{2} should reach main.go's symbols; got %v", nodes(q))
	}
}

func TestModernQueryZeroRepIsTheAnchor(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// {0}: the element vanishes — only the skip path remains, so the
	// chain yields the previous target itself.
	q := query(t, s, map[string]any{"selector": "file#main.go > *{0}", "limit": 50})
	if q.TotalMatches != 1 || !hasNode(q, "main.go") {
		t.Errorf("*{0} should be the anchor file itself; got %v", nodes(q))
	}
	// Contrast: the default descendant range excludes self.
	q = query(t, s, map[string]any{"selector": "file#main.go *", "limit": 100})
	if hasNode(q, "main.go") {
		t.Errorf("plain descendant must exclude the anchor itself; got %v", nodes(q))
	}
}

func TestModernQueryAttrOpsAndUnion(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Attribute ops stay supported under the hood (not documented in
	// the tool description — the guided error teaches them).
	q := query(t, s, map[string]any{"selector": "func[name^=Call]", "limit": 50})
	if !hasNode(q, "main.go#CallsStart") {
		t.Errorf("prefix op failed; got %v", nodes(q))
	}
	// Comma = union.
	q = query(t, s, map[string]any{"selector": "struct, class", "limit": 50})
	if !hasNode(q, "main.go#Server") || !hasNode(q, "web/some_file.ts#UserService") {
		t.Errorf("union failed; got %v", nodes(q))
	}
}

func TestModernQueryGuidedParseError(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": "func:bogus(x)"})
	if !strings.Contains(msg, "unknown pseudo-class") || !strings.Contains(msg, "Selector grammar") {
		t.Errorf("expected a guided grammar dump; got %q", msg)
	}
	// The deep grammar (attribute ops) is taught by the error, not the
	// every-turn description.
	if !strings.Contains(msg, "[name^=X]") {
		t.Errorf("grammar dump should teach attribute ops; got %q", msg)
	}
	// A bare word that isn't a type: almost always a workspace NAME used where
	// a type belongs, so the error points at the id.
	if msg := queryErr(t, s, map[string]any{"selector": "nosuchtype"}); !strings.Contains(msg, "#nosuchtype") {
		t.Errorf("unknown type should suggest the #id form; got %q", msg)
	}
	// `.func` for a KNOWN type is accepted, not corrected. Tags are canonical,
	// but a CSS prior beats a schema line every time — measured: the model kept
	// writing `.file` with the description saying tags twice, and rejecting it
	// cost ~4 turns per task to fix a spelling that was never ambiguous.
	if q := query(t, s, map[string]any{"selector": ".func"}); q.TotalMatches == 0 {
		t.Errorf(`".func" should be accepted as the known type "func"`)
	}
	// The guard that matters is unaffected: it was never about the dot. An
	// unknown NAME is still refused and still points at the id form.
	if msg := queryErr(t, s, map[string]any{"selector": ".cache"}); !strings.Contains(msg, "#cache") {
		t.Errorf(`".cache" should still suggest #cache; got %q`, msg)
	}
}

// ---------------------------------------------------------- grep

func TestModernQueryGrepReturnsHitsWithContext(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": "method#Start",
		"grep":     "-A1 fmt.Println",
	})
	if q.TotalMatches != 1 {
		t.Fatalf("want 1 node, got %d (%v)", q.TotalMatches, nodes(q))
	}
	hits := q.Matches[0].Hits
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %+v", hits)
	}
	if !strings.Contains(hits[0].Text, "fmt.Println(ctx)") {
		t.Errorf("hit text = %q", hits[0].Text)
	}
	// Line numbers are absolute file lines, so they line up with @.
	if hits[0].Line < q.Matches[0].At[0] || hits[0].Line > q.Matches[0].At[1] {
		t.Errorf("hit line %d outside node span %v", hits[0].Line, q.Matches[0].At)
	}
	if len(hits[0].After) != 1 || !strings.Contains(hits[0].After[0], "return nil") {
		t.Errorf("-A1 should carry one following line; got %+v", hits[0].After)
	}
}

func TestModernQueryGrepFiltersNonMatchingNodes(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": "func", "grep": "-i PRINTLN", "limit": 50})
	for _, n := range nodes(q) {
		if strings.Contains(n, "init") {
			t.Errorf("grep should filter out nodes with no hit; got %v", nodes(q))
		}
	}
}

func TestModernQueryGrepRejectsUnsupportedFlag(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": "*", "grep": "-r derp"})
	if !strings.Contains(msg, "-r") || !strings.Contains(msg, "selector") {
		t.Errorf("expected a guided error naming -r and the selector's scoping role; got %q", msg)
	}
	// A bare file argument is rejected for the same reason.
	if msg := queryErr(t, s, map[string]any{"selector": "*", "grep": "derp main.go"}); !strings.Contains(msg, "extra argument") {
		t.Errorf("expected extra-argument rejection; got %q", msg)
	}
}

func TestModernQueryGrepLiteralByDefaultRegexWithE(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Default is a LITERAL substring: regex metachars match nothing.
	q := query(t, s, map[string]any{"selector": "method#Start", "grep": "fmt.P.intln"})
	if q.TotalMatches != 0 {
		t.Errorf("default should be literal, not regex; got %v", nodes(q))
	}
	// -E opts into a regex.
	q = query(t, s, map[string]any{"selector": "method#Start", "grep": "-E 'fmt.P.intln'"})
	if q.TotalMatches != 1 {
		t.Errorf("-E should match by regex; got %v", nodes(q))
	}
}

// ---------------------------------------------------------- pagination

func TestModernQueryPaginationDefaultsAndNote(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "\nfunc F%02d() {}\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Default limit is 20 — small on purpose.
	q := query(t, s, map[string]any{"selector": "func"})
	if q.TotalMatches != 25 || q.Returned != 20 || len(q.Matches) != 20 {
		t.Fatalf("want 25 total / 20 returned, got %d / %d", q.TotalMatches, q.Returned)
	}
	if !q.Truncated {
		t.Error("truncated should be set when more matches exist")
	}
	if !strings.Contains(q.Note, "20 of 25") {
		t.Errorf("note = %q, want it to say 20 of 25", q.Note)
	}

	// offset pages, and the last page isn't truncated.
	q2 := query(t, s, map[string]any{"selector": "func", "offset": 20})
	if q2.Returned != 5 || q2.Truncated {
		t.Errorf("page 2: returned=%d truncated=%v, want 5/false", q2.Returned, q2.Truncated)
	}
	if nodes(q)[0] == nodes(q2)[0] {
		t.Error("offset didn't advance the window")
	}

	// A window that fits carries no truncation signal at all.
	q3 := query(t, s, map[string]any{"selector": "func", "limit": 100})
	if q3.Truncated || q3.Note != "" {
		t.Errorf("full window should be untruncated and unannotated; got %+v", q3)
	}
}

func TestModernQueryFlatRowShape(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_query", map[string]any{"selector": "method#Start"})
	var raw struct {
		Matches []map[string]any `json:"matches"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &raw)
	if len(raw.Matches) != 1 {
		t.Fatalf("want 1 match, got %v", raw.Matches)
	}
	m := raw.Matches[0]
	// Flat {node,type,@} rows — no grouping, no nesting. The key is "type",
	// matching the tag grammar: a row that said "class" would model the
	// spelling we removed, every turn, right where the model copies from.
	if m["node"] != "main.go#Server.Start" || m["type"] != "method" {
		t.Errorf("row = %+v", m)
	}
	at, ok := m["@"].([]any)
	if !ok || len(at) != 2 {
		t.Errorf("@ should be [start,end]; got %+v", m["@"])
	}
	if _, has := m["hits"]; has {
		t.Errorf("hits must be absent when grep isn't set; got %+v", m)
	}
}

// ---------------------------------------------------------- node_read

func TestModernNodeReadNeverTruncatesAddressedNode(t *testing.T) {
	dir := t.TempDir()
	// A declaration well past the ~2k auto-cap that used to silently
	// truncate node_read (and so let node_edit destroy the tail).
	var body strings.Builder
	body.WriteString("package main\n\nfunc Big() {\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&body, "\t_ = %d // padding padding padding padding padding\n", i)
	}
	body.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_read", map[string]any{"node": "big.go#Big"})
	if r.IsError {
		t.Fatalf("errored: %s", r.Content[0].Text)
	}
	var payload map[string]any
	json.Unmarshal([]byte(r.Content[0].Text), &payload)

	text, _ := payload["text"].(string)
	if len(text) < 2048 {
		t.Fatalf("declaration should come back whole, got %d bytes", len(text))
	}
	if !strings.Contains(text, "_ = 199") || !strings.HasSuffix(strings.TrimRight(text, "\n"), "}") {
		t.Error("declaration tail missing — a partial node read must be impossible")
	}
	// The core invariant: no truncation key can appear on this path.
	for _, k := range []string{"truncated", "truncatedReason", "hint"} {
		if _, has := payload[k]; has {
			t.Errorf("addressed node read must never carry %q; got %+v", k, payload)
		}
	}
}

func TestModernNodeReadSymbolRejectsLineWindow(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "main.go#Server.Start", "startLine": 2})
	if !r.IsError {
		t.Fatalf("startLine with a symbol-resolving node must error; got %s", r.Content[0].Text)
	}
	if !strings.Contains(r.Content[0].Text, "always whole") {
		t.Errorf("error should explain node reads are whole; got %q", r.Content[0].Text)
	}
	// lineLimit is rejected the same way.
	if r := s.callTool("node_read", map[string]any{"node": "main.go#Server.Start", "lineLimit": 3}); !r.IsError {
		t.Error("lineLimit with a symbol node must error")
	}
}

func TestModernNodeReadWholeFileBrowseStillWindows(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// A whole-FILE address is a browse: startLine/lineLimit apply.
	r := s.callTool("node_read", map[string]any{"node": "main.go", "startLine": 1, "lineLimit": 2})
	if r.IsError {
		t.Fatalf("errored: %s", r.Content[0].Text)
	}
	var p struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)
	if !strings.HasPrefix(p.Text, "package main") || strings.Contains(p.Text, "func Free") {
		t.Errorf("line window not applied; got %q", p.Text)
	}
}

func TestModernNodeReadRejectsDirectory(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "web"})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "directory") {
		t.Errorf("reading a dir should error clearly; got %+v", r.Content)
	}
}

func TestModernNodeReadBySelector(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// node also accepts a full selector, as long as it's unambiguous.
	r := s.callTool("node_read", map[string]any{"node": "method:any(argument#retries)"})
	if r.IsError {
		t.Fatalf("errored: %s", r.Content[0].Text)
	}
	var p struct {
		Node string `json:"node"`
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)
	if p.Node != "main.go#Server.Start" || !strings.Contains(p.Text, "func (s *Server) Start") {
		t.Errorf("selector didn't resolve to Server.Start; got %+v", p)
	}
}

func TestModernNodeReadAmbiguousSelectorErrors(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "func"})
	if !r.IsError {
		t.Fatalf("an ambiguous selector must never be silently picked; got %s", r.Content[0].Text)
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "main.go#Free") {
		t.Errorf("error should list candidates; got %q", msg)
	}
}

// ------------------------------------------- the ambiguity-as-error bug

// TestModernBareAmbiguousAddressErrors is the regression test for the
// silent-wrong-node bug: with two `init` funcs, renderSegment emits
// init[1]/init[2], and the legacy resolver normalized a BARE `init`
// to "the first one" — so an address obtained while there was only one
// init would silently start resolving to a different symbol once a
// second appeared. The modern surface errors and lists both instead.
func TestModernBareAmbiguousAddressErrors(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	for _, tool := range []string{"node_read", "node_edit"} {
		args := map[string]any{"node": "main.go#init"}
		if tool == "node_edit" {
			args["newText"] = "func init() { _ = 99 }"
		}
		r := s.callTool(tool, args)
		if !r.IsError {
			t.Fatalf("%s: bare ambiguous address must error, got %s", tool, r.Content[0].Text)
		}
		msg := r.Content[0].Text
		if !strings.Contains(msg, "ambiguous") {
			t.Errorf("%s: error should say ambiguous; got %q", tool, msg)
		}
		for _, cand := range []string{"main.go#init[1]", "main.go#init[2]"} {
			if !strings.Contains(msg, cand) {
				t.Errorf("%s: error should list candidate %q; got %q", tool, cand, msg)
			}
		}
	}
	// Critically: the failed edit wrote nothing.
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "_ = 1") || !strings.Contains(string(got), "_ = 2") {
		t.Errorf("ambiguous edit must not write:\n%s", got)
	}
	// An explicit ordinal still disambiguates exactly as before.
	r := s.callTool("node_read", map[string]any{"node": "main.go#init[2]"})
	if r.IsError {
		t.Fatalf("explicit ordinal should resolve: %s", r.Content[0].Text)
	}
	var p struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)
	if !strings.Contains(p.Text, "_ = 2") {
		t.Errorf("init[2] resolved to the wrong node: %q", p.Text)
	}
}

func TestModernUniqueBareAddressStillResolves(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Ambiguity-as-error must not break the common case: a bare name
	// with exactly one candidate resolves fine.
	r := s.callTool("node_read", map[string]any{"node": "main.go#Free"})
	if r.IsError {
		t.Fatalf("unique bare address should resolve: %s", r.Content[0].Text)
	}
}

// ---------------------------------------------------------- node_edit

func TestModernNodeEditExactlyOneOp(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Zero ops.
	r := s.callTool("node_edit", map[string]any{"node": "main.go#Free"})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "exactly one") {
		t.Errorf("no-op edit should error; got %+v", r.Content)
	}
	// Two ops.
	r = s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "newText": "func Free(only int) {}", "rename": "Freed",
	})
	if !r.IsError {
		t.Fatalf("two ops must error, not silently pick one")
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "newText") || !strings.Contains(msg, "rename") {
		t.Errorf("error should name the conflicting ops; got %q", msg)
	}
	// delete:false is rejected rather than treated as absent.
	r = s.callTool("node_edit", map[string]any{"node": "main.go#Free", "delete": false})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "delete must be true") {
		t.Errorf("delete:false should error; got %+v", r.Content)
	}
}

func TestModernNodeEditRenameOnlyModifiers(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "newText": "func Free(only int) {}", "includeComments": true,
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "includeComments only applies to rename") {
		t.Errorf("includeComments outside rename should error; got %+v", r.Content)
	}
	r = s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "delete": true,
		"resolution": map[string]any{"mode": "underlying", "target": "x"},
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "resolution only applies to rename") {
		t.Errorf("resolution outside rename should error; got %+v", r.Content)
	}
}

// ---- the four legal shapes

// Shape 1: modify an existing node with a SNIPPET. oldText only has to
// be unique within the addressed node, not the file.
func TestModernNodeEditSnippetModify(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// "return nil" appears in Start AND in other funcs across the
	// file — but scoped to this node it is unique, so a short snippet
	// is enough. That's the whole point of address-then-edit.
	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Server.Start", "oldText": "return nil", "newText": "return errors.New(\"boom\")",
	})
	if r.IsError {
		t.Fatalf("snippet edit errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), `return errors.New("boom")`) {
		t.Errorf("snippet not applied:\n%s", got)
	}
	// Only the addressed node changed: the identical snippet inside
	// the OTHER function is untouched.
	if !strings.Contains(string(got), "func CallsStart()") {
		t.Errorf("neighbour clobbered:\n%s", got)
	}
	if strings.Count(string(got), `errors.New("boom")`) != 1 {
		t.Errorf("edit escaped its node:\n%s", got)
	}
}

// Shape 2: whole-node rewrite falls out of shape 1 — no special flag.
// node_read's text is a valid oldText by construction (it's never
// truncated), which is exactly what makes this reliable.
func TestModernNodeEditWholeNodeRewriteViaReadText(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "main.go#Free"})
	var p struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)
	if p.Text != "func Free(only int) {}" {
		t.Fatalf("read text = %q", p.Text)
	}
	// Feed the whole read text straight back as oldText.
	if r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": p.Text, "newText": "func Free(only int) { _ = only }",
	}); r.IsError {
		t.Fatalf("whole-node rewrite errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Free(only int) { _ = only }") {
		t.Errorf("rewrite missing:\n%s", got)
	}
	if !strings.Contains(string(got), "func CallsStart") {
		t.Errorf("neighbour clobbered:\n%s", got)
	}
}

// A no-op round trip (oldText == newText == the node's whole text)
// leaves the file byte-identical.
func TestModernNodeEditIdentityRoundTrip(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	r := s.callTool("node_read", map[string]any{"node": "main.go#Free"})
	var p struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &p)

	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": p.Text, "newText": p.Text,
	}); r.IsError {
		t.Fatalf("identity round-trip errored: %s", r.Content[0].Text)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(before) != string(after) {
		t.Errorf("read → write of the same text changed the file:\n%s", after)
	}
}

// ---- error cases

// oldText not found: the error carries the node's CURRENT full text so
// a retry is one turn, not a read-then-edit round trip.
func TestModernNodeEditOldTextNotFound(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": "nowhere in here", "newText": "x",
	})
	if !r.IsError {
		t.Fatal("missing oldText must error")
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "not found") {
		t.Errorf("error should say not found; got %q", msg)
	}
	// The node's current text is in the payload.
	if !strings.Contains(msg, "func Free(only int) {}") {
		t.Errorf("error must include the node's current text; got %q", msg)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(before) != string(after) {
		t.Error("failed edit must not write")
	}
}

// oldText found more than once: never guess which — list them.
func TestModernNodeEditOldTextAmbiguous(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nfunc Dup() {\n\tx := 1\n\tx = 2\n\tx = 2\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "d.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{
		"node": "d.go#Dup", "oldText": "x = 2", "newText": "x = 3",
	})
	if !r.IsError {
		t.Fatal("ambiguous oldText must error, never pick an occurrence")
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "occurs 2 times") {
		t.Errorf("error should count occurrences; got %q", msg)
	}
	if !strings.Contains(msg, "lengthen") {
		t.Errorf("error should tell the caller to lengthen oldText; got %q", msg)
	}
	// Each occurrence is listed with its line of context.
	if strings.Count(msg, "node line") != 2 {
		t.Errorf("error should list both occurrences with context; got %q", msg)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "d.go"))
	if string(got) != src {
		t.Error("ambiguous edit must not write")
	}
}

// newText alone against an address that ALREADY resolves is the
// create-degrades-into-clobber guard.
func TestModernNodeEditCreateGuardOnExistingNode(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	for _, node := range []string{"main.go#Free", "main.go"} {
		r := s.callTool("node_edit", map[string]any{"node": node, "newText": "whatever"})
		if !r.IsError {
			t.Fatalf("%s: newText alone on an existing node must error", node)
		}
		if !strings.Contains(r.Content[0].Text, "already exists") {
			t.Errorf("%s: error should say it already exists; got %q", node, r.Content[0].Text)
		}
		if !strings.Contains(r.Content[0].Text, "oldText") {
			t.Errorf("%s: error should point at oldText; got %q", node, r.Content[0].Text)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(before) != string(after) {
		t.Error("guarded create must not write")
	}
}

// oldText against an address that doesn't resolve → the normal
// not-found error, no special casing.
func TestModernNodeEditOldTextOnMissingAddress(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "nope/missing.go", "oldText": "a", "newText": "b",
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "no such file") {
		t.Errorf("expected not-found; got %+v", r.Content)
	}
	// A missing SYMBOL is the existing guided resolution error.
	r = s.callTool("node_edit", map[string]any{
		"node": "main.go#NoSuchSym", "oldText": "a", "newText": "b",
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "did you mean") {
		t.Errorf("expected guided symbol error; got %+v", r.Content)
	}
}

func TestModernNodeEditOldTextWithoutNewText(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{"node": "main.go#Free", "oldText": "func Free"})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "oldText needs newText") {
		t.Errorf("oldText without newText should error; got %+v", r.Content)
	}
}

func TestModernNodeEditDeleteTakesNoText(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "delete": true, "oldText": "func Free",
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "neither oldText nor newText") {
		t.Errorf("delete+oldText should error; got %+v", r.Content)
	}
}

func TestModernNodeEditCreateAndDeleteFile(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// newText against a not-yet-existing path creates the file.
	r := s.callTool("node_edit", map[string]any{
		"node": "web/new_file.ts", "newText": "export const x = 1;\n",
	})
	if r.IsError {
		t.Fatalf("create errored: %s", r.Content[0].Text)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "web/new_file.ts")); err != nil || string(b) != "export const x = 1;\n" {
		t.Errorf("file not created: %v / %q", err, b)
	}
	// Empty newText is refused rather than silently erasing.
	if r := s.callTool("node_edit", map[string]any{"node": "web/new_file.ts", "newText": ""}); !r.IsError {
		t.Error("empty whole-file newText should be refused")
	}
	// delete:true removes it.
	if r := s.callTool("node_edit", map[string]any{"node": "web/new_file.ts", "delete": true}); r.IsError {
		t.Fatalf("delete errored: %s", r.Content[0].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "web/new_file.ts")); !os.IsNotExist(err) {
		t.Error("file should be gone")
	}
}

func TestModernNodeEditRejectsDirectory(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{"node": "web", "delete": true})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "recurse into directories") {
		t.Errorf("dir delete should be refused; got %+v", r.Content)
	}
}

func TestModernNodeEditDeleteSymbol(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	if r := s.callTool("node_edit", map[string]any{"node": "main.go#Free", "delete": true}); r.IsError {
		t.Fatalf("errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if strings.Contains(string(got), "func Free") {
		t.Errorf("Free should be excised:\n%s", got)
	}
	if !strings.Contains(string(got), "func CallsStart") {
		t.Errorf("neighbour clobbered:\n%s", got)
	}
}

func TestModernNodeEditRename(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	if r := s.callTool("node_edit", map[string]any{"node": "main.go#Free", "rename": "Freed"}); r.IsError {
		t.Fatalf("rename errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Freed(") || strings.Contains(string(got), "func Free(") {
		t.Errorf("rename didn't apply:\n%s", got)
	}
}

// ---------------------------------------------------------- resources

func TestModernSurfaceExposesNoResources(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	resp := s.request("resources/list", nil)
	var got struct {
		Resources []any `json:"resources"`
	}
	json.Unmarshal(resp.Result, &got)
	if len(got.Resources) != 0 {
		t.Errorf("modern surface should expose no resources; got %+v", got.Resources)
	}
}

// TestEnclosingSymPathIgnoresArguments pins the fix for a regression
// the .argument node model would otherwise cause: a parameter shares
// its callable's signature LINE but has a zero-line span, so the
// smallest-span tie-break would make it the "enclosing symbol" of
// every hit on that line — changing what the shared enclosingSymPath
// reports to :references AND to the legacy search / node_references.
func TestEnclosingSymPathIgnoresArguments(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	cache := map[string][]symbols.Symbol{}
	abs := filepath.Join(dir, "main.go")
	// Line 9 is `func (s *Server) Start(ctx string, retries int) error {`
	// — the signature line, where the ctx/retries arguments live.
	got := s.srv.enclosingSymPath(abs, 9, cache)
	if got != "Server.Start" {
		t.Errorf("enclosing symbol of the signature line = %q, want Server.Start (not an argument)", got)
	}
}
