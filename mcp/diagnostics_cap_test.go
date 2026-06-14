package mcp

import (
	"strings"
	"testing"
)

func TestClampMessage(t *testing.T) {
	short := "all good"
	if got := clampMessage(short); got != short {
		t.Errorf("clampMessage(short) = %q, want unchanged", got)
	}

	long := strings.Repeat("x", maxDiagnosticMessageChars+50)
	got := clampMessage(long)
	if r := []rune(got); len(r) != maxDiagnosticMessageChars+1 { // +1 for the ellipsis
		t.Errorf("clampMessage(long) rune len = %d, want %d", len(r), maxDiagnosticMessageChars+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clampMessage(long) missing ellipsis marker: %q", got)
	}
}

// TestCapBytesEnforcesBudget proves a flood of fat diagnostics can't
// blow the budget: enrichment is shed, core fields survive, and the
// serialized payload ends up under the cap. Mirrors the real overflow
// (editing a module outside the workspace root → 25 verbose items).
func TestCapBytesEnforcesBudget(t *testing.T) {
	bigMsg := strings.Repeat("y", 200)
	refs := make([]siteJSON, 15)
	for i := range refs {
		refs[i] = siteJSON{Name: "Sym", File: strings.Repeat("p", 80), Line: i, Col: 1, Confidence: "lexical"}
	}
	ed := editDiagnostics{Available: true}
	for i := 0; i < 25; i++ {
		ed.Items = append(ed.Items, diagnosticJSON{
			File:       "a.go",
			Severity:   "error",
			Message:    bigMsg,
			StartLine:  i,
			References: append([]siteJSON(nil), refs...),
			Text:       strings.Repeat("t", 100),
		})
	}
	if jsonLen(ed) <= maxDiagnosticsBytes {
		t.Fatalf("test setup: payload not oversized (%d <= %d)", jsonLen(ed), maxDiagnosticsBytes)
	}

	capped := ed.capBytes(maxDiagnosticsBytes)

	if got := jsonLen(capped); got > maxDiagnosticsBytes {
		t.Errorf("capBytes left payload at %d bytes, want <= %d", got, maxDiagnosticsBytes)
	}
	if len(capped.Items) == 0 {
		t.Fatal("capBytes dropped every item; core diagnostics lost")
	}
	if capped.Items[0].Message == "" {
		t.Error("capBytes stripped the message (a core field) from a surviving item")
	}
}

// TestCapBytesUnderBudgetIsNoop: a small payload passes through with
// all enrichment intact.
func TestCapBytesUnderBudgetIsNoop(t *testing.T) {
	ed := editDiagnostics{
		Available: true,
		Items: []diagnosticJSON{{
			File:       "a.go",
			Severity:   "error",
			Message:    "boom",
			References: []siteJSON{{Name: "X", File: "a.go", Line: 1, Col: 1}},
		}},
	}
	capped := ed.capBytes(maxDiagnosticsBytes)
	if capped.Items[0].References == nil {
		t.Error("capBytes shed enrichment on an under-budget payload; want no-op")
	}
	if capped.DroppedDiagnostics != 0 {
		t.Errorf("DroppedDiagnostics = %d, want 0 under budget", capped.DroppedDiagnostics)
	}
}
