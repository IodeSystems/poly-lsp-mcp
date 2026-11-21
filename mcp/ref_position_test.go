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

	// Bare-edge landing: the answer is the FAR NODES (the symbols that
	// use Server as a type), not the edge rows. The 3 type uses (param,
	// return, field) point to 2 distinct far nodes: build and Config.
	if n := count(`#'Server'::in.type`); n != 2 {
		t.Errorf("::in.type should land on 2 far nodes (build, Config); got %d", n)
	}
	// Position splits them: each position's far node is the one symbol
	// that uses Server in that slot.
	if n := count(`#'Server'::in.return.type`); n != 1 {
		t.Errorf("::in.return.type should land on exactly 1 far node (build); got %d", n)
	}
	if n := count(`#'Server'::in.param.type`); n != 1 {
		t.Errorf("::in.param.type should land on exactly 1 far node (build); got %d", n)
	}
	if n := count(`#'Server'::in.field.type`); n != 1 {
		t.Errorf("::in.field.type should land on exactly 1 far node (Config); got %d", n)
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
