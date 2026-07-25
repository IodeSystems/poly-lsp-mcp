package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPathUnder pins the component-wise containment check — the part the
// plan flags as "goes wrong in practice". String-prefix bypasses
// (/home/u/local vs /home/u/localsecrets) and `..` escapes must be
// rejected; genuine descendants and the prefix itself must be accepted.
func TestPathUnder(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/home/u/local", "/home/u/local", true},          // identical
		{"/home/u/local/x", "/home/u/local", true},        // descendant
		{"/home/u/local/x/y/z", "/home/u/local", true},    // deep descendant
		{"/home/u/localsecrets", "/home/u/local", false},  // string-prefix bypass
		{"/home/u/local-evil", "/home/u/local", false},    // string-prefix bypass
		{"/home/u", "/home/u/local", false},               // ancestor, not under
		{"/home/other/local", "/home/u/local", false},     // sibling tree
		{"/etc/passwd", "/home/u/local", false},           // unrelated
	}
	for _, c := range cases {
		// pathUnder assumes cleaned inputs; clean here to mirror the
		// AllowList contract (Resolve cleans before calling).
		got := pathUnder(filepath.Clean(c.path), filepath.Clean(c.prefix))
		if got != c.want {
			t.Errorf("pathUnder(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

// TestAllowListResolveRejectsEscapes checks the full Resolve path,
// including `..` traversal in the requested root, against a real temp
// tree (EvalSymlinks needs the paths to exist).
func TestAllowListResolveRejectsEscapes(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	sibling := filepath.Join(base, "allowedsecrets") // string-prefix bypass target
	inside := filepath.Join(allowed, "proj")
	for _, d := range []string{allowed, sibling, inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	al := NewAllowList([]string{allowed})

	if got, err := al.Resolve(inside); err != nil {
		t.Errorf("Resolve(inside) rejected: %v", err)
	} else if got != inside {
		t.Errorf("Resolve(inside) = %q, want canonical %q", got, inside)
	}

	// The sibling whose name shares a prefix must be rejected.
	if _, err := al.Resolve(sibling); err == nil {
		t.Errorf("Resolve(%q) accepted — string-prefix bypass not blocked", sibling)
	}

	// A `..` walk out of the allowed dir must resolve+reject.
	escape := filepath.Join(allowed, "..", "allowedsecrets")
	if _, err := al.Resolve(escape); err == nil {
		t.Errorf("Resolve(%q) accepted — `..` escape not blocked", escape)
	}
}

// TestAllowListSymlinkEscape ensures a symlink pointing outside the
// allowed prefix is rejected: EvalSymlinks resolves it before the check,
// so a link inside `allowed` aimed at `/etc` can't smuggle access.
func TestAllowListSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	al := NewAllowList([]string{allowed})
	if _, err := al.Resolve(link); err == nil {
		t.Errorf("Resolve(symlink→outside) accepted — symlink escape not blocked")
	}
}

// TestAllowListEmptyDeniesAll confirms an empty allow-list rejects
// everything (the daemon refuses to start with one, but the type must be
// safe if constructed empty).
func TestAllowListEmptyDeniesAll(t *testing.T) {
	al := NewAllowList(nil)
	if _, err := al.Resolve(t.TempDir()); err == nil {
		t.Error("empty AllowList admitted a root")
	}
}
