package symbols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sibling declarations must never overlap. The rules that grow a decl span
// beyond its raw node — the doc block above it, the comment trailing it —
// are the ones that can push one declaration into its neighbour, and a span
// that overlaps is not a reporting bug: node_edit REPLACES the span, so the
// neighbour's text is destroyed.
//
// This sweeps the repo as a live corpus rather than a fixture because the
// case that broke it was not one anybody would have written by hand: c's
// preproc_def swallows its terminating newline, so `#define A 1` reports an
// end at column 1 of the NEXT line and claimed the comment trailing the
// #define below it. Point SWEEP_ROOT at a larger tree to widen the sweep;
// measured at 0 overlaps over 22,859 files / 2.2M symbols.
func TestSiblingSpansDoNotOverlap(t *testing.T) {
	root := os.Getenv("SWEEP_ROOT")
	if root == "" {
		root = ".."
	}
	byExt := map[string]string{
		".go": "go", ".ts": "typescript", ".tsx": "typescript", ".py": "python",
		".java": "java", ".kt": "kotlin", ".c": "c", ".h": "c", ".cpp": "cpp",
	}

	files, symCount, overlaps := 0, 0, 0
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			switch fi.Name() {
			case ".git", "bin", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		lang, ok := byExt[strings.ToLower(filepath.Ext(p))]
		if !ok {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		syms, err := FileSymbols(lang, b)
		if err != nil {
			return nil
		}
		files++
		symCount += len(syms)
		for i := range syms {
			a := syms[i]
			if a.DeclStartLine == 0 {
				continue
			}
			for j := range syms {
				if i == j {
					continue
				}
				c := syms[j]
				if c.DeclStartLine == 0 || !sameParent(a.Sym, c.Sym) {
					continue
				}
				if posAfter(a.DeclStartLine, a.DeclStartCol, c.DeclStartLine, c.DeclStartCol) &&
					posAfter(c.DeclEndLine, c.DeclEndCol, a.DeclStartLine, a.DeclStartCol) {
					overlaps++
					if overlaps <= 10 {
						t.Errorf("%s: %s [%d:%d..%d:%d] starts inside its sibling %s [%d:%d..%d:%d]",
							p, a.Sym, a.DeclStartLine, a.DeclStartCol, a.DeclEndLine, a.DeclEndCol,
							c.Sym, c.DeclStartLine, c.DeclStartCol, c.DeclEndLine, c.DeclEndCol)
					}
				}
			}
		}
		return nil
	})
	if files == 0 {
		t.Fatalf("swept no files under %q — the corpus is not where this test thinks", root)
	}
	t.Logf("swept %d files, %d symbols, %d overlaps", files, symCount, overlaps)
}

// sameParent reports whether two dotted symbol paths sit at the same level
// under the same owner — only siblings can overlap illegitimately, since a
// child is SUPPOSED to sit inside its parent's span.
func sameParent(a, b string) bool {
	i, j := strings.LastIndex(a, "."), strings.LastIndex(b, ".")
	if i != j {
		return false
	}
	return i < 0 || a[:i] == b[:j]
}

func posAfter(l1, c1, l2, c2 int) bool {
	if l1 != l2 {
		return l1 > l2
	}
	return c1 > c2
}
