package main

import "testing"

// cannedSQLRows is the same graph as cannedCypherRows: the three languages
// must be comparable on identical rows.
func cannedSQLRows() *Rows {
	return &Rows{
		Nodes: []Node{
			{ID: "store.go#Save", File: "store.go", Name: "Save", Kind: "func"},
			{ID: "main.go#Caller", File: "main.go", Name: "Caller", Kind: "func"},
			{ID: "web.go#Handler", File: "web.go", Name: "Handler", Kind: "func"},
			{ID: "store.go#flush", File: "store.go", Name: "flush", Kind: "func"},
		},
		Edges: []Edge{
			{From: "main.go#Caller", To: "store.go#Save", Kind: "call"},
			{From: "web.go#Handler", To: "store.go#Save", Kind: "call"},
			{From: "store.go#Save", To: "store.go#flush", Kind: "call"},
			{From: "web.go#Handler", To: "main.go#Caller", Kind: "call"},
		},
	}
}

// (d) SQL executor: canned rows + canned query (incl. one recursive CTE) →
// known set.

func TestSQLDirectCallers(t *testing.T) {
	rows := cannedSQLRows()
	q := "SELECT DISTINCT src FROM edge WHERE dst = 'store.go#Save' AND kind = 'call'"
	got, err := execSQL(rows, q)
	if err != nil {
		t.Fatalf("execSQL: %v", err)
	}
	want := []string{"main.go#Caller", "web.go#Handler"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestSQLTransitiveReachesRecursiveCTE(t *testing.T) {
	rows := cannedSQLRows()
	q := `WITH RECURSIVE reach AS (
	  SELECT src FROM edge WHERE dst = 'store.go#Save'
	  UNION
	  SELECT e.src FROM edge e JOIN reach r ON e.dst = r.src
	) SELECT DISTINCT src FROM reach`
	got, err := execSQL(rows, q)
	if err != nil {
		t.Fatalf("execSQL: %v", err)
	}
	want := []string{"main.go#Caller", "web.go#Handler"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestSQLCallersInFile(t *testing.T) {
	rows := cannedSQLRows()
	q := `SELECT DISTINCT e.src FROM edge e
	  WHERE e.dst = 'store.go#Save' AND e.kind = 'call'`
	got, err := execSQL(rows, q)
	if err != nil {
		t.Fatalf("execSQL: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want 2 callers", got)
	}
}

func TestSQLNodeLookup(t *testing.T) {
	rows := cannedSQLRows()
	q := `SELECT DISTINCT id FROM node WHERE name = 'Save'`
	got, err := execSQL(rows, q)
	if err != nil {
		t.Fatalf("execSQL: %v", err)
	}
	if len(got) != 1 || got[0] != "store.go#Save" {
		t.Errorf("got %v, want [store.go#Save]", got)
	}
}

func TestSQLLike(t *testing.T) {
	rows := cannedSQLRows()
	q := `SELECT DISTINCT id FROM node WHERE file LIKE 'store.go%'`
	got, err := execSQL(rows, q)
	if err != nil {
		t.Fatalf("execSQL: %v", err)
	}
	want := []string{"store.go#Save", "store.go#flush"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestSQLParseError(t *testing.T) {
	rows := cannedSQLRows()
	if _, err := execSQL(rows, "DELETE FROM edge"); err == nil {
		t.Error("expected an error for a non-SELECT statement")
	}
}
