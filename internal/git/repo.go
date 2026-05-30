// Package git wraps the git binary for the bits poly-lsp-mcp needs to
// prewarm its parse cache from ancestor branches. We deliberately
// shell out instead of binding a Go git library — the operations we
// need (rev-parse, ls-tree, show) are stable porcelain enough to
// stay forward-compatible, and a real git binary handles repo
// quirks (worktrees, sparse-checkouts, submodule edges, .gitignore)
// without us re-implementing any of it.
//
// Every operation returns an error wrapped from os/exec so callers
// can branch on "git not on PATH" / "not a repo" / "branch missing"
// distinctly via errors.Is.
package git

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotInRepo is returned by FromCWD when the supplied working
// directory isn't inside any git repository.
var ErrNotInRepo = errors.New("git: not in a repository")

// ErrGitMissing is returned when the git binary can't be found on
// PATH. Callers typically skip prewarm in this case.
var ErrGitMissing = errors.New("git: binary not on PATH")

// Repo wraps git access for one working tree. Dir is the top level
// (`git rev-parse --show-toplevel`) — distinct from the .git
// directory itself (which Repo doesn't need to know about; git
// figures that out from cwd).
type Repo struct {
	Dir string
}

// FromCWD locates the working tree containing cwd. Returns
// ErrNotInRepo when cwd is outside a git checkout, ErrGitMissing
// when the binary is unavailable.
func FromCWD(cwd string) (*Repo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, ErrGitMissing
	}
	out, err := runIn(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		// `git` will exit nonzero with "not a git repository" on the
		// stderr. We only care that it failed at all — the exact
		// reason isn't load-bearing for callers.
		return nil, fmt.Errorf("%w (cwd=%s): %v", ErrNotInRepo, cwd, err)
	}
	return &Repo{Dir: strings.TrimSpace(out)}, nil
}

// CurrentBranch returns the active branch name. Empty string + nil
// error indicates detached HEAD — callers without a current branch
// to anchor on should skip prewarm.
func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	name := strings.TrimSpace(out)
	if name == "HEAD" {
		return "", nil // detached
	}
	return name, nil
}

// UpstreamBranch returns the upstream of `branch` as a local-form
// ref (strip the remote prefix from `origin/main` → `main`). Empty
// string + nil error means no upstream is configured for that
// branch.
//
// We strip the remote because the prewarm walker needs to read
// files from a ref `git show` can resolve; local refs are guaranteed
// to be fast and don't require a fetch. Stacked branches typically
// track other LOCAL branches anyway (e.g., `feature/b` tracks
// `feature/a`).
func (r *Repo) UpstreamBranch(branch string) (string, error) {
	out, err := r.run("rev-parse", "--abbrev-ref", branch+"@{upstream}")
	if err != nil {
		// `git` exits non-zero when no upstream is configured.
		// Treat that as "no upstream" rather than a hard error so
		// callers can use this to discover that the chain has
		// terminated.
		return "", nil
	}
	upstream := strings.TrimSpace(out)
	if upstream == "" || upstream == branch {
		return "", nil
	}
	// Strip remote prefix: `origin/main` → `main`. We only do this
	// when there's a corresponding local branch by the same name,
	// otherwise return the original ref.
	if slash := strings.IndexByte(upstream, '/'); slash >= 0 {
		local := upstream[slash+1:]
		if _, err := r.run("rev-parse", "--verify", "--quiet", "refs/heads/"+local); err == nil {
			return local, nil
		}
	}
	return upstream, nil
}

// AncestorChain returns the chain of ancestor branches reachable
// from `branch` by following each branch's upstream. Returns at
// most maxDepth entries (excluding `branch` itself). Cycles are
// guarded — if the chain revisits a branch it stops there.
//
// Example: in a feature/c → feature/b → feature/a → main stack,
// AncestorChain("feature/c", 10) returns
// ["feature/b", "feature/a", "main"].
func (r *Repo) AncestorChain(branch string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{branch: true}
	current := branch
	for i := 0; i < maxDepth; i++ {
		up, err := r.UpstreamBranch(current)
		if err != nil {
			return out, err
		}
		if up == "" || seen[up] {
			break
		}
		out = append(out, up)
		seen[up] = true
		current = up
	}
	return out, nil
}

// ListFiles returns the paths of every blob in `branch`, relative
// to the repository root. Uses git ls-tree -r which respects the
// tree's actual contents (so .gitignore-d files and untracked files
// don't appear).
func (r *Repo) ListFiles(branch string) ([]string, error) {
	out, err := r.run("ls-tree", "-r", "--name-only", branch)
	if err != nil {
		return nil, fmt.Errorf("ls-tree %s: %w", branch, err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	return paths, nil
}

// FileAt returns the contents of `path` (repo-relative) as seen on
// `branch`. Empty content + nil error when the file exists but is
// empty. A nonexistent path returns an error wrapping the git exit
// status.
func (r *Repo) FileAt(branch, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", branch+":"+path)
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("show %s:%s: %v (%s)", branch, path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r *Repo) run(args ...string) (string, error) {
	return runIn(r.Dir, args...)
}

func runIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
