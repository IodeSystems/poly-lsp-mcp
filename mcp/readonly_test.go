package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Read-only is ENFORCEMENT, not a hint: a tool the model cannot see is one it
// cannot call, which is stronger than any instruction not to.
func TestReadOnlyHidesMutatingTools(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, "package main\n\nfunc Keep() {}\n"), nil, nil)
	defer s.close()
	s.srv.SetReadOnly(true)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	names := map[string]bool{}
	for n := range s.srv.tools {
		names[n] = true
	}
	for _, gone := range []string{"node_edit", "node_delete", "node_refactor", "node_rename_file"} {
		if names[gone] {
			t.Errorf("read-only surface still exposes %q", gone)
		}
	}
	for _, kept := range []string{"node_query", "node_read"} {
		if !names[kept] {
			t.Errorf("read-only surface must keep %q", kept)
		}
	}
}

// Calling a hidden tool is a plain unknown-tool error — there is no secret
// handler still wired up behind it.
func TestReadOnlyRejectsEditCall(t *testing.T) {
	dir := goWorkspace(t, "package main\n\nfunc Keep() {}\n")
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.srv.SetReadOnly(true)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Rejected at the JSON-RPC layer, not as a tool result — the tool simply
	// does not exist. That is stronger than an in-band error: there is no
	// handler left to reach.
	resp := s.request("tools/call", map[string]any{
		"name":      "node_edit",
		"arguments": map[string]any{"node": "new.go", "newText": "package main\n"},
	})
	if resp.Error == nil {
		t.Fatal("node_edit must not be callable in read-only mode")
	}
	if !strings.Contains(strings.ToLower(resp.Error.Message), "unknown tool") {
		t.Errorf("want an unknown-tool error, got: %+v", resp.Error)
	}
	// And nothing was written.
	if _, err := os.Stat(filepath.Join(dir, "new.go")); err == nil {
		t.Error("read-only mode created a file")
	}
}

// Composes with the legacy surface in EITHER call order — each setter
// re-registers the base surface, so a naive implementation silently
// resurrects the mutating tools when the other setter runs second.
func TestReadOnlyComposesWithLegacyEitherOrder(t *testing.T) {
	check := func(t *testing.T, s *mcpSession) {
		t.Helper()
		for _, gone := range []string{"node_edit", "node_delete", "node_refactor"} {
			if _, ok := s.srv.tools[gone]; ok {
				t.Errorf("legacy+read-only still exposes %q", gone)
			}
		}
		if _, ok := s.srv.tools["structure"]; !ok {
			t.Error("legacy read-only should still expose structure")
		}
	}
	t.Run("readonly-then-legacy", func(t *testing.T) {
		s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
		defer s.close()
		s.srv.SetReadOnly(true)
		s.srv.SetLegacyTools(true)
		check(t, s)
	})
	t.Run("legacy-then-readonly", func(t *testing.T) {
		s := startSessionFull(t, goWorkspace(t, "package main\n"), nil, nil)
		defer s.close()
		s.srv.SetLegacyTools(true)
		s.srv.SetReadOnly(true)
		check(t, s)
	})
}
