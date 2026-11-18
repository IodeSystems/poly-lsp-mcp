package mcp

import (
	"crypto/sha256"
	"os"
	"sync"
	"time"
)

// gitignoreReloadDelay coalesces a burst of .gitignore events (a branch
// switch, an editor's save storm) into one `git ls-files` run.
const gitignoreReloadDelay = 500 * time.Millisecond

// gitignoreMtimeSlack is how much older than the ignore set's load time
// an unseen .gitignore must be before it counts as already reflected.
// It covers file systems with 1s or 2s mtime resolution.
const gitignoreMtimeSlack = 2 * time.Second

// gitignoreReload debounces the re-resolve of the ignore set and skips
// it when no .gitignore content changed. A skipped re-resolve also keeps
// the set's `git check-ignore` memo.
type gitignoreReload struct {
	mu      sync.Mutex
	timer   *time.Timer
	pending map[string]bool
	// seen is the fingerprint of each .gitignore the current set reflects.
	seen map[string]string

	flushMu sync.Mutex // one flush at a time
}

// queueGitignoreReload records an event on a .gitignore and starts the
// debounce timer when none is running.
func (s *Server) queueGitignoreReload(path string) {
	r := &s.ignoreReload
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]bool{}
	}
	r.pending[path] = true
	if r.timer == nil {
		r.timer = time.AfterFunc(gitignoreReloadDelay, s.flushGitignoreReload)
	}
}

// stop cancels a pending re-resolve.
func (r *gitignoreReload) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.pending = nil
}

// flushGitignoreReload re-resolves the ignore set once if any queued
// .gitignore changed since the set was loaded.
func (s *Server) flushGitignoreReload() {
	r := &s.ignoreReload
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	paths := r.pending
	r.pending, r.timer = nil, nil
	r.mu.Unlock()
	root := s.getRoot()
	if root == "" || len(paths) == 0 {
		return
	}
	loaded := s.getIgnores().LoadedAt()
	changed := false
	fps := make(map[string]string, len(paths))
	r.mu.Lock()
	for p := range paths {
		fp, mtime, exists := gitignoreFingerprint(p)
		fps[p] = fp
		old, ok := r.seen[p]
		switch {
		case ok:
			changed = changed || old != fp
		case exists && !loaded.IsZero() && mtime.Before(loaded.Add(-gitignoreMtimeSlack)):
			// First event for this file, and it predates the set: the
			// set already reflects this content.
		default:
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		s.setIgnores(s.loadIgnores(root))
	}
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]string{}
	}
	for p, fp := range fps {
		r.seen[p] = fp
	}
	r.mu.Unlock()
}

// gitignoreFingerprint hashes a .gitignore's content; "" when it cannot
// be read (deleted).
func gitignoreFingerprint(path string) (fp string, mtime time.Time, exists bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", info.ModTime(), true
	}
	sum := sha256.Sum256(b)
	return string(sum[:]), info.ModTime(), true
}
