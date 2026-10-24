package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// AllowList is the set of directory prefixes a client may address roots
// under. The check is the part that goes wrong in practice, so it is
// deliberate: compare EvalSymlinks'd, Clean'd ABSOLUTE paths and match on
// PATH COMPONENTS — /home/u/local must not admit /home/u/localsecrets,
// and `..` must not walk out. Test the bypasses, not just the happy path.
//
// The list is a starting list, not a boundary: the socket admits only
// peers of the daemon's own uid (credListener), and such a peer can run
// poly-lsp in-process on any root. So a root a client OPENS outside every
// prefix is added (Admit); only the entry points that take a root without
// an open (/call, /access, /query) still refuse one not on the list.
type AllowList struct {
	mu       sync.RWMutex
	prefixes []string // resolved, cleaned, absolute
	added    []string // roots Admit added, in order (a subset of prefixes)
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
	al.mu.RLock()
	defer al.mu.RUnlock()
	if al.under(abs) {
		return abs, nil
	}
	return "", fmt.Errorf("root %s is not under any allowed prefix %v", abs, al.prefixes)
}

// Admit is Resolve for an open: a root outside every prefix is added to
// the list (as its canonical form) instead of refused. added reports
// that it was, for the caller to log.
func (al *AllowList) Admit(root string) (canonical string, added bool, err error) {
	abs, err := resolvePath(root)
	if err != nil {
		return "", false, fmt.Errorf("resolve root %q: %w", root, err)
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	if al.under(abs) {
		return abs, false, nil
	}
	// Only a directory is added: a typo must not grow the list.
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", false, fmt.Errorf("root %s is not a directory", abs)
	}
	al.prefixes = append(al.prefixes, abs)
	al.added = append(al.added, abs)
	return abs, true, nil
}

// under reports whether abs is under a prefix; al.mu must be held.
func (al *AllowList) under(abs string) bool {
	for _, p := range al.prefixes {
		if pathUnder(abs, p) {
			return true
		}
	}
	return false
}

// Prefixes returns the resolved allow prefixes, the added roots included
// (for logging).
func (al *AllowList) Prefixes() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return slices.Clone(al.prefixes)
}

// Added returns the roots Admit added, for a takeover to carry
// (--allowed) so the new daemon admits what this one did.
func (al *AllowList) Added() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return slices.Clone(al.added)
}

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
