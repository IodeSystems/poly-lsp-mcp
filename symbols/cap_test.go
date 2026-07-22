package symbols_test

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

func TestCapHitLineShortLineUnchanged(t *testing.T) {
	line := "func Foo() {} // TODO"
	if got := symbols.CapHitLine(line, 15, 19); got != line {
		t.Errorf("a short line must pass through unchanged; got %q", got)
	}
}

func TestCapHitLineElidesAroundMatch(t *testing.T) {
	// A 40k-char generated line with the match in the middle.
	head := strings.Repeat("a", 20000)
	tail := strings.Repeat("b", 20000)
	line := head + "NEEDLE" + tail
	start := len(head)
	got := symbols.CapHitLine(line, start, start+len("NEEDLE"))

	if len(got) >= len(line) {
		t.Fatalf("cap must shrink the line: got %d, orig %d", len(got), len(line))
	}
	if len(got) > 700 { // budget + two markers, comfortably under
		t.Errorf("capped line too long: %d bytes", len(got))
	}
	if !strings.Contains(got, "NEEDLE") {
		t.Errorf("the match must survive the cap; got %q", got[:80])
	}
	if !strings.Contains(got, "chars)") {
		t.Errorf("an elided line must carry a (+N chars) marker; got %q", got[:80])
	}
	if !utf8.ValidString(got) {
		t.Error("capped line must be valid UTF-8")
	}
}

func TestCapHitLineRuneSafe(t *testing.T) {
	// Multi-byte runes straddling the cut points must not be split.
	line := strings.Repeat("é", 5000) + "NEEDLE" + strings.Repeat("ü", 5000)
	start := strings.Index(line, "NEEDLE")
	got := symbols.CapHitLine(line, start, start+len("NEEDLE"))
	if !utf8.ValidString(got) {
		t.Errorf("cut landed mid-rune: %q", got)
	}
	if !strings.Contains(got, "NEEDLE") {
		t.Error("match lost")
	}
}

// A real (non-generated) file with one long line still returns its match,
// trimmed — the whole file is NOT skipped (its longest line is under the
// generated threshold), but the matched line is capped.
func TestSearchCapsLongMatchedLine(t *testing.T) {
	dir := t.TempDir()
	// One ~2k line: over the per-hit cap (500) but under the generated
	// threshold (5000), so the file is searched and the line trimmed.
	long := "x TODO " + strings.Repeat("y", 2000)
	writeTree(t, dir, map[string]string{"a.txt": long + "\n"})

	pat := regexp.MustCompile(`TODO`)
	hits, _, skipped, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("file is under the generated threshold; skipped=%d", skipped)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if len(hits[0].Text) > 700 {
		t.Errorf("matched line not capped: %d bytes", len(hits[0].Text))
	}
	if !strings.Contains(hits[0].Text, "TODO") {
		t.Error("cap dropped the match")
	}
}

// A generated/minified file (a line past the threshold) is skipped whole
// and counted — the loud-not-silent contract.
func TestSearchSkipsGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"real.txt":      "TODO real match\n",
		"bundle.min.js": "var x=TODO;" + strings.Repeat("z", 6000) + "\n",
	})

	pat := regexp.MustCompile(`TODO`)
	hits, _, skipped, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("the generated file must be skipped and counted; skipped=%d", skipped)
	}
	if len(hits) != 1 {
		t.Fatalf("only the real file's match should return; got %d: %+v", len(hits), hits)
	}
	if !strings.HasSuffix(hits[0].File, "real.txt") {
		t.Errorf("hit not from the real file: %+v", hits[0])
	}

	// Opt back in: IncludeGenerated searches it (and the long line is
	// still capped, so the budget stays bounded).
	hits2, _, skipped2, err := symbols.Search(dir, pat, symbols.SearchOptions{IncludeGenerated: true})
	if err != nil {
		t.Fatal(err)
	}
	if skipped2 != 0 || len(hits2) != 2 {
		t.Errorf("IncludeGenerated must search the generated file: skipped=%d hits=%d", skipped2, len(hits2))
	}
	for _, h := range hits2 {
		if len(h.Text) > 700 {
			t.Errorf("even an included generated line must be capped: %d bytes", len(h.Text))
		}
	}
}
