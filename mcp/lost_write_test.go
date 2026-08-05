package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regression this exists for, reduced to its bones.
//
// On the volcycle bench, FOUR independent agent runs — two prompt framings,
// with and without the reference graph — deleted `mVirtualControlKeyDown =
// down`, leaving the field declared and still read by readControlKey(). Volume
// Down stopped being Ctrl on a phone with no hardware keyboard. javac was
// happy (the field IS still read, it is simply never true again), the bench's
// textual tier scored it 5/6 KEEP, and it shipped.
//
// Nothing in the tool surface objected, because nothing looks at whether an
// edit removed the last thing that ever SET a name.
func TestEditWarnsWhenTheLastWriteIsRemoved(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\ngo 1.26\n")
	write("a.go", "package p\n\ntype C struct{ flag bool }\n\n"+
		"func (c *C) set(d bool) {\n\tc.flag = d\n}\n\n"+
		"func (c *C) read() bool {\n\treturn c.flag\n}\n")

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	edit := func(args map[string]any) map[string]any {
		t.Helper()
		r := s.callTool("node_edit", args)
		var m map[string]any
		if len(r.Content) > 0 {
			_ = json.Unmarshal([]byte(r.Content[0].Text), &m)
		}
		if r.IsError {
			t.Fatalf("edit failed: %s", r.Content[0].Text)
		}
		return m
	}

	t.Run("removing the only assignment is flagged", func(t *testing.T) {
		m := edit(map[string]any{
			"node": "a.go#C.set", "oldText": "c.flag = d", "newText": "_ = d",
		})
		lost, _ := m["lostWrites"].([]any)
		if len(lost) != 1 || lost[0] != "flag" {
			t.Fatalf("expected lostWrites=[flag], got %v (payload %v)", lost, m)
		}
		alert, _ := m["alert"].(string)
		if !strings.Contains(alert, "still referenced") {
			t.Errorf("the alert must say why it matters; got %q", alert)
		}
		// It REPORTS. Removing a write is legitimate, and a refusal would be
		// wrong far more often than a note is annoying.
		if rejected, _ := m["rejected"].(bool); rejected {
			t.Error("the guard must not reject — it reports")
		}
	})

	// Restore, then the negatives.
	edit(map[string]any{"node": "a.go#C.set", "oldText": "_ = d", "newText": "c.flag = d"})

	t.Run("deleting the reads too is just a deletion", func(t *testing.T) {
		// Drop the reader entirely: nothing references flag afterwards, so
		// there is nothing to warn about.
		m := edit(map[string]any{
			"node":    "a.go",
			"oldText": "func (c *C) read() bool {\n\treturn c.flag\n}\n",
			"newText": "",
		})
		_ = m
		m2 := edit(map[string]any{
			"node": "a.go#C.set", "oldText": "c.flag = d", "newText": "_ = d",
		})
		if lost, ok := m2["lostWrites"]; ok {
			t.Errorf("with no readers left there is nothing to warn about; got %v", lost)
		}
	})

	t.Run("an edit that keeps a write says nothing", func(t *testing.T) {
		write("b.go", "package p\n\ntype D struct{ n int }\n\n"+
			"func (d *D) bump() {\n\td.n = d.n + 1\n}\n\nfunc (d *D) get() int { return d.n }\n")
		m := edit(map[string]any{
			"node": "b.go#D.bump", "oldText": "d.n = d.n + 1", "newText": "d.n = d.n + 2",
		})
		if lost, ok := m["lostWrites"]; ok {
			t.Errorf("the assignment is still there; got %v", lost)
		}
	})
}
