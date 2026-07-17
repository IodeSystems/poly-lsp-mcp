package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// A TS type reference must count ONCE per occurrence — no doubling. An
// earlier ❓ (plan.md) reported `Widget::in.type` = 4 on a 2-use fixture,
// blamed on a TS-specific site dup. It no longer reproduces in any shape
// (interface / class / generic / export / .tsx / union / cross-file), so
// this pins the correct behavior: the count equals the real occurrences,
// split cleanly by the POSITION axis, with value refs (`new Widget()`)
// staying out of `.type`.
func writeTSInTypeFixture(t *testing.T, rel, body string) *Server {
	t.Helper()
	dir := t.TempDir()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newQueryServer(t, dir)
	s.SetQueryWorkBudget(50_000_000)
	return s
}

func TestTSInTypeNoDoubleCount(t *testing.T) {
	count := func(s *Server, sel string) int {
		list, err := parseModernSelector(sel)
		if err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		e, err := s.buildTree()
		if err != nil {
			t.Fatal(err)
		}
		return len(e.evaluate(list))
	}

	// Every shape: one type declared, used as a param type and a return
	// type — exactly 2 uses. None may double.
	shapes := map[string]string{
		"alias":     "type Widget = { id: number }\nfunction build(w: Widget): Widget { return w }\n",
		"interface": "interface Widget { id: number }\nfunction build(w: Widget): Widget { return w }\n",
		"export":    "export interface Widget { id: number }\nexport function build(w: Widget): Widget { return w }\n",
		"generic":   "type Widget = { id: number }\nfunction build(w: Array<Widget>): Widget { return w[0] }\n",
	}
	for name, body := range shapes {
		s := writeTSInTypeFixture(t, name+".ts", body)
		if n := count(s, `#'Widget'::in.type`); n != 2 {
			t.Errorf("%s: ::in.type must be exactly the 2 uses, not doubled; got %d", name, n)
		}
		if p, r := count(s, `#'Widget'::in.param.type`), count(s, `#'Widget'::in.return.type`); p != 1 || r != 1 {
			t.Errorf("%s: position split must be 1 param + 1 return; got param=%d return=%d", name, p, r)
		}
	}

	// .tsx path (class field + return) — the tsx grammar parses both, and
	// a class type use must not double either.
	tsx := writeTSInTypeFixture(t, "panel.tsx",
		"type Widget = { id: number }\nclass Panel { widget: Widget; make(): Widget { return this.widget } }\n")
	if n := count(tsx, `#'Widget'::in.type`); n != 2 {
		t.Errorf(".tsx class use doubled: got %d want 2", n)
	}

	// A `class` is BOTH a type and a value binding; `new Widget()` is a
	// VALUE ref (a call), not a type ref — it must stay OUT of ::in.type.
	cls := writeTSInTypeFixture(t, "cls.ts",
		"class Widget { id = 1 }\nfunction build(w: Widget): Widget { return new Widget() }\n")
	if n := count(cls, `#'Widget'::in.type`); n != 2 {
		t.Errorf("class type uses = 2 (param + return); new Widget() is a value ref, not a type; got %d", n)
	}
}
