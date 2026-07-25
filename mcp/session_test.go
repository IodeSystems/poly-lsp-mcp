package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callSession drives node_edit through the daemon seam (CallTool with an
// explicit session id), the path a daemon client takes. Returns the decoded
// JSON payload, whether the tool flagged an error, and any handler error
// (a claim conflict surfaces as a handler error).
func callSession(t *testing.T, s *Server, sess string, args map[string]any) (map[string]any, bool, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	content, isErr, err := s.CallTool(sess, "node_edit", raw)
	if err != nil {
		return nil, isErr, err
	}
	var m map[string]any
	if len(content) > 0 {
		_ = json.Unmarshal([]byte(content[0].Text), &m)
	}
	return m, isErr, nil
}

// Two sessions on one root keep SEPARATE batches: a plain commit by session
// B must not commit or revert session A's staged edit. This is the daemon
// corruption fix — before session-keying, one editBatch field was shared and
// B's edit closed A's batch.
func TestSessionBatchesAreIsolated(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	srv := s.srv

	mainPath := filepath.Join(dir, "main.go")
	notesPath := filepath.Join(dir, "notes.md")
	mainOrig, _ := os.ReadFile(mainPath)
	notesOrig, _ := os.ReadFile(notesPath)

	// A stages an edit on main.go.
	m, _, err := callSession(t, srv, "A", map[string]any{
		"node": "main.go#Free", "oldText": "func Free(only int) {}", "newText": "func Free(only, extra int) {}",
		"commit": false,
	})
	if err != nil || m["staged"] != true {
		t.Fatalf("A stage: want staged; got %+v err=%v", m, err)
	}

	// B stages an edit on a DIFFERENT file — its own batch, pending 1 (not 2).
	m, _, err = callSession(t, srv, "B", map[string]any{
		"node": "notes.md", "oldText": "# hello", "newText": "# hi",
		"commit": false,
	})
	if err != nil || m["staged"] != true || m["pending"].(float64) != 1 {
		t.Fatalf("B stage: want staged pending=1 (B's own batch); got %+v err=%v", m, err)
	}

	// B commits (noop commit). It must commit ONLY B's edit and leave A's open.
	m, _, err = callSession(t, srv, "B", map[string]any{"commit": true})
	if err != nil || m["committed"] != true || m["edits"].(float64) != 1 {
		t.Fatalf("B commit: want committed edits=1; got %+v err=%v", m, err)
	}
	if b, _ := os.ReadFile(notesPath); string(b) == string(notesOrig) {
		t.Fatal("B's commit did not persist its edit")
	}
	if b, _ := os.ReadFile(mainPath); string(b) == string(mainOrig) {
		t.Fatal("A's staged edit vanished — B's commit must not touch A's batch")
	}

	// A's batch is still open: rolling A back discards exactly A's 1 edit and
	// restores main.go; notes.md (B's) stays committed.
	m, _, err = callSession(t, srv, "A", map[string]any{"rollback": true})
	if err != nil || m["rolledBack"] != true || m["discarded"].(float64) != 1 {
		t.Fatalf("A rollback: want rolledBack discarded=1; got %+v err=%v", m, err)
	}
	if b, _ := os.ReadFile(mainPath); string(b) != string(mainOrig) {
		t.Fatal("A rollback did not restore main.go")
	}
	if b, _ := os.ReadFile(notesPath); string(b) == string(notesOrig) {
		t.Fatal("A rollback must not touch B's committed file")
	}
}

// A file staged by one session is CLAIMED: a second session staging the same
// file is rejected (naming the holder) instead of silently clobbering the
// first session's staged bytes on a later revert.
func TestSessionClaimBlocksCrossSessionStage(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	srv := s.srv

	if m, _, err := callSession(t, srv, "A", map[string]any{
		"node": "main.go#Free", "oldText": "func Free(only int) {}", "newText": "func Free(only, extra int) {}",
		"commit": false,
	}); err != nil || m["staged"] != true {
		t.Fatalf("A stage: want staged; got %+v err=%v", m, err)
	}

	// B stages a different symbol in the SAME file → claim conflict.
	_, isErr, err := callSession(t, srv, "B", map[string]any{
		"node": "main.go#CallsStart", "oldText": "s := &Server{}", "newText": "s := &Server{Name: \"x\"}",
		"commit": false,
	})
	if err == nil {
		t.Fatal("B staging a file A holds must fail")
	}
	if !isErr || !strings.Contains(err.Error(), "another client session") {
		t.Fatalf("claim conflict must name the holder; got isErr=%v err=%v", isErr, err)
	}

	// The holder can still commit its own batch.
	if m, _, err := callSession(t, srv, "A", map[string]any{"commit": true}); err != nil || m["committed"] != true {
		t.Fatalf("A commit after claim: want committed; got %+v err=%v", m, err)
	}
	// Claim released: B can now stage the same file.
	if m, _, err := callSession(t, srv, "B", map[string]any{
		"node": "main.go#CallsStart", "oldText": "s := &Server{}", "newText": "s := &Server{Name: \"x\"}",
		"commit": false,
	}); err != nil || m["staged"] != true {
		t.Fatalf("B stage after A committed: want staged; got %+v err=%v", m, err)
	}
}

// A staged file written underneath the batch (another session, the editor, a
// formatter, git) is caught at commit: the commit ABORTS with a conflict, the
// batch stays open, and nothing is silently overwritten or reverted.
func TestSessionCommitDetectsExternalWrite(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	srv := s.srv

	mainPath := filepath.Join(dir, "main.go")

	if m, _, err := callSession(t, srv, "A", map[string]any{
		"node": "main.go#Free", "oldText": "func Free(only int) {}", "newText": "func Free(only, extra int) {}",
		"commit": false,
	}); err != nil || m["staged"] != true {
		t.Fatalf("A stage: want staged; got %+v err=%v", m, err)
	}

	// Something outside the batch rewrites the staged file.
	if err := os.WriteFile(mainPath, []byte("package x\n\n// clobbered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, isErr, err := callSession(t, srv, "A", map[string]any{"commit": true})
	if err != nil {
		t.Fatalf("commit call: %v", err)
	}
	if !isErr || m["conflict"] != true {
		t.Fatalf("external write must abort the commit with a conflict; got %+v", m)
	}
	changed, _ := m["changedFiles"].([]any)
	if len(changed) != 1 || changed[0].(string) != "main.go" {
		t.Fatalf("conflict must name the changed file; got %+v", m["changedFiles"])
	}
	// The batch is still OPEN (nothing reverted): A can roll it back.
	if m, _, err := callSession(t, srv, "A", map[string]any{"rollback": true}); err != nil || m["rolledBack"] != true {
		t.Fatalf("batch must stay open after a conflict; got %+v err=%v", m, err)
	}
}
