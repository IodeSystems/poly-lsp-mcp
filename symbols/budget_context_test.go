package symbols

import (
	"strings"
	"testing"
)

// BudgetHitContext keeps the whole hit within maxHitTotalBytes: context
// fills whatever the matched line leaves, nearest-first and contiguous.
func TestBudgetHitContext(t *testing.T) {
	// Short match, short lines: all context fits, both sides kept whole.
	b := []string{"b2", "b1"} // file order: far, near
	a := []string{"a1", "a2"} // near, far
	ob, oa := BudgetHitContext(10, b, a)
	if strings.Join(ob, ",") != "b2,b1" || strings.Join(oa, ",") != "a1,a2" {
		t.Fatalf("small hit should keep all context; got before=%v after=%v", ob, oa)
	}

	// Match line already at the budget: no room for any context.
	ob, oa = BudgetHitContext(maxHitTotalBytes, b, a)
	if ob != nil || oa != nil {
		t.Errorf("a full-budget match line leaves no context; got before=%v after=%v", ob, oa)
	}

	// Tight budget: nearest lines kept, far lines dropped, and the KEPT
	// context is contiguous with the match (near end inward).
	line := strings.Repeat("x", 100) // 100 bytes + 1 = 101 per line
	before := []string{"FAR" + line, "NEAR" + line}
	after := []string{"NEAR" + line, "FAR" + line}
	// remaining = 500 - 250 = 250 -> fits ~2 lines total (after-near, before-near).
	ob, oa = BudgetHitContext(250, before, after)
	if len(oa) != 1 || !strings.HasPrefix(oa[0], "NEAR") {
		t.Errorf("tight budget keeps nearest after line only; got %v", oa)
	}
	if len(ob) != 1 || !strings.HasPrefix(ob[0], "NEAR") {
		t.Errorf("tight budget keeps nearest before line (contiguous); got %v", ob)
	}
}

// A single overlong context line doesn't get partially kept — it either
// fits or that whole side stops (no half-line).
func TestBudgetHitContextStopsSideOnOverflow(t *testing.T) {
	big := strings.Repeat("y", 480)
	after := []string{big, "small"}
	_, oa := BudgetHitContext(50, nil, after)
	// remaining = 450; big is 481 -> doesn't fit -> after side stops, nothing kept.
	if len(oa) != 0 {
		t.Errorf("an overlong nearest line stops the side with nothing kept; got %v", oa)
	}
}
