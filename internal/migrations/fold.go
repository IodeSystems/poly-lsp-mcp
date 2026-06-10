// Package migrations folds an ordered set of *.up.sql migration files into the
// cumulative schema they build — tables, columns (with each column's current
// defining site), and views. This is the authoritative ROOT for SQL derivation:
// a sqlc-generated Go struct field is @derived from a column whose source of truth
// is the CREATE/ALTER that last defined it here, not any single migration file.
//
// DDL is parsed with tree-sitter-sql; Postgres-specific constructs the grammar
// can't model degrade gracefully (the fold keeps what it can resolve).
package migrations

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/sql"
)

// Site is a 1-based file position (byte column).
type Site struct {
	File string
	Line int
	Col  int
}

// Column is one column in the folded schema. DefinedAt is where it currently lives
// (the CREATE TABLE line, or the latest ALTER … ADD/RENAME that touched it).
type Column struct {
	Name      string
	Type      string
	DefinedAt Site
}

// Table is a folded table; Columns is in declaration order.
type Table struct {
	Name      string
	Columns   []*Column
	DefinedAt Site
}

// View is a folded view. Projects best-effort lists the bare column identifiers it
// selects (expression/aliased projections are not fully modeled yet).
type View struct {
	Name      string
	DefinedAt Site
	Projects  []string
}

// Schema is the cumulative result of folding the migrations in order.
type Schema struct {
	Tables map[string]*Table
	Views  map[string]*View
	Files  []string // migration files applied, in order
}

// Fold finds the ordered *.up.sql migrations under root and applies their DDL in
// sequence into one Schema.
func Fold(root string) *Schema {
	sc := &Schema{Tables: map[string]*Table{}, Views: map[string]*View{}}
	for _, f := range findMigrations(root) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		applyFile(sc, f, data)
		sc.Files = append(sc.Files, f)
	}
	return sc
}

func findMigrations(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || (strings.HasPrefix(n, ".") && n != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".up.sql") {
			out = append(out, path)
		}
		return nil
	})
	// golang-migrate's zero-padded version prefix makes lexical == migration order.
	sort.Strings(out)
	return out
}

func applyFile(sc *Schema, file string, data []byte) {
	p := sitter.NewParser()
	p.SetLanguage(sql.GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, data)
	if err != nil || tree == nil {
		return
	}
	defer tree.Close()
	root := tree.RootNode()
	for i := 0; i < int(root.ChildCount()); i++ {
		stmt := root.Child(i)
		if stmt.Type() != "statement" {
			continue
		}
		for j := 0; j < int(stmt.ChildCount()); j++ {
			n := stmt.Child(j)
			switch n.Type() {
			case "create_table":
				applyCreateTable(sc, file, data, n)
			case "alter_table":
				applyAlterTable(sc, file, data, n)
			case "create_view":
				applyCreateView(sc, file, data, n)
			case "drop_table", "drop_view", "drop_statement":
				applyDrop(sc, data, n)
			}
		}
	}
}

func applyCreateTable(sc *Schema, file string, data []byte, n *sitter.Node) {
	name := objectRefName(child(n, "object_reference"), data)
	if name == "" {
		return
	}
	t := &Table{Name: name, DefinedAt: siteOf(file, child(n, "object_reference"))}
	if defs := child(n, "column_definitions"); defs != nil {
		for i := 0; i < int(defs.ChildCount()); i++ {
			cd := defs.Child(i)
			if cd.Type() != "column_definition" {
				continue
			}
			id := firstChildOfType(cd, "identifier")
			if id == nil {
				continue
			}
			t.Columns = append(t.Columns, &Column{
				Name:      unquote(id.Content(data)),
				Type:      columnType(cd, id, data),
				DefinedAt: siteOf(file, id),
			})
		}
	}
	sc.Tables[name] = t
}

func applyAlterTable(sc *Schema, file string, data []byte, n *sitter.Node) {
	t := sc.Tables[objectRefName(child(n, "object_reference"), data)]
	if t == nil {
		return
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		op := n.Child(i)
		switch op.Type() {
		case "add_column":
			cd := firstChildOfType(op, "column_definition")
			if cd == nil {
				continue
			}
			id := firstChildOfType(cd, "identifier")
			if id == nil {
				continue
			}
			t.Columns = append(t.Columns, &Column{
				Name: unquote(id.Content(data)), Type: columnType(cd, id, data), DefinedAt: siteOf(file, id),
			})
		case "drop_column":
			if id := firstChildOfType(op, "identifier"); id != nil {
				dropColumn(t, unquote(id.Content(data)))
			}
		}
	}
}

func applyCreateView(sc *Schema, file string, data []byte, n *sitter.Node) {
	name := objectRefName(child(n, "object_reference"), data)
	if name == "" {
		return
	}
	v := &View{Name: name, DefinedAt: siteOf(file, child(n, "object_reference"))}
	if q := child(n, "create_query"); q != nil {
		if sel := firstChildOfType(q, "select"); sel != nil {
			collectIdentifiers(sel, data, &v.Projects)
		}
	}
	sc.Views[name] = v
}

func applyDrop(sc *Schema, data []byte, n *sitter.Node) {
	name := objectRefName(child(n, "object_reference"), data)
	delete(sc.Tables, name)
	delete(sc.Views, name)
}

// ---- helpers ----

func child(n *sitter.Node, typ string) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		if c := n.Child(i); c.Type() == typ {
			return c
		}
	}
	return nil
}

func firstChildOfType(n *sitter.Node, typ string) *sitter.Node { return child(n, typ) }

// objectRefName returns the bare (unquoted, schema-stripped) name of an
// object_reference like redline."USER" → USER.
func objectRefName(ref *sitter.Node, data []byte) string {
	if ref == nil {
		return ""
	}
	var last string
	for i := 0; i < int(ref.ChildCount()); i++ {
		if c := ref.Child(i); c.Type() == "identifier" {
			last = unquote(c.Content(data))
		}
	}
	return last
}

// columnType returns the text after the column name up to the first comma/constraint
// keyword — good enough to label the type without modeling every Postgres type.
func columnType(cd, nameID *sitter.Node, data []byte) string {
	if next := nameID.NextSibling(); next != nil {
		return strings.TrimSpace(next.Content(data))
	}
	_ = cd
	return ""
}

func collectIdentifiers(n *sitter.Node, data []byte, out *[]string) {
	if n.Type() == "identifier" {
		*out = append(*out, unquote(n.Content(data)))
		return
	}
	for i := 0; i < int(n.ChildCount()); i++ {
		collectIdentifiers(n.Child(i), data, out)
	}
}

func dropColumn(t *Table, name string) {
	for i, c := range t.Columns {
		if c.Name == name {
			t.Columns = append(t.Columns[:i], t.Columns[i+1:]...)
			return
		}
	}
}

func unquote(s string) string { return strings.Trim(s, "\"`") }

func siteOf(file string, n *sitter.Node) Site {
	if n == nil {
		return Site{File: file}
	}
	pt := n.StartPoint()
	return Site{File: file, Line: int(pt.Row) + 1, Col: int(pt.Column) + 1}
}
