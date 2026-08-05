package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The batch controls are UNDECLARED in the schema to save tokens, and are
// revealed in prose at the moment they are needed. A dun session on 2026-08-05
// shows the cost of that: told to retry with commit:false, the agent sent
//
//	{"node":…, "oldText":…, "newText":…, "return":"commit:false"}
//
// It knew the mechanism and not the parameter, so it guessed a field that
// exists. The reply — "takes exactly one of {…}; got 2: oldText+newText,
// return" — is true and useless to someone who meant one op and one PARAMETER.
//
// This pins that the reply names where a batch control actually goes.
func TestMisfiledBatchControlIsNamed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package p\n\nfunc F() int {\n\treturn 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	call := func(args map[string]any) (string, bool) {
		r := s.callTool("node_edit", args)
		if len(r.Content) == 0 {
			return "", r.IsError
		}
		return r.Content[0].Text, r.IsError
	}

	t.Run("the session's exact shape", func(t *testing.T) {
		msg, isErr := call(map[string]any{
			"node": "a.go", "oldText": "return 1", "newText": "return 2",
			"return": "commit:false",
		})
		if !isErr {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(msg, "commit:false") {
			t.Errorf("the reply must say where a batch control goes; got %q", msg)
		}
		if !strings.Contains(msg, "parameter") && !strings.Contains(msg, "PARAMETER") {
			t.Errorf("the reply must distinguish a parameter from an op; got %q", msg)
		}
	})

	t.Run("two genuine ops still read as two ops", func(t *testing.T) {
		msg, isErr := call(map[string]any{
			"node": "a.go", "oldText": "return 1", "newText": "return 2",
			"rename": "G",
		})
		if !isErr {
			t.Fatal("expected a refusal")
		}
		if strings.Contains(msg, "batch control") {
			t.Errorf("a real two-op request must not be explained as a misfiled directive; got %q", msg)
		}
		// It should still point at the way to do several edits together.
		if !strings.Contains(msg, "commit:false") {
			t.Errorf("the reply should name how to apply several edits together; got %q", msg)
		}
	})

	// And the mechanism it names must actually work when spelled correctly.
	t.Run("commit:false as a real parameter stages the edit", func(t *testing.T) {
		msg, isErr := call(map[string]any{
			"node": "a.go", "oldText": "return 1", "newText": "return 2",
			"commit": false,
		})
		if isErr {
			t.Fatalf("commit:false should stage, not fail: %s", msg)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(msg), &m)
		if staged, _ := m["staged"].(bool); !staged {
			t.Errorf("expected a staged response, got %s", msg)
		}
		if _, isErr := call(map[string]any{"rollback": true}); isErr {
			t.Error("rollback should discard the staged batch")
		}
	})
}
