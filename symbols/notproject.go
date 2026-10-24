package symbols

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A root that is not a project is refused before any build.
//
// A client started in the home directory, or in a directory that holds
// clones side by side (~/src), indexes every repository under it as one
// workspace: the budget is spent, and what is indexed is not the project
// the user is working on. The check decides for the user, says so, and
// names both ways out: a narrower --root, or --index-anyway.
//
// It must cost milliseconds: an Lstat per ancestor for "inside a repo", and
// one ReadDir of the root (an Lstat of each subdirectory's .git). No walk.

// LeftNotProject is the reason a not-a-project refusal carries.
const LeftNotProject = "not a project"

// notProjectRepos is how many sibling repositories a refusal names.
const notProjectRepos = 20

// clonesMin is how many immediate subdirectories must be repositories for a
// root outside any repository to count as a directory of clones. Two is a
// project with a vendored checkout as often as not; three is a pattern.
const clonesMin = 3

// NotProjectError is a root refused as not a project.
type NotProjectError struct {
	Root string
	Why  string // "the home directory", "the filesystem root", "a directory of N git repositories"
	// Repos are the git repositories among the root's immediate
	// subdirectories, relative to it, sorted, hidden ones last; at most
	// notProjectRepos.
	Repos []string
	// Total is how many there are; more than len(Repos) when capped.
	Total int
}

func (e *NotProjectError) Error() string {
	return fmt.Sprintf("%s: %s is %s", LeftNotProject, e.Root, e.Why)
}

// Remedy names the ways out.
func (e *NotProjectError) Remedy() string {
	first := "<a project directory>"
	if len(e.Repos) > 0 {
		first = filepath.Join(e.Root, e.Repos[0])
	}
	return fmt.Sprintf("pass --root <one project> (e.g. --root %s), or --index-anyway to index %s regardless", first, e.Root)
}

// Detail is the refusal for a person: the reason, the repositories found,
// and the remedy.
func (e *NotProjectError) Detail() string {
	var b strings.Builder
	b.WriteString(e.Error() + "\n")
	if e.Total > 0 {
		fmt.Fprintf(&b, "git repositories under it (%d): %s", e.Total, strings.Join(e.Repos, ", "))
		if e.Total > len(e.Repos) {
			fmt.Fprintf(&b, " … and %d more", e.Total-len(e.Repos))
		}
		b.WriteByte('\n')
	}
	b.WriteString("remedy: " + e.Remedy() + "\n")
	return b.String()
}

// CheckProjectRoot refuses a root that is not a project: the home
// directory, the filesystem root, or a directory outside any repository
// whose immediate subdirectories include clonesMin or more repositories.
// nil when root looks like a project.
func CheckProjectRoot(root string) *NotProjectError {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	real := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		real = r
	}
	why := ""
	switch {
	case real == string(filepath.Separator) || filepath.Dir(real) == real:
		why = "the filesystem root"
	case isHome(real):
		why = "the home directory"
	}
	if why == "" && insideRepo(real) {
		return nil
	}
	repos, total := childRepos(real)
	if why == "" {
		if total < clonesMin {
			return nil
		}
		why = fmt.Sprintf("a directory of %d git repositories, not inside one", total)
	}
	return &NotProjectError{Root: abs, Why: why, Repos: repos, Total: total}
}

// isHome reports whether dir is the user's home directory ($HOME on unix).
func isHome(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	return filepath.Clean(home) == dir
}

// insideRepo reports whether dir or an ancestor holds a .git (a directory,
// or the file a worktree or submodule has).
func insideRepo(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		up := filepath.Dir(dir)
		if up == dir {
			return false
		}
		dir = up
	}
}

// childRepos lists root's immediate subdirectories that hold a .git, sorted
// (hidden ones last), at most notProjectRepos, and how many there are.
func childRepos(root string) ([]string, int) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, 0
	}
	var repos []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, e.Name(), ".git")); err == nil {
			repos = append(repos, e.Name())
		}
	}
	// Hidden ones (~/.nvm, ~/.oh-my-zsh) last: they are tools' clones, and
	// the first name is the remedy's example.
	sort.Slice(repos, func(i, j int) bool {
		hi, hj := strings.HasPrefix(repos[i], "."), strings.HasPrefix(repos[j], ".")
		if hi != hj {
			return hj
		}
		return repos[i] < repos[j]
	})
	total := len(repos)
	if len(repos) > notProjectRepos {
		repos = repos[:notProjectRepos]
	}
	return repos, total
}
