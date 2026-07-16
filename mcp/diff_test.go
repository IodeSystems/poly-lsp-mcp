package mcp

import (
	"strings"
	"testing"
)

func TestApplyUnifiedDiffSimpleReplace(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\ndelta\n"
	diff := `--- a/x
+++ b/x
@@ -2,2 +2,2 @@
-bravo
+bravo MODIFIED
 charlie
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\nbravo MODIFIED\ncharlie\ndelta\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyUnifiedDiffInsertion(t *testing.T) {
	orig := "alpha\nbravo\n"
	diff := `@@ -1,2 +1,3 @@
 alpha
+inserted
 bravo
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\ninserted\nbravo\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyUnifiedDiffDeletion(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\n"
	diff := `@@ -1,3 +1,2 @@
 alpha
-bravo
 charlie
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\ncharlie\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyUnifiedDiffMultipleHunks(t *testing.T) {
	// Generate 10 lines.
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("line ")
		b.WriteByte(byte('0' + i%10))
		b.WriteString("\n")
	}
	orig := b.String()
	diff := `@@ -2,1 +2,1 @@
-line 2
+LINE TWO
@@ -8,1 +8,1 @@
-line 8
+LINE EIGHT
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "LINE TWO") {
		t.Errorf("first hunk missed: %s", got)
	}
	if !strings.Contains(string(got), "LINE EIGHT") {
		t.Errorf("second hunk missed: %s", got)
	}
	if strings.Contains(string(got), "line 2\n") || strings.Contains(string(got), "line 8\n") {
		t.Errorf("originals not removed: %s", got)
	}
}

func TestApplyUnifiedDiffContextMismatchIsError(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\n"
	diff := `@@ -1,3 +1,3 @@
 alpha
-bravo DRIFTED
+changed
 charlie
`
	_, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err == nil {
		t.Error("expected error on context mismatch")
	}
}

func TestApplyUnifiedDiffHunkOutOfOrderIsError(t *testing.T) {
	orig := "a\nb\nc\nd\n"
	diff := `@@ -3,1 +3,1 @@
-c
+C
@@ -1,1 +1,1 @@
-a
+A
`
	_, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err == nil {
		t.Error("expected error on out-of-order hunks")
	}
}

func TestApplyUnifiedDiffPreservesTrailingNewline(t *testing.T) {
	orig := "a\nb\n"
	diff := `@@ -1,1 +1,1 @@
-a
+A
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("trailing newline dropped: %q", got)
	}
}

func TestApplyUnifiedDiffCRLFInputNormalizes(t *testing.T) {
	orig := "alpha\r\nbravo\r\n"
	diff := `@@ -1,2 +1,2 @@
 alpha
-bravo
+bravo!
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alpha\nbravo!\n" {
		t.Errorf("CRLF not normalized: %q", got)
	}
}

func TestApplyUnifiedDiffEmptyDiffIsError(t *testing.T) {
	_, err := ApplyUnifiedDiff([]byte("anything\n"), "")
	if err == nil {
		t.Error("expected error on empty diff")
	}
}

// TestApplyUnifiedDiffFuzzyHeaderOffsetStillApplies is the dominant
// real-world failure this fix targets: an LLM emits correct context
// lines but a wrong @@ header (here +2 off — claims line 4 when the
// real match is line 2). A strict-anchor applier rejects this outright;
// the fuzzy applier locates the unique context match and applies.
func TestApplyUnifiedDiffFuzzyHeaderOffsetStillApplies(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\ndelta\necho\n"
	diff := `@@ -4,2 +4,2 @@
-bravo
+bravo MODIFIED
 charlie
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatalf("expected fuzzy match to apply despite wrong header, got error: %v", err)
	}
	want := "alpha\nbravo MODIFIED\ncharlie\ndelta\necho\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestApplyUnifiedDiffFuzzyHeaderNegativeOffsetStillApplies: same as
// above but the header undershoots (-1) instead of overshoots.
func TestApplyUnifiedDiffFuzzyHeaderNegativeOffsetStillApplies(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\ndelta\necho\n"
	diff := `@@ -3,2 +3,2 @@
-delta
+DELTA
 echo
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatalf("expected fuzzy match to apply despite wrong header, got error: %v", err)
	}
	want := "alpha\nbravo\ncharlie\nDELTA\necho\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestApplyUnifiedDiffNonMatchingContextStillErrors: context that
// genuinely doesn't exist anywhere in the file must still error, not
// silently apply somewhere wrong.
func TestApplyUnifiedDiffNonMatchingContextStillErrors(t *testing.T) {
	orig := "alpha\nbravo\ncharlie\n"
	diff := `@@ -1,3 +1,3 @@
 alpha
-this line does not exist
+changed
 charlie
`
	_, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err == nil {
		t.Fatal("expected error: context not present anywhere in file")
	}
	if !strings.Contains(err.Error(), "context not found") {
		t.Errorf("expected 'context not found' error, got: %v", err)
	}
}

// TestApplyUnifiedDiffAmbiguousContextIsError: the pattern matches
// multiple locations and the (wrong) header doesn't point at any of
// them, so applying would be a guess — must error instead of picking
// one. The error must be actionable: list every matched line number
// and steer the caller toward the range-edit shape (startLine/endLine),
// which needs no line-arithmetic guessing the way diff hunk headers do.
func TestApplyUnifiedDiffAmbiguousContextIsError(t *testing.T) {
	orig := "same\nfiller\nsame\nfiller\nsame\n"
	diff := `@@ -99,1 +99,1 @@
-same
+CHANGED
`
	_, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err == nil {
		t.Fatal("expected ambiguous-match error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
	for _, line := range []string{"1", "3", "5"} {
		if !strings.Contains(msg, line) {
			t.Errorf("expected match line %q listed in error, got: %v", line, err)
		}
	}
	if !strings.Contains(msg, "startLine") || !strings.Contains(msg, "endLine") {
		t.Errorf("expected error to steer toward range-edit retry (startLine/endLine), got: %v", err)
	}
}

// TestApplyUnifiedDiffHintDisambiguatesDuplicateContext: same
// duplicate-context setup, but this time the header DOES point at one
// of the two matches — that's enough to disambiguate and apply.
func TestApplyUnifiedDiffHintDisambiguatesDuplicateContext(t *testing.T) {
	orig := "same\nsame\n"
	diff := `@@ -2,1 +2,1 @@
-same
+CHANGED
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatalf("expected header to disambiguate, got error: %v", err)
	}
	want := "same\nCHANGED\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestApplyUnifiedDiffFuzzyMultiHunkStillWorks: multi-hunk patches
// with off-by-N headers on EACH hunk still apply in order.
func TestApplyUnifiedDiffFuzzyMultiHunkStillWorks(t *testing.T) {
	orig := "one\ntwo\nthree\nfour\nfive\n"
	diff := `@@ -99,1 +99,1 @@
-one
+ONE
@@ -1,1 +1,1 @@
-four
+FOUR
`
	got, err := ApplyUnifiedDiff([]byte(orig), diff)
	if err != nil {
		t.Fatalf("expected fuzzy multi-hunk to apply, got error: %v", err)
	}
	want := "ONE\ntwo\nthree\nFOUR\nfive\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
