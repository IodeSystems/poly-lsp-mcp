package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// In java/kotlin `import a.b.C;` DECLARES an `import` node, so the line is a
// declaration and fileSites excludes it from reference sites — correctly.
// The consequence was that the class it names had no incoming import edge at
// all: measured on termux-app 2026-08-04,
// `class[name=ExtraKeysView]::in.import` answered "no matches" while three
// files imported it, and the zero-result note then pointed at a javadoc
// {@link}, which reads as "nothing imports this".
//
// The data was always indexed (`import[name=ExtraKeysView]` listed all three).
// importRefs wires that relation to the spelling a caller actually writes,
// and only when `.import` is NAMED — a bare ::in must not start scanning
// import tables for every host it walks.
func TestImportRefsFindJavaImporters(t *testing.T) {
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
	write("lib/Widget.java", "package lib;\n\npublic class Widget {\n  public int size() { return 1; }\n}\n")
	write("app/Screen.java", "package app;\n\nimport lib.Widget;\n\nclass Screen {\n  Widget w;\n}\n")
	write("app/Panel.java", "package app;\n\nimport lib.Widget;\n\nclass Panel {\n  Widget w;\n}\n")
	// A javadoc mention is NOT an import — it was the thing masquerading as
	// one in the note that started this.
	write("app/Note.java", "package app;\n\n/** see {@link Widget} */\nclass Note {}\n")

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	q := query(t, s, map[string]any{"selector": "class[name=Widget]::in.import", "limit": 50})
	got := map[string]bool{}
	for _, m := range q.Matches {
		got[m.Node] = true
	}
	if len(got) != 2 {
		t.Fatalf("Widget is imported by exactly Screen.java and Panel.java; got %d: %+v", len(got), q.Matches)
	}
	for _, want := range []string{"app/Screen.java@3", "app/Panel.java@3"} {
		if !got[want] {
			t.Errorf("missing importer %s; got %+v", want, q.Matches)
		}
	}

	// Built only when NAMED: a bare ::in is the site-based edge set, and
	// must not silently grow import declarations.
	bare := query(t, s, map[string]any{"selector": "class[name=Widget]::in", "limit": 50})
	for _, m := range bare.Matches {
		if m.Node == "app/Screen.java@3" || m.Node == "app/Panel.java@3" {
			t.Errorf("a bare ::in must not include import declarations; got %+v", bare.Matches)
		}
	}

	// `::out.import` on a class is not a relation — what a class imports is
	// a property of its FILE, not of the symbol.
	out := query(t, s, map[string]any{"selector": "class[name=Widget]::out.import", "limit": 50})
	if out.TotalMatches != 0 {
		t.Errorf("::out.import on a class should be empty; got %+v", out.Matches)
	}
}
