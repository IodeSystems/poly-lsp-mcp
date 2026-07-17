package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// :annotated greps the decorator / annotation / doc block attached to a
// declaration — the SYMBOL carrying the mark, which ::grep (line nodes)
// and :contains (the body) cannot name. The trivia lands in different
// places per language: above the span for Python/TS decorators, inside
// it for Go, where the doc comment is folded into the declaration. One
// predicate must cover both.
func writeAnnotatedFixture(t *testing.T) string {
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
	write("go.mod", "module anno\ngo 1.21\n")
	// Go: doc comment folded INTO the declaration span.
	write("svc.go", `package anno

// Deprecated: use NewClient.
func OldClient() {}

// NewClient builds a client.
func NewClient() {}
`)
	// Python: decorators ABOVE the span, stacked.
	write("app/handlers.py", `@app.route("/users")
def list_users():
    return []

@app.route("/admin")
@requires_auth
def admin_panel():
    return "secret"

def plain_helper():
    return 1
`)
	// TS: class decorator above the span.
	write("web.ts", `@Component({selector: 'app'})
export class AppComponent {
  ok() { return 1; }
}

export class Plain {
  no() { return 2; }
}
`)
	return dir
}

func annNodes(t *testing.T, s *Server, sel string) []string {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("%s: %v", sel, err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range e.evaluate(list) {
		out = append(out, r.addr())
	}
	sort.Strings(out)
	return out
}

func TestAnnotatedNamesTheDecoratedSymbol(t *testing.T) {
	s := newQueryServer(t, writeAnnotatedFixture(t))

	cases := []struct {
		sel  string
		want []string
	}{
		// Python decorators above the span, incl. a stacked one.
		{`func:annotated('@app.route')`, []string{"app/handlers.py#list_users", "app/handlers.py#admin_panel"}},
		{`func:annotated('@requires_auth')`, []string{"app/handlers.py#admin_panel"}},
		// TS class decorator.
		{`class:annotated('@Component')`, []string{"web.ts#AppComponent"}},
		// Go doc comment folded into the span — the tricky one.
		{`func:annotated('-w Deprecated')`, []string{"svc.go#OldClient"}},
	}
	for _, c := range cases {
		got := annNodes(t, s, c.sel)
		sort.Strings(c.want)
		if len(got) != len(c.want) {
			t.Errorf("%s = %v, want %v", c.sel, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s = %v, want %v", c.sel, got, c.want)
				break
			}
		}
	}

	// The mark is on the decorated symbol, not on its neighbours: a plain
	// function next to a decorated one must NOT match.
	for _, n := range annNodes(t, s, `func:annotated('@app.route')`) {
		if n == "app/handlers.py#plain_helper" {
			t.Error(":annotated leaked to an undecorated neighbour")
		}
	}

	// :not(:annotated) is the clean inverse — undecorated funcs.
	got := annNodes(t, s, `func:not(:annotated('@app.route')):not(:annotated('-w Deprecated'))`)
	for _, n := range got {
		if n == "app/handlers.py#list_users" || n == "svc.go#OldClient" {
			t.Errorf(":not(:annotated) kept a decorated symbol: %s", n)
		}
	}
}

// :contains greps the body, :annotated the block ON the decl. A marker
// that lives ONLY in the annotation must be found by :annotated and
// missed by :contains — otherwise the two predicates are redundant.
func TestAnnotatedIsDistinctFromContains(t *testing.T) {
	s := newQueryServer(t, writeAnnotatedFixture(t))
	// The decorator text is above the Python span, not in the body.
	if got := annNodes(t, s, `func:contains('@app.route')`); len(got) != 0 {
		t.Errorf(":contains greps the BODY; @app.route lives above it, got %v", got)
	}
	if got := annNodes(t, s, `func:annotated('@app.route')`); len(got) == 0 {
		t.Error(":annotated must see the decorator above the span")
	}
}

func TestAnnotatedMisspellingsAreGuided(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	for _, sel := range []string{`func:decorated('x')`, `func:tagged('x')`} {
		msg := queryErr(t, s, map[string]any{"selector": sel})
		if !strings.Contains(msg, ":annotated") {
			t.Errorf("%s should point at :annotated; got %s", sel, msg)
		}
	}
}
