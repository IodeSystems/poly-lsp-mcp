package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manifest is the project's file set as git already knows it: tracked
// files (submodule contents included) plus untracked files that are not
// ignored. A build iterates it instead of walking the tree — git has
// tracked these files already, so there is nothing to rediscover, and an
// ignored tree (node_modules, a venv, 48 agent worktrees) is never stat'ed.
//
// Nested repositories are the one thing git reports without descending: an
// untracked `dir/` that holds its own .git. They are classified, and left
// out with the reason, because they are not this project's code — unlike a
// submodule, which is tracked and comes through --recurse-submodules.
type Manifest struct {
	// Files are root-relative, slash-separated, in git's order.
	Files []string
	// Nested are the nested repositories left out.
	Nested []Nested
}

// Nested is a nested repository git does not descend into.
type Nested struct {
	Dir    string // root-relative, no trailing slash
	Reason string // NestedWorktree or NestedRepo
}

const (
	NestedWorktree = "worktree of this repo"
	NestedRepo     = "another repo"
)

// LoadManifest asks git for root's file set: two `ls-files` calls, since
// --recurse-submodules does not combine with --others. nil when root is not
// in a git repository or git is unavailable; the caller walks instead.
func LoadManifest(root string) *Manifest {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	tracked, err := lsFiles(root, "--cached", "--recurse-submodules")
	if err != nil {
		return nil
	}
	others, err := lsFiles(root, "--others", "--exclude-standard")
	if err != nil {
		return nil
	}
	m := &Manifest{Files: tracked}
	common := commonDir(root)
	for _, p := range others {
		if !strings.HasSuffix(p, "/") {
			m.Files = append(m.Files, p)
			continue
		}
		dir := strings.TrimSuffix(p, "/")
		m.Nested = append(m.Nested, Nested{Dir: dir, Reason: classifyNested(filepath.Join(root, dir), common)})
	}
	return m
}

func lsFiles(root string, args ...string) ([]string, error) {
	out, err := exec.Command("git", append([]string{"-C", root, "ls-files", "-z"}, args...)...).Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			files = append(files, string(p))
		}
	}
	return files, nil
}

// commonDir is this repository's shared .git directory (absolute); its
// worktrees live under <common>/worktrees/.
func commonDir(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// classifyNested: a .git FILE whose gitdir is under this repository's
// worktrees/ is a worktree of this repo; anything else with its own .git is
// another repository.
func classifyNested(dir, common string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil || common == "" {
		return NestedRepo // a .git directory, or unreadable
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return NestedRepo
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if strings.HasPrefix(filepath.Clean(gitdir), filepath.Join(common, "worktrees")+string(filepath.Separator)) {
		return NestedWorktree
	}
	return NestedRepo
}

// NestedDirs is the Dir of every nested repository, for an ignore set.
func (m *Manifest) NestedDirs() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.Nested))
	for i, n := range m.Nested {
		out[i] = n.Dir
	}
	return out
}

// IgnoredDirs lists the fully-ignored directories from the snapshot, for a
// report.
func (s *IgnoreSet) IgnoredDirs() []string {
	if s == nil || s.noGit {
		return nil
	}
	out := make([]string, len(s.dirs))
	for i, d := range s.dirs {
		out[i] = strings.TrimSuffix(d, "/")
	}
	return out
}
