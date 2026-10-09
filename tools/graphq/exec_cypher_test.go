package main

import "testing"

// cannedCypherRows is a tiny graph:
//
//	Store.Save  <-  Main.Caller, Web.Handler     (call)
//	Store.Save  ->  Store.flush                   (call)
//	Web.Handler ->  Main.Caller                   (call)
//	Store       ->  "io" (import)
//
// so transitive "everything that reaches Save" = {Main.Caller, Web.Handler}.
func cannedCypherRows() *Rows {
	return &Rows{
		Nodes: []Node{
			{ID: "store.go#Save", Name: "Save", Kind: "func"},
			{ID: "main.go#Caller", Name: "Caller", Kind: "func"},
			{ID: "web.go#Handler", Name: "Handler", Kind: "func"},
			{ID: "store.go#flush", Name: "flush", Kind: "func"},
		},
		Edges: []Edge{
			{From: "main.go#Caller", To: "store.go#Save", Kind: "call"},
			{From: "web.go#Handler", To: "store.go#Save", Kind: "call"},
			{From: "store.go#Save", To: "store.go#flush", Kind: "call"},
			{From: "web.go#Handler", To: "main.go#Caller", Kind: "call"},
		},
	}
}

// (c) Cypher executor: canned node/edge rows + canned query → known set.

func TestCypherDirectCallers(t *testing.T) {
	rows := cannedCypherRows()
	q := "MATCH (a)-[:CALLS]->(b:#'store.go#Save') RETURN DISTINCT a"
	got, err := execCypher(rows, q)
	if err != nil {
		t.Fatalf("execCypher: %v", err)
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

func TestCypherTransitiveReaches(t *testing.T) {
	rows := cannedCypherRows()
	q := "MATCH (a)-[:CALLS*1..3]->(b:#'store.go#Save') RETURN DISTINCT a"
	got, err := execCypher(rows, q)
	if err != nil {
		t.Fatalf("execCypher: %v", err)
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

func TestCypherOutgoing(t *testing.T) {
	rows := cannedCypherRows()
	q := "MATCH (a:#'store.go#Save')-[:CALLS]->(b) RETURN DISTINCT b"
	got, err := execCypher(rows, q)
	if err != nil {
		t.Fatalf("execCypher: %v", err)
	}
	if len(got) != 1 || got[0] != "store.go#flush" {
		t.Errorf("got %v, want [store.go#flush]", got)
	}
}

func TestCypherLabelFilter(t *testing.T) {
	rows := cannedCypherRows()
	// Only func-labelled callers of Save.
	q := "MATCH (a:Func)-[:CALLS]->(b:#'store.go#Save') RETURN DISTINCT a"
	got, err := execCypher(rows, q)
	if err != nil {
		t.Fatalf("execCypher: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want 2 func callers", got)
	}
}

func TestCypherParseError(t *testing.T) {
	rows := cannedCypherRows()
	if _, err := execCypher(rows, "MATCH (a)-[:CALLS]->(b)"); err == nil {
		t.Error("expected an error for a query with no RETURN clause")
	}
	if _, err := execCypher(rows, "RETURN a"); err == nil {
		t.Error("expected an error for a query with no MATCH clause")
	}
}
