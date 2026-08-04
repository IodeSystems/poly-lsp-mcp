package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	TotalMatches   int                 `json:"totalMatches"`
	TotalAtLeast   string              `json:"totalMatchesAtLeast"`
	Returned       int                 `json:"returned"`
	Truncated      bool                `json:"truncated"`
	Note           string              `json:"note"`
	MineDeclares   []map[string]string `json:"mineDeclares"`
	TheirsDeclares []map[string]string `json:"theirsDeclares"`
	Hint           string              `json:"hint"`
	Edges          string              `json:"edges"`
	Cost           []string            `json:"cost"`
	Rollup         map[string]int      `json:"rollup"`
	Matches        []struct {
		Node           string              `json:"node"`
		Class          string              `json:"type"`
		At             []int               `json:"@"`
		In             string              `json:"in"`
		Text           string              `json:"text"`
		Before         []string            `json:"before"`
		After          []string            `json:"after"`
		From           []string            `json:"from"`
		To             []string            `json:"to"`
		Conf           string              `json:"conf"`
		Via            string              `json:"via"`
		Hop            int                 `json:"hop"`
		Ref            string              `json:"ref"`
		Sides          map[string]string   `json:"sides"`
		MineParses     bool                `json:"mineParses"`
		TheirsParses   bool                `json:"theirsParses"`
		Diff           string              `json:"diff"`
		Note           string              `json:"note"`
		MineDeclares   []map[string]string `json:"mineDeclares"`
		TheirsDeclares []map[string]string `json:"theirsDeclares"`
		Domain         string              `json:"domain"`
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

	// A misspelled pseudo-class is not a caller who is lost about SHAPE — it is
	// one who is a word off. Name the word; 7k of grammar buries it. (The full
	// dump stays for genuine syntax errors, asserted just below, and on "?".)
	msg := queryErr(t, s, map[string]any{"selector": "func:bogus(x)"})
	if !strings.Contains(msg, "unknown pseudo-class") {
		t.Errorf("expected the pseudo-class to be named; got %q", msg)
	}
	if strings.Contains(msg, "TASK → QUERY") {
		t.Errorf("a wrong NAME should not reprint the grammar; got %q", msg)
	}
	if !strings.Contains(msg, ":annotated") || !strings.Contains(msg, ":arity") {
		t.Errorf("the error should list the real vocabulary; got %q", msg)
	}
	// Half-remembered names get the one they meant. Edit distance alone picks
	// ":all" for ":arg" (2 edits vs 3); the shared prefix is what carries it.
	if m := queryErr(t, s, map[string]any{"selector": "func:arg"}); !strings.Contains(m, "did you mean :arity") {
		t.Errorf(":arg should suggest :arity; got %q", m)
	}
	// Malformed SYNTAX still gets the full grammar — there the caller does not
	// know the shape, and the attribute-op table is the thing they need.
	syn := queryErr(t, s, map[string]any{"selector": "func[name^=[A-Z]]"})
	for _, want := range []string{"TASK → QUERY", "^=", "~= is a regex", "[path~=test|smoke]"} {
		if !strings.Contains(syn, want) {
			t.Errorf("grammar dump should teach attribute ops; missing %q in %q", want, syn)
		}
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

// ---------------------------------------------------------- ::grep

func TestModernQueryGrepFragmentsWithContext(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `method#Start::grep('-A1 fmt.Println')`})
	if q.TotalMatches != 1 {
		t.Fatalf("want 1 fragment, got %d (%v)", q.TotalMatches, nodes(q))
	}
	m := q.Matches[0]
	if m.Class != "::grep" || m.In != "main.go#Server.Start" {
		t.Errorf("fragment row should carry its host; got %+v", m)
	}
	if !strings.Contains(m.Text, "fmt.Println(ctx)") {
		t.Errorf("fragment text = %q", m.Text)
	}
	// The address is the matched line itself.
	if m.Node != fmt.Sprintf("main.go@%d", m.At[0]) {
		t.Errorf("fragment address should be its site; got %q at %v", m.Node, m.At)
	}
	if len(m.After) != 1 || !strings.Contains(m.After[0], "return nil") {
		t.Errorf("-A1 should carry one following line; got %+v", m.After)
	}
}

// Default ::grep (no -A/-B/-C) returns the matched LINE and NO context —
// context is the token sink, so it is opt-in. The result carries a
// per-file rollup counting matches across the WHOLE set, so a wide search
// shows WHERE a term concentrates without paying for any line body.
func TestModernQueryGrepDefaultNoContextHasRollup(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `method#Start::grep('fmt.Println')`})
	if q.TotalMatches != 1 {
		t.Fatalf("want 1 fragment, got %d (%v)", q.TotalMatches, nodes(q))
	}
	m := q.Matches[0]
	if !strings.Contains(m.Text, "fmt.Println") {
		t.Errorf("the matched line is always present; got text=%q", m.Text)
	}
	if len(m.Before) != 0 || len(m.After) != 0 {
		t.Errorf("default grep must carry NO context; got before=%v after=%v", m.Before, m.After)
	}
	if q.Rollup["main.go"] != 1 {
		t.Errorf("rollup should count matches per file across the whole set; got %v", q.Rollup)
	}
}

// A ::grep hit on a pathologically long line (a generated bundle a file
// node can still point at) must come back ELIDED around the match, not as
// a multi-KB fragment that blows the token budget.
func TestModernQueryGrepCapsLongLine(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	// A 12k-char line with the match buried in the middle.
	longLine := "// " + strings.Repeat("a", 6000) + "NEEDLE" + strings.Repeat("b", 6000)
	write("gen.go", "package x\n"+longLine+"\nvar ok = 1\n")

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	q := query(t, s, map[string]any{"selector": `#'gen.go'::grep('NEEDLE')`, "limit": 5})
	if q.TotalMatches != 1 {
		t.Fatalf("want 1 fragment, got %d (%v)", q.TotalMatches, nodes(q))
	}
	m := q.Matches[0]
	if len(m.Text) > 700 {
		t.Errorf("long ::grep line not capped: %d bytes", len(m.Text))
	}
	if !strings.Contains(m.Text, "NEEDLE") {
		t.Errorf("the match must survive the cap; got %.80q", m.Text)
	}
	if !strings.Contains(m.Text, "chars)") {
		t.Errorf("an elided fragment must carry a (+N chars) marker; got %.80q", m.Text)
	}
}

// The `return` node exposes a callable's return TYPE as an addressable
// child, so `func:any(return#error)` = funcs returning error — including
// Go's (T, error) tuple, which splits into one node per type.
func TestModernQueryReturnNode(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	write("m.go", `package x

import "io"

func Single() error { return nil }
func Tuple() (int, error) { return 0, nil }
func Writer() io.Writer { return nil }
func Void() {}
`)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// The headline: funcs returning error — both the single and the tuple.
	q := query(t, s, map[string]any{"selector": `func:any(return#error)`, "limit": 20})
	got := nodes(q)
	if !slices.Contains(got, "m.go#Single") || !slices.Contains(got, "m.go#Tuple") {
		t.Errorf(":any(return#error) should match Single and Tuple; got %v", got)
	}
	if slices.Contains(got, "m.go#Void") || slices.Contains(got, "m.go#Writer") {
		t.Errorf(":any(return#error) must exclude Void and Writer; got %v", got)
	}

	// A qualified type matches by leaf (Writer) and by full alias (io.Writer).
	if q := query(t, s, map[string]any{"selector": `func:any(return#Writer)`}); !slices.Contains(nodes(q), "m.go#Writer") {
		t.Errorf(":any(return#Writer) should match Writer via the leaf; got %v", nodes(q))
	}
	if q := query(t, s, map[string]any{"selector": `func:any(return#'io.Writer')`}); !slices.Contains(nodes(q), "m.go#Writer") {
		t.Errorf(":any(return#'io.Writer') should match via the alias; got %v", nodes(q))
	}

	// The tuple's two types are BOTH addressable children.
	q = query(t, s, map[string]any{"selector": `#'m.go#Tuple' > return`, "limit": 20})
	if q.TotalMatches != 2 {
		t.Errorf("Tuple returns (int, error) — want 2 return children; got %v", nodes(q))
	}
}

// :arity filters by the count of `argument` children — a sound,
// structural signature-size predicate (no edge guessing).
func TestModernQueryArity(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Exact counts. Start(ctx, retries) = 2; Free(only) = 1.
	if q := query(t, s, map[string]any{"selector": `method#Start:arity(2)`}); q.TotalMatches != 1 {
		t.Errorf("Start has 2 params — :arity(2) should match; got %v", nodes(q))
	}
	if q := query(t, s, map[string]any{"selector": `method#Start:arity(1)`}); q.TotalMatches != 0 {
		t.Errorf("Start has 2 params — :arity(1) must NOT match; got %v", nodes(q))
	}
	if q := query(t, s, map[string]any{"selector": `func#Free:arity(1)`}); q.TotalMatches != 1 {
		t.Errorf("Free has 1 param — :arity(1) should match; got %v", nodes(q))
	}

	// No-arg: init()/CallsStart() match; Free (1 arg) does not.
	q := query(t, s, map[string]any{"selector": `func:arity(0,0)`, "limit": 50})
	got := nodes(q)
	for _, n := range got {
		if strings.Contains(n, "#Free") {
			t.Errorf(":arity(0,0) must exclude Free (1 arg); got %v", got)
		}
	}
	if !slices.Contains(got, "main.go#CallsStart") {
		t.Errorf(":arity(0,0) should include CallsStart (0 args); got %v", got)
	}

	// Open upper bound: two-or-more.
	q = query(t, s, map[string]any{"selector": `method:arity(2,)`, "limit": 50})
	for _, m := range q.Matches {
		if m.Node == "" {
			t.Errorf("bad match %+v", m)
		}
	}
	if q.TotalMatches < 1 {
		t.Errorf(":arity(2,) should match Start (2 params); got %v", nodes(q))
	}

	// Malformed ranges are guided errors, not silent.
	if msg := queryErr(t, s, map[string]any{"selector": `func:arity()`}); !strings.Contains(msg, "arity") {
		t.Errorf("empty :arity() should error and name the pseudo; got %q", msg)
	}
	if msg := queryErr(t, s, map[string]any{"selector": `func:arity(2,1)`}); !strings.Contains(msg, "max must be >= min") {
		t.Errorf(":arity(2,1) should reject max<min; got %q", msg)
	}
}

func TestModernQueryGrepFiltersAndClaims(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Hosts with no matching line yield no fragments.
	q := query(t, s, map[string]any{"selector": `func::grep('-i PRINTLN')`, "limit": 50})
	for _, m := range q.Matches {
		if strings.Contains(m.In, "init") {
			t.Errorf("init has no PRINTLN line; got %+v", nodes(q))
		}
	}
	// The boolean form: :contains ≡ :where(::grep).
	a := query(t, s, map[string]any{"selector": `method:contains('fmt.Println')`, "limit": 50})
	b := query(t, s, map[string]any{"selector": `method:where(::grep('fmt.Println'))`, "limit": 50})
	if strings.Join(nodes(a), "|") != strings.Join(nodes(b), "|") || len(nodes(a)) == 0 {
		t.Errorf(":contains and :where(::grep) must agree; got %v vs %v", nodes(a), nodes(b))
	}
}

func TestModernQueryGrepFieldIsGone(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": "*", "grep": "derp"})
	if !strings.Contains(msg, "::grep") {
		t.Errorf("the retired grep field should point at ::grep; got %q", msg)
	}
	// Unsupported flags stay guided errors, now at parse time.
	if msg := queryErr(t, s, map[string]any{"selector": `*::grep('-r derp')`}); !strings.Contains(msg, "-r") {
		t.Errorf("expected a guided error naming -r; got %q", msg)
	}
}

func TestModernQueryGrepLiteralByDefaultRegexWithE(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Default is a LITERAL substring: regex metachars match nothing.
	q := query(t, s, map[string]any{"selector": `method#Start::grep('fmt.P.intln')`})
	if q.TotalMatches != 0 {
		t.Errorf("default should be literal, not regex; got %v", nodes(q))
	}
	// -E opts into a regex; the pattern is verbatim after the flags.
	q = query(t, s, map[string]any{"selector": `method#Start::grep('-E fmt.P.intln')`})
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

	// Default limit is 20 — small on purpose. A plain `func` is the
	// cappable shape, so the walk STOPS at 20 rather than finding all 25:
	// that is the point of short-circuiting, and it costs the exact
	// total. The count becomes a FLOOR, reported under a different key
	// and spelled ">N" so it cannot be misread as exact.
	q := query(t, s, map[string]any{"selector": "func"})
	if q.Returned != 20 || len(q.Matches) != 20 {
		t.Fatalf("want 20 returned, got %d", q.Returned)
	}
	if q.TotalMatches != 0 {
		t.Errorf("a short-circuited walk must NOT report an exact total, got %d", q.TotalMatches)
	}
	if q.TotalAtLeast != ">20" {
		t.Errorf("totalMatchesAtLeast = %q, want \">20\"", q.TotalAtLeast)
	}
	if !q.Truncated {
		t.Error("truncated should be set when more matches exist")
	}
	if !strings.Contains(q.Note, "STOPPED at the limit") {
		t.Errorf("note = %q, want it to say the walk stopped early", q.Note)
	}

	// A limit that can hold everything still reports an EXACT total —
	// the walk ran to completion, so there is nothing to hedge.
	qAll := query(t, s, map[string]any{"selector": "func", "limit": 100})
	if qAll.TotalMatches != 25 || qAll.TotalAtLeast != "" {
		t.Errorf("uncapped run: total=%d atLeast=%q, want exact 25", qAll.TotalMatches, qAll.TotalAtLeast)
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

	r := s.callTool("node_edit", map[string]any{"node": "main.go#Free", "rename": "Freed"})
	if r.IsError {
		t.Fatalf("rename errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(got), "func Freed(") || strings.Contains(string(got), "func Free(") {
		t.Errorf("rename didn't apply:\n%s", got)
	}
	// The result must read as TERMINAL: a model that sees it should not
	// re-rename per-file or hand-patch afterwards (the dogfood failure).
	if !strings.Contains(r.Content[0].Text, "DONE") || !strings.Contains(r.Content[0].Text, "ONE call") {
		t.Errorf("rename result should state it is workspace-wide and done; got %s", r.Content[0].Text)
	}
}

// A rename needs a SYMBOL: a whole-file address (or a ref/::grep/span) has
// no identifier, so renaming one must ERROR with a pointer to the symbol
// form — not silently rewrite whatever token sits on the span's first line
// and report filesChanged:0 (the dogfood bug).
func TestModernNodeEditRenameRejectsNonSymbol(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{"node": "main.go", "rename": "Whatever"})
	if !r.IsError {
		t.Fatalf("rename on a whole file should error; got %s", r.Content[0].Text)
	}
	if !strings.Contains(r.Content[0].Text, "SYMBOL address") {
		t.Errorf("error should point at the symbol form; got %s", r.Content[0].Text)
	}
}

// A LEXICAL rename of a name declared in more than one package by unrelated
// symbols must be BLOCKED, not silently applied to all of them — the guard
// against the ab_bench islive-rename corruption (renaming payments.Gateway.
// IsLive also rewrote the unrelated llm interfaces). Names declared in ONE
// package (an interface + its impls) are unaffected and still rename.
func TestModernNodeEditRenameBlocksCrossPackageCollision(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	// Two unrelated packages that each declare a method named Ping.
	write("gw/gw.go", "package gw\n\ntype A struct{}\n\nfunc (A) Ping() bool { return true }\n")
	write("llm/llm.go", "package llm\n\ntype B struct{}\n\nfunc (B) Ping() bool { return false }\n")

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_edit", map[string]any{"node": "gw/gw.go#A.Ping", "rename": "Pong"})
	if !r.IsError {
		t.Fatalf("cross-package name collision must block, not apply; got %s", r.Content[0].Text)
	}
	if !strings.Contains(r.Content[0].Text, "rename-blocked") ||
		!strings.Contains(r.Content[0].Text, "llm/llm.go") {
		t.Errorf("block should name the colliding declaration; got %s", r.Content[0].Text)
	}
	// And nothing was written — Ping survives in BOTH files.
	for _, f := range []string{"gw/gw.go", "llm/llm.go"} {
		got, _ := os.ReadFile(filepath.Join(dir, f))
		if !strings.Contains(string(got), "Ping") || strings.Contains(string(got), "Pong") {
			t.Errorf("%s was mutated by a blocked rename:\n%s", f, got)
		}
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

// Short-circuiting: `--limit 5` must not pay for the whole workspace.
//
// The win is per-FILE, not per-node: finding a match in a file means
// loading that file's symbols, so the cap pays off by never reaching the
// later files at all. Measured while writing this — on a single fat file
// the saving is marginal, because the one load dominates either way.
func TestModernQueryShortCircuitsTheWalk(t *testing.T) {
	dir := t.TempDir()
	// Many files, few symbols each: the shape where stopping early means
	// never loading the rest.
	for f := 0; f < 120; f++ {
		body := fmt.Sprintf("package p%03d\n\nfunc F%03d() {}\n", f, f)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.go", f)),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Ample for a handful of files, far too small for 120.
	const tight = "200ops"
	q := query(t, s, map[string]any{"selector": "func", "limit": 5, "budget": tight})
	if q.Returned != 5 {
		t.Errorf("capped query returned %d, want 5 — the walk should stop, not blow the budget", q.Returned)
	}
	if strings.Contains(q.Note, "work budget") {
		t.Errorf("the capped walk should finish inside the budget; note = %q", q.Note)
	}
	if q.TotalAtLeast != ">5" {
		t.Errorf("totalMatchesAtLeast = %q, want \">5\"", q.TotalAtLeast)
	}

	// The same query without a small limit has to reach every file, and
	// on this budget that means an incomplete result.
	full := query(t, s, map[string]any{"selector": "func", "limit": 100000, "budget": tight})
	if !strings.Contains(full.Note, "work budget") {
		t.Errorf("uncapped query should exhaust the tight budget; note = %q", full.Note)
	}
}

// The cap is only sound where evaluation is one document-ordered walk.
// Anything composed through sets — a union, a chain, a pseudo-class that
// consults other nodes — must fall back to a full evaluation and an
// EXACT total, or a page could silently omit a node that sorts ahead of
// one shown.
func TestModernQueryDoesNotShortCircuitComposedSelectors(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "\nfunc F%02d(a int) {}\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	for _, sel := range []string{
		"func:has(argument)", // pseudo-class consults other nodes
		"func, argument",     // union merges two sets
		"func argument",      // descendant chain
	} {
		q := query(t, s, map[string]any{"selector": sel, "limit": 5})
		if q.TotalAtLeast != "" {
			t.Errorf("%s: reported a floor %q; composed selectors must evaluate fully",
				sel, q.TotalAtLeast)
		}
		if q.TotalMatches == 0 {
			t.Errorf("%s: expected an exact total, got 0", sel)
		}
	}
}

// The corpus below is REAL: every selector here was written by a model driving
// this server in a recorded session, and every one of them failed. Across ~205
// node_query calls, 39 distinct selectors errored, and they were not evenly
// spread — 25 were a bare file path where an id belongs and 8 were a grep
// pattern quoted twice. Those two classes are 85% of all selector failures.
//
// The test asserts the error CORRECTS the caller. Naming the mistake is not
// enough: the old unknown-type error told a path-writer to try "#terminal-view"
// (the first segment), they followed it, and hit a different wall — that exact
// pair is in the corpus.
func TestSelectorCorpus_ErrorsNameTheFix(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	cases := []struct {
		name     string
		selector string
		want     []string // substrings the error must contain
	}{{
		name:     "bare path in tag position",
		selector: "terminal-view/src/main/java/com/termux/view/TerminalView.java",
		want:     []string{"is a PATH", "#'terminal-view/src/main/java/com/termux/view/TerminalView.java'"},
	}, {
		name:     "bare path with a pseudo-element after it",
		selector: "app/src/main/java/Foo.java::grep('bar')",
		want:     []string{"is a PATH", "#'app/src/main/java/Foo.java'"},
	}, {
		name:     "unquoted id containing a path — the follow-on mistake",
		selector: "#termux-shared/src/main/java/Foo.java",
		want:     []string{"is a PATH", "#'termux-shared/src/main/java/Foo.java'"},
	}, {
		name:     "a lone filename is still a path",
		selector: "sum.mjs",
		want:     []string{"is a PATH", "#'sum.mjs'"},
	}, {
		name:     "misremembered pseudo-class",
		selector: "func:arg:contains(x)",
		want:     []string{"did you mean :arity"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := queryErr(t, s, map[string]any{"selector": c.selector})
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error does not carry the fix %q\ngot: %s", w, msg)
				}
			}
			if len(msg) > 1500 {
				t.Errorf("a corrected mistake should be answered, not buried: %d bytes", len(msg))
			}
		})
	}
}

// The silent one. A pattern quoted twice used to parse fine, match nothing, and
// report totalMatches:0 — indistinguishable from "that code is not here", which
// is how a session ends up looking somewhere else entirely.
//
// It is now ANSWERED rather than rejected: the caller gets the search they
// plainly meant. The note is what keeps it from being the old silence with a
// friendlier face — matching while saying nothing would teach the shell habit
// instead of correcting it.
func TestDoubleQuotedGrepIsAnsweredAndAnnounced(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// A pattern that DOES occur, wrapped the way a shell habit wraps it.
	raw := query(t, s, map[string]any{"selector": `file::grep('-E "func|type"')`})
	if raw.TotalMatches == 0 {
		t.Fatalf("a double-quoted pattern must still find what it meant; got 0 matches")
	}
	// Same query without the inner quotes: identical result set.
	clean := query(t, s, map[string]any{"selector": `file::grep('-E func|type')`})
	if raw.TotalMatches != clean.TotalMatches {
		t.Errorf("stripped pattern should search identically: quoted=%d bare=%d",
			raw.TotalMatches, clean.TotalMatches)
	}
	if !strings.Contains(raw.Note, "quoted twice") {
		t.Errorf("a normalised pattern must say so; note=%q", raw.Note)
	}
	if clean.Note != "" && strings.Contains(clean.Note, "quoted twice") {
		t.Errorf("an unquoted pattern must not claim it was normalised; note=%q", clean.Note)
	}

	// The legitimate case still works: quotes INSIDE a pattern are content,
	// never stripped, never announced.
	inner := query(t, s, map[string]any{"selector": `file::grep('printf("%d")')`})
	if strings.Contains(inner.Note, "quoted twice") {
		t.Errorf("quotes inside a pattern are content, not wrapping; note=%q", inner.Note)
	}
}

// The rest of the corpus's shell habits: flags as a separate argv word, and an
// absolute path (doubly wrong — addresses are workspace-relative).
func TestSelectorCorpus_ShellHabits(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": `::grep('-E' 'sum.mjs')`})
	if !strings.Contains(msg, "ONE quoted argument") || !strings.Contains(msg, `::grep('-E sum.mjs')`) {
		t.Errorf("separate-word flags should be corrected inline; got %s", msg)
	}
	abs := queryErr(t, s, map[string]any{"selector": "/tmp/ws/cmd/dun/tui.go"})
	if !strings.Contains(abs, "workspace-RELATIVE") {
		t.Errorf("an absolute path should say addresses are relative; got %s", abs)
	}
}

// Bracket-free attributes. Brackets were never the problem — QUOTING was: a
// path is the commonest thing to name and the fiddliest to write, so callers
// wrote it bare and got an error. Unbracketed, the value runs to whitespace, so
// a path needs no quoting at all.
func TestBareAttribute(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// The shape that replaces #'a/b.go': scope by path, no quotes.
	bare := query(t, s, map[string]any{"selector": "path=main.go func", "limit": 50})
	brack := query(t, s, map[string]any{"selector": "#'main.go' func", "limit": 50})
	if bare.TotalMatches == 0 {
		t.Fatalf("path=main.go func matched nothing")
	}
	if got, want := nodes(bare), nodes(brack); len(got) != len(want) {
		t.Errorf("bare and id forms disagree:\n bare %v\n  id  %v", got, want)
	}
	// Every operator the bracketed form has.
	for _, sel := range []string{"name^=Serve", "name*=erve", "path$=.go", "path~=ma+in"} {
		if r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 5}); r.IsError {
			t.Errorf("%s: %s", sel, r.Content[0].Text)
		}
	}
	// A space is a NODE BOUNDARY, always. `file path=main.go` is two
	// elements — files, then anything at that path inside them — exactly what
	// it looks like. It used to ATTACH the attribute to the element before it,
	// so the same space meant "descend" before a tag and "filter" before an
	// attribute, with nothing in the result saying which you got. That rule
	// lived only in the `?` grammar help, which 2 of 426 real calls ever
	// asked for, while the always-present tool description said
	// "space=descendant" and nothing else.
	desc := query(t, s, map[string]any{"selector": "file path=main.go", "limit": 50})
	attached := query(t, s, map[string]any{"selector": "file[path=main.go]", "limit": 50})
	if len(nodes(desc)) <= len(nodes(attached)) {
		t.Errorf("a space should descend, not filter: `file path=main.go`=%v vs `file[path=main.go]`=%v",
			nodes(desc), nodes(attached))
	}
	if got := nodes(attached); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("the bracketed form is the filter, got %v", got)
	}
	// A bare attribute is sugar for `*[…]` — one element, same as writing it.
	if a, b := nodes(query(t, s, map[string]any{"selector": "file > path=main.go", "limit": 50})),
		nodes(query(t, s, map[string]any{"selector": "file > *[path=main.go]", "limit": 50})); len(a) != len(b) {
		t.Errorf("a bare attr must equal *[attr]: %v vs %v", a, b)
	}
	// And the two readings are now VISIBLY different, which is the point:
	// `> * path=x` is three elements (a child, then a descendant filtered by
	// path), `> *[path=x]` is one filtered child. Under the old attach rule
	// they were the same query written two ways, and nothing said so.
	if a, b := nodes(query(t, s, map[string]any{"selector": "file > * path=main.go", "limit": 50})),
		nodes(query(t, s, map[string]any{"selector": "file > *[path=main.go]", "limit": 50})); len(a) == len(b) {
		t.Errorf("`> * path=x` descends and `> *[path=x]` filters — they should differ: %v vs %v", a, b)
	}
	// An unknown attribute name is still an error, not a silent tag.
	if msg := queryErr(t, s, map[string]any{"selector": "bogus=x"}); !strings.Contains(msg, "unknown attribute") {
		t.Errorf("bare attrs must validate the name; got %s", msg)
	}
}

// The path error should teach the form that needs no quoting.
func TestPathErrorTeachesBareAttribute(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	msg := queryErr(t, s, map[string]any{"selector": "src/main/Foo.java"})
	if !strings.Contains(msg, "path=src/main/Foo.java") {
		t.Errorf("the corrected selector should lead with the bare attribute; got %s", msg)
	}
}

// A regex searched literally is the worst kind of wrong answer: correct to the
// question asked, useless to the question meant, and indistinguishable from
// "it isn't there". In one recorded session 44 of 49 empty results were this,
// and the same pattern was retried verbatim ten times because zero results give
// nothing to correct.
func TestLiteralRegexNote(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		want     bool
	}{
		{"regex without -E", `::grep('func.*tuiModel.*refresh')`, true},
		{"alternation without -E", `path=cmd/dun ::grep('slashCmd|commandNamed')`, true},
		{"escaped parens without -E", `::grep('func \(m \*tuiModel\) refresh')`, true},
		{"explicitly a regex", `::grep('-E func.*refresh')`, false},
		{"explicitly literal", `::grep('-F func.*refresh')`, false},
		{"an ordinary word", `::grep('refresh')`, false},
		{"a dotted name is not a regex", `::grep('strings.Join')`, false},
		{"no grep at all", `func[name=refresh]`, false},
	}
	for _, c := range cases {
		got := literalRegexNote(c.selector) != ""
		if got != c.want {
			t.Errorf("%s: note=%v, want %v (%s)", c.name, got, c.want, c.selector)
		}
	}
	// The note has to carry the corrected call, not just a diagnosis.
	n := literalRegexNote(`::grep('func.*refresh')`)
	if !strings.Contains(n, "-E func.*refresh") {
		t.Errorf("note should spell out the fix: %s", n)
	}
}

// The corpus's SILENT class. Every selector below is syntactically perfect,
// matches nothing, and used to return {"matches":[],"returned":0} — the same
// silent no-op the parser refuses at parse time, arriving through the one door
// still open.
//
// Three of the four came from a dun session that fixed a real data race with
// these tools (2026-08-02); 4 of its 17 node_query calls hit nothing. The one
// that matters is `method name~=newInputStream` (∅) followed immediately by
// `func name~=newInputStream` (2 matches): the caller had to know the tag
// BEFORE it could ask, and guessing wrong returns a result byte-identical to
// the symbol not existing.
func TestSelectorCorpus_ZeroResultNamesTheClause(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	cases := []struct {
		name     string
		selector string
		want     []string
	}{{
		// The dun case, transposed onto the fixture: Start is a method.
		name:     "wrong tag, right filter",
		selector: "func[name=Start]",
		want:     []string{"no func matches", "TAG", "method", "main.go#Server.Start"},
	}, {
		name:     "wrong tag the other way",
		selector: "method[name=topLevel]",
		want:     []string{"no method matches", "TAG", "func", "web/some_file.ts#topLevel"},
	}, {
		// dun guessed a filename from the TEST name; the symbol lived in
		// main.go. The directory was right, so the nearest sibling is a
		// correction rather than a shot in the dark.
		name:     "invented filename in a real directory",
		selector: "path=web/some_fil.ts",
		want:     []string{"no file or dir is at path=web/some_fil.ts", "web/some_file.ts"},
	}, {
		name:     "path that names nothing at all",
		selector: "path=cmd/dun/inputstream.go",
		want:     []string{"no file or dir matches path=cmd/dun/inputstream.go", "workspace-relative"},
	}, {
		name:     "misspelt name",
		selector: "name=Stat",
		want:     []string{`nothing is named "Stat"`, "main.go#Server.Start", `Retry with "Start"`},
	}, {
		// The general form: two clauses, and the hint says which one did it.
		name:     "one clause of several",
		selector: "func[path=web/some_file.ts][name^=Call]",
		want:     []string{"[name^=Call] filter is what emptied it", "web/some_file.ts#topLevel"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := query(t, s, map[string]any{"selector": c.selector})
			if q.TotalMatches != 0 {
				t.Fatalf("fixture drift: %s matches %d, this corpus is about ZERO results",
					c.selector, q.TotalMatches)
			}
			for _, w := range c.want {
				if !strings.Contains(q.Hint, w) {
					t.Errorf("hint does not carry %q\ngot: %s", w, q.Hint)
				}
			}
			// ONE line, ONE alternative. A hint that lists candidates is a
			// search result wearing an error's clothes, and callers would read
			// hints instead of narrowing selectors.
			if strings.Contains(q.Hint, "\n") || len(q.Hint) > 240 {
				t.Errorf("hint should be one short line, got %d bytes: %s", len(q.Hint), q.Hint)
			}
		})
	}
}

// A hint is an explanation, not a second answer — so it must stay quiet
// wherever it cannot name a clause honestly, and must never disturb what the
// caller is told about their OWN query.
func TestZeroResultHintStaysQuiet(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// A result that isn't empty has nothing to explain.
	if q := query(t, s, map[string]any{"selector": "func"}); q.Hint != "" {
		t.Errorf("a non-empty result carries no hint; got %q", q.Hint)
	}

	// A budget blow already explains the emptiness. Blaming the selector for
	// the clock would be a lie, and the probe would be spending a budget the
	// caller has already run out of.
	blown := query(t, s, map[string]any{"selector": "func[name=Nope]", "budget": "1ops"})
	if blown.TotalMatches != 0 || !strings.Contains(blown.Note, "work budget") {
		t.Fatalf("expected a budget-blown empty result; total=%d note=%q",
			blown.TotalMatches, blown.Note)
	}
	if blown.Hint != "" {
		t.Errorf("a budget blow must not be reported as a selector mistake; got %q", blown.Hint)
	}

	// A union's emptiness has no single clause to name.
	if q := query(t, s, map[string]any{"selector": "func name=Stat, method name=Stat"}); q.Hint != "" {
		t.Errorf("a union carries no hint; got %q", q.Hint)
	}

	// ::grep has its own answer for an empty result (literalRegexNote) and a
	// generated element has no containment clause to relax.
	grep := query(t, s, map[string]any{"selector": `file::grep('func.*Nope')`})
	if grep.TotalMatches != 0 {
		t.Fatalf("fixture drift: the literal pattern matched %d", grep.TotalMatches)
	}
	if !strings.Contains(grep.Note, "searched LITERALLY") {
		t.Errorf("the grep note should still fire; got %q", grep.Note)
	}
	if grep.Hint != "" {
		t.Errorf("::grep is answered by its own note, not a clause hint; got %q", grep.Hint)
	}

	// A lone filter has no OTHER clause to blame. Relaxing `func name^=Zzz`
	// to `func` would report that some unrelated func exists, which reads as
	// an answer and says only "your filter filtered".
	if q := query(t, s, map[string]any{"selector": "func[name^=Zzz]"}); q.Hint != "" {
		t.Errorf("a single narrowing clause has nothing to name; got %q", q.Hint)
	}
	// Add a second clause and the hint becomes information again.
	two := query(t, s, map[string]any{"selector": "func[path=main.go][name^=Zzz]"})
	if !strings.Contains(two.Hint, "[name^=Zzz] filter is what emptied it") {
		t.Errorf("two clauses, one culprit — say which; got %q", two.Hint)
	}

	// An occurrence is not a declaration. Println is indexed (main.go calls it)
	// but declared in the stdlib, so suggesting it would hand back a retry that
	// returns zero again — worse than the silence it replaced.
	if q := query(t, s, map[string]any{"selector": "name=Printn"}); q.Hint != "" {
		t.Errorf("a suggestion must be a real declaration; got %q", q.Hint)
	}

	// The probe runs on the live engine. It must leave no trace: no phantom
	// truncation, no cost trace, no budget note stolen from its own run.
	hinted := query(t, s, map[string]any{"selector": "func name=Start"})
	if hinted.Hint == "" {
		t.Fatal("expected a hint to have been probed for")
	}
	if hinted.Truncated || hinted.Note != "" || len(hinted.Cost) != 0 {
		t.Errorf("the probe leaked into the caller's own result: truncated=%v note=%q cost=%v",
			hinted.Truncated, hinted.Note, hinted.Cost)
	}
}

// `#X::out > method` is a habit the 39-selector corpus never saw: dun invented
// it mid-session and got ∅. It is legal syntax — the far end of an out-edge,
// filtered to methods — so it must be ANSWERED, not rejected.
//
// An empty edge result now names its clause too. The probe re-runs the chain
// against edges the caller's own query ALREADY materialized (they are memoized
// on the tree for the life of a query), so it costs no child-LSP round-trip;
// a probe that would need new edge work is discarded instead — see
// TestProbeNeverDoesNewEdgeWork.
func TestSelectorCorpus_EdgeFarEndTag(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": "#'main.go#CallsStart'::out > method"})
	if !hasNode(q, "main.go#Server.Start") {
		t.Errorf("::out > method should reach the called method; got %v", nodes(q))
	}

	cases := []struct {
		name     string
		selector string
		want     []string
	}{{
		// The dead ANCHOR. This is the damaging one: the tool's own recipes
		// teach "0 matches = unused", so a misspelt symbol reads as a fact
		// about the code — `#Nope::in` said "nothing calls it" when it should
		// have said "there is no such thing".
		name:     "the anchor does not exist",
		selector: "#Nope::in",
		want:     []string{"#Nope matches nothing", "rest of the chain never ran"},
	}, {
		name:     "dead anchor with a far-end tag behind it",
		selector: "#Nope::out > method",
		want:     []string{"#Nope matches nothing"},
	}, {
		// The far end's tag, same mistake as `method name~=newInputStream`.
		name:     "wrong tag on the far end",
		selector: "#'main.go#CallsStart'::out > interface",
		want:     []string{"no interface matches", "TAG", "struct #'main.go#Server'"},
	}, {
		// The edge spelling of the same guess-before-you-ask: which KIND of
		// use is it? A wrong kind returns the same ∅ as no edges at all.
		name:     "wrong kind class",
		selector: "#'main.go#Server.Start'::in.type",
		want:     []string{"no ::in.type edge here", "KIND class", "::in.call", "main.go@"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := query(t, s, map[string]any{"selector": c.selector})
			if e.TotalMatches != 0 {
				t.Fatalf("fixture drift: %s matches %d", c.selector, e.TotalMatches)
			}
			for _, w := range c.want {
				if !strings.Contains(e.Hint, w) {
					t.Errorf("hint does not carry %q\ngot: %s", w, e.Hint)
				}
			}
			if strings.Contains(e.Hint, "\n") || len(e.Hint) > 240 {
				t.Errorf("hint should be one short line, got %d bytes: %s", len(e.Hint), e.Hint)
			}
		})
	}

	// A symbol with genuinely no incoming edges gets NO hint: the empty result
	// IS the answer ("0 matches = unused"), and inventing an explanation for a
	// correct answer would teach the caller to distrust it.
	if unused := query(t, s, map[string]any{"selector": "#'main.go#Free'::in.call"}); unused.Hint != "" {
		t.Errorf("a truly unused symbol needs no hint; got %q", unused.Hint)
	}
}

// dun keeps per-session git WORKTREES in .dun — copies of the workspace. Left
// indexed, every workspace-wide query in a real repo came back 100% stale
// duplicates.
func TestSkipScanDir_ToolState(t *testing.T) {
	for _, d := range []string{".git", ".dun", ".poly-lsp-mcp", "node_modules"} {
		if !skipScanDir(d) {
			t.Errorf("%s should never be indexed", d)
		}
	}
	for _, d := range []string{"cmd", "internal", ".github", "src"} {
		if skipScanDir(d) {
			t.Errorf("%s is real source and must be indexed", d)
		}
	}
	// A bench probe's seed workspace. bench/probes/find-render-entrypoints
	// pins a snapshot of THIS package as its corpus, so without the skip a
	// workspace query returns the live symbol and a stale copy of it —
	// indistinguishable, and one of them is wrong.
	if !skipScanDir("_fixture") {
		t.Error("_fixture is a probe seed workspace, not source of this repo")
	}
	// Narrow on purpose: the marker is the fixture convention, not the
	// underscore. Jekyll's _posts is real content a markdown query should reach.
	for _, d := range []string{"_posts", "_layouts", "_internal"} {
		if skipScanDir(d) {
			t.Errorf("%s is not a fixture dir and must stay indexed", d)
		}
	}
}

// oldText+newText on a FILE node (not a symbol) must rewrite the whole
// file correctly — including when newText is larger than oldText and
// appends content to the end. This reproduces a real failure mode where
// the response reported success but the appended content was lost on disk.
func TestModernNodeEditFileNodeAppend(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// Read the current file.
	before, err := os.ReadFile(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	beforeStr := string(before)

	// Append a block to the end of the file using oldText+newText on the
	// file node (no symbol). oldText matches the last line; newText
	// replaces it with the last line PLUS new content.
	oldText := "# hello"
	newText := "# hello\n\n## appended\n\nThis line was added by node_edit.\n"
	r := s.callTool("node_edit", map[string]any{
		"node":    "notes.md",
		"oldText": oldText,
		"newText": newText,
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %s", r.Content[0].Text)
	}

	// Verify the file on disk has the appended content.
	after, err := os.ReadFile(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	afterStr := string(after)

	// The old content should still be there (replaced, not removed).
	if !strings.Contains(afterStr, "## appended") {
		t.Errorf("appended section missing from disk.\nFile content:\n%s", afterStr)
	}
	if !strings.Contains(afterStr, "This line was added by node_edit.") {
		t.Errorf("appended line missing from disk.\nFile content:\n%s", afterStr)
	}

	// The file should be larger than before.
	if len(afterStr) <= len(beforeStr) {
		t.Errorf("file did not grow: was %d bytes, now %d bytes", len(beforeStr), len(afterStr))
	}

	// Verify the exact expected content.
	expected := "# hello\n\n## appended\n\nThis line was added by node_edit.\n\n"
	if afterStr != expected {
		t.Errorf("file content mismatch.\nwant %q\ngot  %q", expected, afterStr)
	}
}

// oldText+newText on a file node where newText replaces a middle snippet
// with a much larger block must not truncate the tail.
func TestModernNodeEditFileNodeExpandMiddle(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// Start with a known file.
	file := filepath.Join(dir, "expand.txt")
	orig := "line1\nline2\nline3\nline4\n"
	if err := os.WriteFile(file, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Replace "line2" with a much longer block.
	r := s.callTool("node_edit", map[string]any{
		"node":    "expand.txt",
		"oldText": "line2",
		"newText": "line2-expanded\nline2-extra\nline2-more",
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %s", r.Content[0].Text)
	}

	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	// Tail must be preserved.
	if !strings.Contains(string(after), "line3\nline4\n") {
		t.Errorf("tail was truncated.\nFile:\n%s", string(after))
	}
	// Head must be preserved.
	if !strings.HasPrefix(string(after), "line1\n") {
		t.Errorf("head was damaged.\nFile:\n%s", string(after))
	}
	// Expansion must be present.
	if !strings.Contains(string(after), "line2-expanded") {
		t.Errorf("expansion missing.\nFile:\n%s", string(after))
	}
}

// A field's trailing comment is part of the field, so an edit naming it in
// oldText applies. Reported from dogfooding: the agent wrote the field line
// as it appears on screen, comment included, and got "oldText not found"
// because the node stopped at the declaration.
func TestModernNodeEditFieldTrailingComment(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	src := "package main\n\ntype Config struct {\n" +
		"\tName       string // the harness name\n" +
		"\tEnableShip bool   // add the ship tool (enabled by default)\n" +
		"\tTimeout    int\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "harness.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	r := s.callTool("node_read", map[string]any{"node": "harness.go#Config.EnableShip"})
	if r.IsError {
		t.Fatalf("node_read errored: %s", r.Content[0].Text)
	}
	if !strings.Contains(r.Content[0].Text, "add the ship tool") {
		t.Errorf("the field's own comment is missing from its text: %s", r.Content[0].Text)
	}
	// ...and it must NOT have taken the comment belonging to the line above.
	if strings.Contains(r.Content[0].Text, "the harness name") {
		t.Errorf("field took the PREVIOUS field's comment: %s", r.Content[0].Text)
	}

	e := s.callTool("node_edit", map[string]any{
		"node":    "harness.go#Config.EnableShip",
		"oldText": "EnableShip bool   // add the ship tool (enabled by default)",
		"newText": "EnableShip bool   // add the ship tool (opt-in)",
	})
	if e.IsError {
		t.Fatalf("the reported edit still fails: %s", e.Content[0].Text)
	}

	after, err := os.ReadFile(filepath.Join(dir, "harness.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\ntype Config struct {\n" +
		"\tName       string // the harness name\n" +
		"\tEnableShip bool   // add the ship tool (opt-in)\n" +
		"\tTimeout    int\n}\n"
	if string(after) != want {
		t.Errorf("file after edit:\nwant %q\ngot  %q", want, string(after))
	}
}

// Deleting a declaration takes its own trailing comment with it and leaves
// the neighbour's alone. Before the span fix this did the exact opposite in
// typescript: it destroyed the comment above and stranded its own.
func TestModernNodeDeleteTakesOwnTrailingComment(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	src := "export class Config {\n" +
		"  name: string = \"\"; // the harness name\n" +
		"  enableShip = false; // add the ship tool\n" +
		"  timeout = 0;\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "web/conf.ts"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if r := s.callTool("node_edit", map[string]any{
		"node": "web/conf.ts#Config.enableShip", "delete": true,
	}); r.IsError {
		t.Fatalf("delete errored: %s", r.Content[0].Text)
	}

	after, err := os.ReadFile(filepath.Join(dir, "web/conf.ts"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(after)
	if !strings.Contains(got, "// the harness name") {
		t.Errorf("delete destroyed the PREVIOUS field's comment:\n%s", got)
	}
	if strings.Contains(got, "// add the ship tool") {
		t.Errorf("delete stranded the deleted field's own comment:\n%s", got)
	}
	if !strings.Contains(got, "timeout = 0;") {
		t.Errorf("delete damaged the field below:\n%s", got)
	}
}
