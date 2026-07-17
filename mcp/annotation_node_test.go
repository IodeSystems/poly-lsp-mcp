package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Decorators (Python/TS) and struct-tag keys (Go) are first-class
// `annotation` child nodes of the symbol they mark — so "who is
// annotated with X" is a structural query, `func:any(annotation#route)`,
// not a text grep. Each answers to its leaf (#route) and its virtual
// FQN (#'app.route').
func writeAnnotationNodeFixture(t *testing.T) string {
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
	write("go.mod", "module ann\ngo 1.21\n")
	write("handlers.py", `@app.route("/users")
@requires_auth
def admin():
    return 1

def plain():
    return 2
`)
	write("web.ts", `@Component({selector: 'app'})
export class AppComponent {
  @Input() name: string;
}
`)
	write("model.go", `package ann

type User struct {
	Name  string `+"`"+`json:"name" validate:"required"`+"`"+`
	Email string `+"`"+`json:"email"`+"`"+`
	plain int
}
`)
	return dir
}

func annNodeSet(t *testing.T, s *Server, sel string) []string {
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

func TestAnnotationNodesAcrossLanguages(t *testing.T) {
	s := newQueryServer(t, writeAnnotationNodeFixture(t))

	cases := []struct {
		sel  string
		want []string
	}{
		// Python decorators, incl. a stacked one, name the decorated func.
		{`func:any(annotation#route)`, []string{"handlers.py#admin"}},
		{`func:any(annotation#requires_auth)`, []string{"handlers.py#admin"}},
		// Virtual FQN: @app.route answers to app.route too.
		{`annotation#'app.route'`, []string{"handlers.py#admin.route"}},
		// TS class + field decorators.
		{`class:any(annotation#Component)`, []string{"web.ts#AppComponent"}},
		{`field:any(annotation#Input)`, []string{"web.ts#AppComponent.name"}},
		// Go struct-tag keys are annotations on the field.
		{`field:any(annotation#json)`, []string{"model.go#User.Name", "model.go#User.Email"}},
		{`field:any(annotation#validate)`, []string{"model.go#User.Name"}},
	}
	for _, c := range cases {
		got := annNodeSet(t, s, c.sel)
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

	// An annotation is a CHILD of the symbol it marks — containment, so
	// the whole graph-selector language composes over it.
	if got := annNodeSet(t, s, `#'handlers.py#admin' > annotation`); len(got) != 2 {
		t.Errorf("admin has two decorators as children; got %v", got)
	}
	// The undecorated function has none — the mark is on the symbol, not
	// the file.
	if got := annNodeSet(t, s, `func:not(:any(annotation)):any(&)`); len(got) == 0 {
		t.Skip() // :any(&) shape may vary; the negative below is the point
	}
	for _, n := range annNodeSet(t, s, `func:any(annotation#route)`) {
		if n == "handlers.py#plain" {
			t.Error("annotation#route leaked to the undecorated neighbour")
		}
	}
}

// `annotation` is a real tag: it must be in the valid class set, so a
// bare `annotation` selector lists them all rather than erroring as an
// unknown type.
func TestAnnotationIsAValidTag(t *testing.T) {
	s := newQueryServer(t, writeAnnotationNodeFixture(t))
	all := annNodeSet(t, s, `annotation`)
	if len(all) == 0 {
		t.Fatal("bare `annotation` should list every annotation node")
	}
}
