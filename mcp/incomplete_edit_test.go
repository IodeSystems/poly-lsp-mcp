package mcp

import (
	"strings"
	"testing"
)

// A dun session on 2026-08-05 looped on the same rejection twice while adding a
// --docker-network flag: `dockerNetwork := flag.Bool(...)` is correct, and
// "declared and not used" is the EXPECTED state until its use is added. The
// recovery (commit:false) was already in the payload as `help` and was already
// correct — but Go serializes map keys alphabetically, so `diagnostics` came
// first and buried it at char ~1600 of 2036.
//
// So this pins the DISCRIMINATION, not the wording: an incomplete edit gets an
// imperative in a field that sorts first, and a genuinely broken one does not —
// telling someone to stage a real mistake would turn a correct rejection into
// bad advice.
func TestIncompleteEditActionDiscriminates(t *testing.T) {
	cases := []struct {
		name   string
		errs   []string
		expect bool
	}{{
		name:   "declared and not used — the flag case that looped",
		errs:   []string{"/x/main.go: UnusedVar: declared and not used: dockerNetwork"},
		expect: true,
	}, {
		name:   "use before its declaration exists",
		errs:   []string{"/x/main.go: UndeclaredName: undefined: dockerNetwork"},
		expect: true,
	}, {
		name:   "an unused import, same shape",
		errs:   []string{"/x/main.go: UnusedImport: imported and not used: \"os\""},
		expect: true,
	}, {
		name:   "several, all incomplete",
		errs:   []string{"/x/a.go: UnusedVar: declared and not used: n", "/x/b.go: undefined: helper"},
		expect: true,
	}, {
		name:   "a real type error is NOT a staging problem",
		errs:   []string{"/x/main.go: InvalidAssign: cannot use \"s\" (string) as int value"},
		expect: false,
	}, {
		name:   "one real error among incomplete ones still disqualifies",
		errs:   []string{"/x/a.go: UnusedVar: declared and not used: n", "/x/b.go: syntax error: unexpected }"},
		expect: false,
	}, {
		name:   "nothing to explain",
		errs:   nil,
		expect: false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := incompleteEditAction(c.errs)
			if (got != "") != c.expect {
				t.Fatalf("expected fired=%v, got %q", c.expect, got)
			}
			if c.expect && !strings.Contains(got, "commit:false") {
				t.Errorf("the action must name the mechanism that recovers; got %q", got)
			}
		})
	}
}

// `action` has to sort before `diagnostics` or it lands behind the evidence
// again, which is the entire failure being fixed.
func TestRejectionActionSortsBeforeDiagnostics(t *testing.T) {
	if !("action" < "diagnostics") {
		t.Fatal("the field name must sort before `diagnostics` in the serialized map")
	}
	oc := editOutcome{
		Rejected:  true,
		NewErrors: []string{"/x/main.go: UnusedVar: declared and not used: dockerNetwork"},
	}
	p := oc.rejection("main.go")
	if _, ok := p["action"]; !ok {
		t.Fatalf("an incomplete-edit rejection must carry `action`; got keys %v", keysOf(p))
	}
	if _, ok := p["help"]; !ok {
		t.Error("`help` stays — it explains the general counterpart-edit case")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
