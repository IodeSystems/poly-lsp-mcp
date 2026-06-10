package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFold(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "migrate", "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("000001_base.up.sql",
		"CREATE TABLE redline.\"USER\" (\n  user_id bigint NOT NULL,\n  display_name citext,\n  obsolete text\n);\n"+
			"CREATE VIEW redline.user_view AS SELECT user_id, display_name FROM redline.\"USER\";\n")
	write("000002_add.up.sql",
		"ALTER TABLE redline.\"USER\" ADD COLUMN nickname text;\n"+
			"ALTER TABLE redline.\"USER\" DROP COLUMN obsolete;\n")

	sc := Fold(root)
	if len(sc.Files) != 2 {
		t.Fatalf("folded %d files, want 2", len(sc.Files))
	}
	u := sc.Tables["USER"]
	if u == nil {
		t.Fatal("USER table not folded")
	}
	cols := map[string]*Column{}
	for _, c := range u.Columns {
		cols[c.Name] = c
	}
	for _, want := range []string{"user_id", "display_name", "nickname"} {
		if cols[want] == nil {
			t.Errorf("missing column %q; have %v", want, keys(cols))
		}
	}
	if cols["obsolete"] != nil {
		t.Error("obsolete should have been dropped by 000002")
	}
	// display_name's source of truth is the base migration; nickname's is 000002.
	if dn := cols["display_name"]; dn != nil && !strings.HasSuffix(dn.DefinedAt.File, "000001_base.up.sql") {
		t.Errorf("display_name DefinedAt=%v, want 000001", dn.DefinedAt)
	}
	if nk := cols["nickname"]; nk != nil && !strings.HasSuffix(nk.DefinedAt.File, "000002_add.up.sql") {
		t.Errorf("nickname DefinedAt=%v, want 000002", nk.DefinedAt)
	}
	if sc.Views["user_view"] == nil {
		t.Error("user_view not folded")
	}
}

func keys(m map[string]*Column) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
