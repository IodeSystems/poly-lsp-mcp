package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// A reference occurrence carries TWO orthogonal class axes: KIND
// (.call/.type/.import) and POSITION (.return/.param/.field/.var). They
// compose — ::in.return.type is a type reference in a return slot — so
// Server-as-a-return-type and Server-as-a-param-type, which used to be
// indistinguishable (both "func build"), are now separable. The ref is
// the occurrence; its far end (via >) is still the source symbol.
func writeRefPosFixture(t *testing.T) string {
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
	write("go.mod", "module rp\ngo 1.21\n")
	write("m.go", `package rp

type Server struct{ Name string }

func build(in *Server) *Server { return in }

type Config struct{ srv *Server }
`)
	return dir
}

func TestRefPositionAxis(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))
	s.SetQueryWorkBudget(50_000_000)

	count := func(sel string) int {
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

	// The bare kind is unchanged — every type use, all positions.
	if n := count(`#'Server'::in.type`); n != 3 {
		t.Errorf("::in.type should see all 3 type uses (param, return, field); got %d", n)
	}
	// Position splits them, one each.
	if n := count(`#'Server'::in.return.type`); n != 1 {
		t.Errorf("::in.return.type should be exactly the return use; got %d", n)
	}
	if n := count(`#'Server'::in.param.type`); n != 1 {
		t.Errorf("::in.param.type should be exactly the param use; got %d", n)
	}
	if n := count(`#'Server'::in.field.type`); n != 1 {
		t.Errorf("::in.field.type should be exactly the field use; got %d", n)
	}
	// Order-free, like CSS classes.
	if count(`#'Server'::in.type.return`) != count(`#'Server'::in.return.type`) {
		t.Error(".return.type and .type.return must be the same (classes compose order-free)")
	}
	// A position with no matching kind is empty, not an error (same-axis
	// AND of disjoint tags): no import is a return type here.
	if n := count(`#'Server'::in.return.import`); n != 0 {
		t.Errorf("no import is a return type; got %d", n)
	}

	// The far end is still the SOURCE symbol — the position is on the
	// occurrence, not below it. > * reaches the func; :parents recovers
	// it from deeper.
	dir := writeRefPosFixture(t)
	_ = dir
	list, _ := parseModernSelector(`#'Server'::in.return.type > *`)
	e, _ := s.buildTree()
	rows := e.evaluate(list)
	if len(rows) != 1 || rows[0].addr() != "m.go#build" {
		t.Errorf("::in.return.type > * is the source func build; got %v", nodeAddrs(rows))
	}
}

func nodeAddrs(ns []*treeNode) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.addr()
	}
	return out
}

// An unknown ref class is still rejected — positions are a closed set
// like kinds.
func TestUnknownRefClassRejected(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	if msg := queryErr(t, s, map[string]any{"selector": `func::in.bogus`}); msg == "" {
		t.Error("::in.bogus should be a guided error")
	}
}
