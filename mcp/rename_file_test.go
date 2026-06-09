package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportEditsForRename(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	from := mk("src/events.ts", "export const x = 1\n")
	mk("src/screens/list.tsx", "import { x } from '../events'\nconst s = 'unrelated'\n")
	mk("src/client.ts", "import { x } from './events'\nawait import('./events')\n")
	mk("src/withext.ts", "import { x } from './events.ts'\n")
	mk("src/nomatch.ts", "import { y } from './other'\n")
	to := filepath.Join(root, "src/fundraisers.ts")

	edits := importEditsForRename(root, from, to)

	want := map[string]string{
		filepath.Join(root, "src/screens/list.tsx"): "../fundraisers",
		filepath.Join(root, "src/client.ts"):        "./fundraisers",   // first occurrence
		filepath.Join(root, "src/withext.ts"):       "./fundraisers.ts", // extension preserved
	}
	for f, newSpec := range want {
		es := edits[f]
		if len(es) == 0 {
			t.Errorf("%s: no edit produced", filepath.Base(f))
			continue
		}
		if es[0].NewText != newSpec {
			t.Errorf("%s: NewText=%q want %q", filepath.Base(f), es[0].NewText, newSpec)
		}
	}
	// client.ts has TWO specifiers (static + dynamic import) → 2 edits.
	if got := len(edits[filepath.Join(root, "src/client.ts")]); got != 2 {
		t.Errorf("client.ts edits=%d, want 2 (static + dynamic import)", got)
	}
	// nomatch.ts imports a different module — no edit.
	if _, ok := edits[filepath.Join(root, "src/nomatch.ts")]; ok {
		t.Errorf("nomatch.ts should not be edited")
	}
}
