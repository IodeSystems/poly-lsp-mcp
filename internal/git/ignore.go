package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	root  string
	files map[string]bool
	dirs  []string // relative, each ending in "/"

	// mu guards checked, the memo for the pattern fallback below.
	mu      sync.Mutex
	checked map[string]bool
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
	s := &IgnoreSet{root: root, files: map[string]bool{}, checked: map[string]bool{}}
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
	// Even with nothing currently ignored the set is kept: `.gitignore`
	// may match paths that do not exist YET, and Matches falls back to
	// git for those.
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

// FileIgnored reports whether a file is ignored, either by name, by
// sitting under an ignored directory, or by matching a pattern.
//
// The snapshot from LoadIgnores only enumerates paths that EXISTED when
// it ran, which is enough for the build walk but not for a watcher: a
// build output or a branch switch creates ignored files afterwards. Those
// fall through to `git check-ignore`, memoized per directory so a run
// that writes a thousand files into one ignored tree costs one
// subprocess, not a thousand.
func (s *IgnoreSet) FileIgnored(root, path string) bool {
	if s == nil {
		return false
	}
	r := s.rel(root, path)
	if r == "" {
		return false
	}
	if s.files[r] || s.matchDir(r) {
		return true
	}
	if dir := filepath.Dir(r); dir != "." && dir != "" {
		if s.checkIgnore(dir + "/") {
			return true
		}
	}
	return s.checkIgnore(r)
}

// checkIgnore asks git whether one relative path is ignored, memoizing
// the answer. Returns false when git cannot answer.
func (s *IgnoreSet) checkIgnore(rel string) bool {
	s.mu.Lock()
	if v, ok := s.checked[rel]; ok {
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()

	cmd := exec.Command("git", "-C", s.root, "check-ignore", "-q", "--", rel)
	// Exit 0 = ignored, 1 = not ignored, anything else = error.
	ignored := cmd.Run() == nil

	s.mu.Lock()
	s.checked[rel] = ignored
	s.mu.Unlock()
	return ignored
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
