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
