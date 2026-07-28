package git

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// IgnoreSet is the set of paths git says are ignored in a workspace,
// resolved once and reused. Both the symbol index and the bindings
// resolver consult it, which is why it lives here rather than in either
// of them.
//
// Why this exists: the index walk honoured `skipDirs` — a hardcoded list of
// node_modules, build, dist — but not `.gitignore`. Measured on a real
// repo (zdx-go), 88% of every yaml/json site in the index came from
// GITIGNORED files: tool state, lock files, captured API payloads.
// 462,002 sites of throwaway data against 65,911 of real config, an 8:1
// ratio, all of it costing budget and skewing every cardinality estimate.
//
// IGNORED, not UNTRACKED. The distinction is the whole point: a file an
// agent just created and has not `git add`-ed is untracked but NOT
// ignored, so it keeps indexing. Filtering on `git ls-files` would make
// an agent's own new work invisible to it — the worst possible failure
// for a tool an agent uses mid-task.
type IgnoreSet struct {
	files map[string]bool
	dirs  []string // relative, each ending in "/"
}

// LoadIgnores asks git which paths are ignored under root. Returns nil
// when root is not a git repository, git is unavailable, or the command
// fails — in every one of those cases a walk proceeds unfiltered, which
// is the behaviour that predates this filter. A nil *IgnoreSet is safe
// to call every method on.
func LoadIgnores(root string) *IgnoreSet {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	// --others --ignored --exclude-standard = untracked AND ignored.
	// A TRACKED file is never listed even if a pattern matches it, which
	// is correct: git still tracks it, so it is part of the project.
	// --directory collapses a fully-ignored directory to one entry, so
	// node_modules costs one line instead of a hundred thousand.
	cmd := exec.Command("git", "-C", root,
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	s := &IgnoreSet{files: map[string]bool{}}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			s.dirs = append(s.dirs, p)
			continue
		}
		s.files[p] = true
	}
	if len(s.files) == 0 && len(s.dirs) == 0 {
		// Nothing ignored: skip the per-entry checks entirely.
		return nil
	}
	return s
}

// rel renders an absolute path as the repo-relative, slash-separated
// form git reports. Returns "" when path is not under root.
func (s *IgnoreSet) rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(r)
}

// DirIgnored reports whether a directory is entirely ignored.
func (s *IgnoreSet) DirIgnored(root, path string) bool {
	if s == nil {
		return false
	}
	r := s.rel(root, path)
	if r == "" || r == "." {
		return false
	}
	return s.matchDir(r + "/")
}

// FileIgnored reports whether a file is ignored, either by name or by
// sitting under an ignored directory.
func (s *IgnoreSet) FileIgnored(root, path string) bool {
	if s == nil {
		return false
	}
	r := s.rel(root, path)
	if r == "" {
		return false
	}
	if s.files[r] {
		return true
	}
	return s.matchDir(r)
}

// matchDir reports whether p sits under any ignored directory prefix.
func (s *IgnoreSet) matchDir(p string) bool {
	for _, d := range s.dirs {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	return false
}
