package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
)

// AllowList is the set of directory prefixes a client may address roots
// under. The check is the part that goes wrong in practice, so it is
// deliberate: compare EvalSymlinks'd, Clean'd ABSOLUTE paths and match on
// PATH COMPONENTS — /home/u/local must not admit /home/u/localsecrets,
// and `..` must not walk out. Test the bypasses, not just the happy path.
type AllowList struct {
	prefixes []string // resolved, cleaned, absolute
}

// NewAllowList resolves each prefix to an absolute, symlink-free, cleaned
// path. A prefix that can't be resolved is dropped (it can never match).
// An empty result means "deny everything" — callers default the list to
// $HOME when none is configured.
func NewAllowList(prefixes []string) *AllowList {
	al := &AllowList{}
	for _, p := range prefixes {
		if r, err := resolvePath(p); err == nil {
			al.prefixes = append(al.prefixes, r)
		}
	}
	return al
}

// Resolve validates that root sits under an allowed prefix and returns
// its canonical (symlink-free, absolute, cleaned) form — the key the
// registry uses. An error is returned when root escapes every prefix, so
// the same canonical path both gates access and identifies the server.
func (al *AllowList) Resolve(root string) (string, error) {
	abs, err := resolvePath(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	for _, p := range al.prefixes {
		if pathUnder(abs, p) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("root %s is not under any allowed prefix %v", abs, al.prefixes)
}

// Prefixes returns the resolved allow prefixes (for logging).
func (al *AllowList) Prefixes() []string { return al.prefixes }

// resolvePath makes p absolute, resolves symlinks best-effort (a path
// that doesn't exist yet keeps its cleaned absolute form), and cleans it.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// pathUnder reports whether path is prefix itself or a descendant of it,
// matching on whole path components. Both args must already be cleaned
// absolute paths. filepath.Rel does the component-wise comparison: a
// result that is "." or that does not begin with ".." (and isn't
// absolute) means path is at or below prefix.
func pathUnder(path, prefix string) bool {
	if path == prefix {
		return true
	}
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
