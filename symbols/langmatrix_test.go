package symbols

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// A BUG CLASS is one property asserted the same way in every language whose
// grammar has the construct, run against the CANONICAL language registry
// rather than a hand-written list. That distinction is the point: the
// hand-written table that shipped with the trailing-comment fix covered five
// languages and silently omitted java — which was the one still broken.
//
// Every registered language must land in exactly one cell:
//
//	ok         the property holds
//	n/a        the language cannot have the property, with a reason
//	KNOWN      the property is violated, recorded deliberately with a reason
//	unmeasured NOTHING says anything about it — this FAILS
//
// A KNOWN entry that starts passing also FAILS, so a fix cannot leave a stale
// "we don't support that" note behind. Adding a language to config.Default()
// therefore breaks this test until someone states, per class, which cell it
// belongs in.
type bugClass struct {
	name string
	// what the property is, in one line, for the failure message.
	property string
	// language → fixture. want maps a symbol's dotted path to the EXACT
	// source text its declaration span must cover.
	fixtures map[string]spanFixture
	// language → why the property cannot apply.
	na map[string]string
	// language → why it is violated today.
	known map[string]string
}

type spanFixture struct {
	src  string
	want map[string]string
}

func TestLanguageBugMatrix(t *testing.T) {
	classes := []bugClass{trailingCommentClass(), docBlockAboveClass()}

	type cell struct{ state, note string }
	grid := map[string]map[string]cell{}

	for _, bc := range classes {
		grid[bc.name] = map[string]cell{}
		for _, lang := range config.Default().Languages {
			name := lang.Name
			// A language with no structural grammar cannot be asserted on at
			// all — FileSymbols has nothing to walk. That is a fact about the
			// registry, not a judgement, so it is derived rather than listed.
			if LanguageByName(name) == nil && name != "xml" {
				grid[bc.name][name] = cell{"n/a", "lexical tier — no symbol grammar"}
				continue
			}
			if why, ok := bc.na[name]; ok {
				grid[bc.name][name] = cell{"n/a", why}
				continue
			}
			fx, ok := bc.fixtures[name]
			if !ok {
				grid[bc.name][name] = cell{"UNMEASURED", ""}
				t.Errorf("bug class %q says nothing about %q.\n"+
					"  The property: %s\n"+
					"  Add a fixture, an n/a reason, or a known-violation reason.",
					bc.name, name, bc.property)
				continue
			}
			bad := checkSpanFixture(name, fx)
			why, isKnown := bc.known[name]
			switch {
			case len(bad) == 0 && isKnown:
				grid[bc.name][name] = cell{"FIXED", why}
				t.Errorf("bug class %q is recorded as violated in %q, but it now HOLDS.\n"+
					"  Recorded reason: %s\n"+
					"  Delete the known-violation entry.", bc.name, name, why)
			case len(bad) == 0:
				grid[bc.name][name] = cell{"ok", ""}
			case isKnown:
				grid[bc.name][name] = cell{"KNOWN", why}
			default:
				grid[bc.name][name] = cell{"VIOLATED", strings.Join(bad, "; ")}
				t.Errorf("bug class %q is violated in %q.\n  The property: %s\n  %s",
					bc.name, name, bc.property, strings.Join(bad, "\n  "))
			}
		}
	}

	// The matrix itself, so a passing run still shows the coverage.
	var langs []string
	for _, l := range config.Default().Languages {
		langs = append(langs, l.Name)
	}
	sort.Strings(langs)
	for _, bc := range classes {
		t.Logf("=== %s", bc.name)
		for _, l := range langs {
			c := grid[bc.name][l]
			if c.note != "" {
				t.Logf("    %-12s %-10s %s", l, c.state, c.note)
			} else {
				t.Logf("    %-12s %s", l, c.state)
			}
		}
	}
}

// checkSpanFixture returns one complaint per symbol whose declaration span
// does not cover exactly the expected text.
func checkSpanFixture(lang string, fx spanFixture) []string {
	syms, err := FileSymbols(lang, []byte(fx.src))
	if err != nil {
		return []string{fmt.Sprintf("FileSymbols: %v", err)}
	}
	bySym := map[string]Symbol{}
	for _, s := range syms {
		bySym[s.Sym] = s
	}
	var bad []string
	for _, name := range sortedKeys(fx.want) {
		want := fx.want[name]
		s, ok := bySym[name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: not indexed at all (got %v)", name, sortedSyms(syms)))
			continue
		}
		if got := spanTextOf(fx.src, s); got != want {
			bad = append(bad, fmt.Sprintf("%s: span covers %q, want %q", name, got, want))
		}
	}
	return bad
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSyms(syms []Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Sym)
	}
	sort.Strings(out)
	return out
}

// spanTextOf slices src by a symbol's 1-based, end-exclusive decl span.
func spanTextOf(src string, s Symbol) string {
	lines := strings.Split(src, "\n")
	if s.DeclStartLine < 1 || s.DeclEndLine > len(lines) || s.DeclStartLine > s.DeclEndLine {
		return ""
	}
	clamp := func(c int, l string) int {
		if c < 0 {
			return 0
		}
		if c > len(l) {
			return len(l)
		}
		return c
	}
	if s.DeclStartLine == s.DeclEndLine {
		l := lines[s.DeclStartLine-1]
		a, b := clamp(s.DeclStartCol-1, l), clamp(s.DeclEndCol-1, l)
		if a > b {
			return ""
		}
		return l[a:b]
	}
	first := lines[s.DeclStartLine-1]
	out := []string{first[clamp(s.DeclStartCol-1, first):]}
	out = append(out, lines[s.DeclStartLine:s.DeclEndLine-1]...)
	last := lines[s.DeclEndLine-1]
	out = append(out, last[:clamp(s.DeclEndCol-1, last)])
	return strings.Join(out, "\n")
}
