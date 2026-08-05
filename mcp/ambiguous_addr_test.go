package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A go main.go has TWO symbols answering to "main": the package clause (a
// module) and func main. `file#main` therefore resolves to neither, and the
// disambiguation printed the same address twice:
//
//	"main" is ambiguous in cmd/dun/main.go: 2 symbols share that name.
//	Pass one of these exact addresses:
//	  cmd/dun/main.go#main
//	  cmd/dun/main.go#main
//
// Observed in a dun session 2026-08-05. The list is the whole value of the
// error, so offering a choice that cannot be made is the same as offering
// nothing. The ordinal in classicSymMatch cannot break the tie either — both
// candidates are the bare sym and normalize to [1].
//
// What this pins is that every offered address RESOLVES, and that they are
// distinct from each other. The exact spelling is free to change.
func TestAmbiguousAddressOffersUsableAlternatives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_read", map[string]any{"node": "main.go#main"})
	if !r.IsError {
		t.Fatalf("`main.go#main` must be ambiguous here (module + func); got %+v", r.Content)
	}
	msg := r.Content[0].Text
	if !strings.Contains(msg, "ambiguous") {
		t.Fatalf("expected an ambiguity error, got %q", msg)
	}

	// Harvest the offered addresses and require each to resolve to exactly one
	// node — and to a DIFFERENT one.
	var offered []string
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "main") || strings.Contains(line, "ambiguous") {
			continue
		}
		offered = append(offered, strings.Fields(line)[0])
	}
	if len(offered) < 2 {
		t.Fatalf("expected at least 2 candidates, parsed %v from:\n%s", offered, msg)
	}
	if offered[0] == offered[1] {
		t.Fatalf("the candidates are identical, so no choice can be made: %q\n%s", offered[0], msg)
	}

	spans := map[string]bool{}
	for _, addr := range offered {
		rr := s.callTool("node_read", map[string]any{"node": addr})
		var m map[string]any
		if len(rr.Content) > 0 {
			_ = json.Unmarshal([]byte(rr.Content[0].Text), &m)
		}
		if rr.IsError {
			t.Errorf("offered address %q does not resolve: %s", addr, rr.Content[0].Text)
			continue
		}
		spans[strings.TrimSpace(m["text"].(string))] = true
	}
	if len(spans) < 2 {
		t.Errorf("the offered addresses resolved to the same node; they must pick different ones")
	}
}
