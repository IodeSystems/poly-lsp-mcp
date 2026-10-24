package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renameCommentsWS builds a workspace with one declaration of `sym`, a
// comment mention in Go and a prose mention in markdown.
func renameCommentsWS(t *testing.T, sym string) (*mcpSession, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	write("main.go", "package main\n\n// "+sym+" is documented here\nfunc "+sym+"() {}\n")
	write("README.md", "Call "+sym+" first.\n")
	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s, dir
}

func renameReply(t *testing.T, r toolResp) map[string]any {
	t.Helper()
	if r.IsError {
		t.Fatalf("rename errored: %+v", r.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRenameRewritesCommentsByDefaultForCompoundName(t *testing.T) {
	s, dir := renameCommentsWS(t, "fetchUser")
	defer s.close()
	m := renameReply(t, s.callTool("node_edit", map[string]any{"node": "main.go#fetchUser", "rename": "loadUser"}))

	main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if !strings.Contains(string(main), "// loadUser is documented") || !strings.Contains(string(readme), "Call loadUser first") {
		t.Errorf("comment/prose not rewritten by default:\n%s\n%s", main, readme)
	}
	files, _ := m["commentsChanged"].([]any)
	if len(files) != 2 {
		t.Errorf("commentsChanged should list main.go and README.md; got %v", m["commentsChanged"])
	}
	if note, _ := m["commentsNote"].(string); !strings.Contains(note, "includeComments:false") {
		t.Errorf("reply should say how to get a code-only rename; got %q", note)
	}
}

func TestRenamePlainWordLeavesProseAndReportsSkipped(t *testing.T) {
	s, dir := renameCommentsWS(t, "save")
	defer s.close()
	m := renameReply(t, s.callTool("node_edit", map[string]any{"node": "main.go#save", "rename": "persist"}))

	main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if !strings.Contains(string(main), "func persist()") || !strings.Contains(string(main), "// save is documented") {
		t.Errorf("code renamed, comment untouched expected:\n%s", main)
	}
	if string(readme) != "Call save first.\n" {
		t.Errorf("prose must stay for a plain word:\n%s", readme)
	}
	if m["commentMentionsSkipped"] != float64(2) {
		t.Errorf("commentMentionsSkipped = %v, want 2", m["commentMentionsSkipped"])
	}
	if note, _ := m["commentsNote"].(string); !strings.Contains(note, "includeComments:true") {
		t.Errorf("reply should say how to force it; got %q", note)
	}
}

func TestRenamePlainWordExplicitTrueRewritesProse(t *testing.T) {
	s, dir := renameCommentsWS(t, "save")
	defer s.close()
	renameReply(t, s.callTool("node_edit", map[string]any{"node": "main.go#save", "rename": "persist", "includeComments": true}))
	readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(readme) != "Call persist first.\n" {
		t.Errorf("explicit true must override the plain-word guard:\n%s", readme)
	}
}

func TestRenameIncludeCommentsFalseLeavesCommentsForCompoundName(t *testing.T) {
	s, dir := renameCommentsWS(t, "fetchUser")
	defer s.close()
	m := renameReply(t, s.callTool("node_edit", map[string]any{"node": "main.go#fetchUser", "rename": "loadUser", "includeComments": false}))
	main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if !strings.Contains(string(main), "func loadUser()") || !strings.Contains(string(main), "// fetchUser is documented") {
		t.Errorf("includeComments:false must rename code only:\n%s", main)
	}
	if _, ok := m["commentsChanged"]; ok {
		t.Errorf("no comment report expected with includeComments:false; got %v", m["commentsChanged"])
	}
}

func TestPlainWordName(t *testing.T) {
	for name, want := range map[string]bool{
		"save": true, "data": true, "get": true,
		"fetchUser": false, "user_id": false, "v2": false, "UserID": false, "": false,
	} {
		if got := plainWordName(name); got != want {
			t.Errorf("plainWordName(%q) = %v, want %v", name, got, want)
		}
	}
}
