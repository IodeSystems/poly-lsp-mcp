package main

import (
	"strings"
	"testing"
	"time"
)

// testRows is the tiny shared row set the model-client tests run the executors
// over: a.go#Save is called from b.go#Caller. No server, no network.
func testRows() *Rows {
	return &Rows{
		Nodes: []Node{
			{ID: "a.go#Save", File: "a.go", Name: "Save", Kind: "func"},
			{ID: "b.go#Caller", File: "b.go", Name: "Caller", Kind: "func"},
		},
		Edges: []Edge{
			{From: "b.go#Caller", To: "a.go#Save", Kind: "call"},
		},
	}
}

// TestExtractQueryFenceStripping pins extractQuery's one job: strip a code
// fence if present, do no natural-language parsing. Empty result is a failure
// upstream, so the input is always non-empty here.
func TestExtractQueryFenceStripping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"```sql\nSELECT 1\n```", "SELECT 1"},
		{"```\nSELECT 1\n```", "SELECT 1"},
		{"  ```cypher\nMATCH (a) RETURN a  \n```  ", "MATCH (a) RETURN a"},
		// No fence: returned verbatim (trimmed) — no NL parsing.
		{"here is the query: SELECT 1", "here is the query: SELECT 1"},
	}
	for _, c := range cases {
		if got := extractQuery(c.in); got != c.want {
			t.Errorf("extractQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRunQuestionCleanSuccess is the happy path: one model call, a well-formed
// query, correct answer, zero retries, and the usage counts recorded.
func TestRunQuestionCleanSuccess(t *testing.T) {
	h := &harness{limit: 100} // nil server: the Cypher path never touches it
	q := Question{ID: "q1", Text: "who calls Save", Truth: []string{"b.go#Caller"}, Match: "set"}
	model := func(system, user string) (string, int, int, time.Duration, error) {
		return "```cypher\nMATCH (a)-[:CALLS]->(b:#'a.go#Save') RETURN a\n```", 10, 3, 5 * time.Millisecond, nil
	}
	res := runQuestion(h, testRows(), q, "cypher", "CARD", model)
	if !res.Correct {
		t.Fatalf("want correct, got correct=false execErr=%q answer=%v", res.ExecErr, res.Answer)
	}
	if res.Retries != 0 {
		t.Errorf("want 0 retries, got %d", res.Retries)
	}
	if res.PromptTok != 10 || res.ComplTok != 3 {
		t.Errorf("want 10 prompt / 3 completion tokens, got %d / %d", res.PromptTok, res.ComplTok)
	}
}

// TestRunQuestionSingleRetry is the discriminator for the retry asymmetry: a
// Cypher parse failure must route through the SAME single-retry path as a GSL
// selector error, so the model gets the parse error back once and the tokens
// from BOTH calls accumulate. Under the pre-fix code a Cypher parse failure
// was a hard miss (one model call, Retries=0), so this test fails there.
func TestRunQuestionSingleRetry(t *testing.T) {
	h := &harness{limit: 100}
	q := Question{ID: "q1", Text: "who calls Save", Truth: []string{"b.go#Caller"}, Match: "set"}
	var calls int
	model := func(system, user string) (string, int, int, time.Duration, error) {
		calls++
		if calls == 1 {
			// Malformed: no (a)-[:TYPE]->(b) pattern → parse failure.
			return "MATCH (a) RETURN a", 10, 3, 5 * time.Millisecond, nil
		}
		// The single retry: the model was fed the parse error back.
		if !strings.Contains(user, "Your query failed with:") {
			t.Errorf("retry user turn does not carry the parse error: %q", user)
		}
		return "```cypher\nMATCH (a)-[:CALLS]->(b:#'a.go#Save') RETURN a\n```", 20, 4, 6 * time.Millisecond, nil
	}
	res := runQuestion(h, testRows(), q, "cypher", "CARD", model)
	if calls != 2 {
		t.Fatalf("want 2 model calls (1 + 1 retry), got %d", calls)
	}
	if !res.Correct {
		t.Errorf("want correct after retry, got correct=false execErr=%q answer=%v", res.ExecErr, res.Answer)
	}
	if res.Retries != 1 {
		t.Errorf("want 1 retry, got %d", res.Retries)
	}
	// Token accumulation across both calls: 10+20 prompt, 3+4 completion.
	if res.PromptTok != 30 {
		t.Errorf("want 30 accumulated prompt tokens, got %d", res.PromptTok)
	}
	if res.ComplTok != 7 {
		t.Errorf("want 7 accumulated completion tokens, got %d", res.ComplTok)
	}
}

// TestRunQuestionSecondFailureIsMiss pins the boundary: the retry is exactly
// ONCE. A second parse failure is a hard miss — no third model call, Retries
// stays 1, and the error is recorded. This also fails against the pre-fix code
// (which made the FIRST failure the miss with one call).
func TestRunQuestionSecondFailureIsMiss(t *testing.T) {
	h := &harness{limit: 100}
	q := Question{ID: "q1", Text: "who calls Save", Truth: []string{"b.go#Caller"}, Match: "set"}
	var calls int
	model := func(system, user string) (string, int, int, time.Duration, error) {
		calls++
		return "MATCH (a) RETURN a", 10, 3, 5 * time.Millisecond, nil // always malformed
	}
	res := runQuestion(h, testRows(), q, "cypher", "CARD", model)
	if calls != 2 {
		t.Fatalf("want exactly 2 model calls (one retry, no third), got %d", calls)
	}
	if res.Correct {
		t.Errorf("want a miss after two parse failures, got correct")
	}
	if res.Retries != 1 {
		t.Errorf("want 1 retry recorded, got %d", res.Retries)
	}
	if res.ExecErr == "" {
		t.Errorf("want an exec error recorded, got none")
	}
}
