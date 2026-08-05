package mcp

import "testing"

// `mod` for `module` cost a dun session two calls on 2026-08-05 — and `module`
// was printed three words into the very list the error showed it. The answer
// was present and not pointed at.
//
// The bar has to stay tight in the OTHER direction, though: a bare word in a
// tag position is usually a workspace NAME (`DockerExec`), and suggesting a tag
// there sends the caller to retry a tag when the `#id` form was what they
// needed. So a wrong suggestion is worse than none, and both halves are pinned.
func TestNearestTypeSuggestsOnlyRealMisspellings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mod", "module"},    // truncation — the observed case
		{"clas", "class"},    // one edit
		{"methd", "method"},  // one edit
		{"fiel", "field"},    // truncation
		{"Module", "module"}, // case alone
		{"DockerExec", ""},   // a workspace name, not a tag
		{"zzz", ""},          // nothing like a tag
		{"Handler", ""},      // a type name
		{"cache", ""},        // a directory name
		{"", ""},             // nothing to suggest
	}
	for _, c := range cases {
		if got := nearestType(c.in); got != c.want {
			t.Errorf("nearestType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The suggestion must not displace the id reading, which is the answer for the
// commoner mistake.
func TestUnknownTypeErrKeepsTheIdReading(t *testing.T) {
	if msg := unknownTypeErr("DockerExec").Error(); !contains(msg, "#DockerExec") {
		t.Errorf("a name-shaped word must still be offered as an id; got %q", msg)
	}
	msg := unknownTypeErr("mod").Error()
	if !contains(msg, `did you mean "module"`) {
		t.Errorf("a misspelling should be named; got %q", msg)
	}
	if !contains(msg, "#mod") {
		t.Errorf("the id reading must survive alongside the suggestion — `mod` could still be a dir; got %q", msg)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
