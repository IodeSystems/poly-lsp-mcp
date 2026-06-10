package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// TestApplyDerivedSQL: a sqlc derived:"table.column" tag resolves through the
// migration-fold to the column's defining site, registered as a DECLARED sql binding.
func TestApplyDerivedSQL(t *testing.T) {
	root := t.TempDir()
	mig := filepath.Join(root, "migrate", "files")
	if err := os.MkdirAll(mig, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(p, c string) {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join(mig, "000001_base.up.sql"),
		"CREATE TABLE app.account (\n  account_id bigint NOT NULL,\n  display_name text\n);\n")
	mk(filepath.Join(root, "models.go"),
		"package db\ntype Account struct {\n\tDisplayName string `db:\"display_name\" derived:\"account.display_name\" json:\"display_name\"`\n}\n")

	idx := symbols.NewIndex()
	if roots := NewResolver(root).ApplyDerivedSQL(idx); len(roots) != 1 {
		t.Fatalf("ApplyDerivedSQL added %d roots, want 1", len(roots))
	}
	var ok bool
	for _, s := range idx.Lookup("display_name") {
		if s.Language == "sql" && s.Confidence == symbols.ConfidenceDeclared {
			ok = true
			if !filepath_HasSuffix(s.File, "000001_base.up.sql") {
				t.Errorf("declared site file=%s, want the migration", s.File)
			}
		}
	}
	if !ok {
		t.Errorf("no declared sql site for display_name; sites=%+v", idx.Lookup("display_name"))
	}
}

func filepath_HasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
