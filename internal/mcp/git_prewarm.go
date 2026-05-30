package mcp

import (
	"errors"
	"log"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/internal/symbols"
)

// ancestorChainMaxDepth caps how far up the upstream chain we walk
// when prewarming. A stack of 8 ancestor branches would already be
// unusual; 16 is plenty of headroom without risk of runaway loops
// in weird upstream-cycle scenarios (UpstreamBranch also
// self-protects via the seen-set, but this is belt + suspenders).
const ancestorChainMaxDepth = 16

// kickGitPrewarm starts the ancestor-branch parse-cache prewarm in
// a goroutine. Safe to call when gitPrewarm is disabled, when the
// workspace isn't in a git repo, or when the git binary is missing
// — each of these cases is a silent no-op.
//
// The prewarm seeds s.parseCache with parses keyed on
// (language, sha256(content)) for every file in every ancestor
// branch's tree. When the user later checks out one of those
// branches, files whose bytes are unchanged hit the cache and skip
// re-parsing. See Phase 3 in plan.md for the design.
func (s *Server) kickGitPrewarm() {
	if !s.gitPrewarm {
		return
	}
	if s.getRoot() == "" {
		return
	}

	done := make(chan struct{})
	s.gitPrewarmDoneMu.Lock()
	s.gitPrewarmDone = done
	s.gitPrewarmDoneMu.Unlock()

	go func() {
		defer close(done)
		s.runGitPrewarm()
	}()
}

// runGitPrewarm performs the actual walk. Synchronous; called from
// the goroutine kickGitPrewarm starts.
func (s *Server) runGitPrewarm() {
	root := s.getRoot()
	repo, err := git.FromCWD(root)
	if err != nil {
		// ErrNotInRepo / ErrGitMissing are the expected non-errors
		// here. Log the unexpected ones but don't bother yelling
		// about "not in a repo" — that's a configuration choice.
		if !errors.Is(err, git.ErrNotInRepo) && !errors.Is(err, git.ErrGitMissing) {
			log.Printf("mcp git-prewarm: open repo: %v", err)
		}
		return
	}
	currentBranch, err := repo.CurrentBranch()
	if err != nil {
		log.Printf("mcp git-prewarm: current branch: %v", err)
		return
	}
	if currentBranch == "" {
		// Detached HEAD — no upstream chain to walk.
		return
	}
	chain, err := repo.AncestorChain(currentBranch, ancestorChainMaxDepth)
	if err != nil {
		log.Printf("mcp git-prewarm: ancestor chain: %v", err)
		return
	}
	if len(chain) == 0 {
		return
	}

	start := time.Now()
	total := 0
	for _, branch := range chain {
		fresh, err := symbols.PrewarmFromBranch(repo, branch, s.registry, s.parseCache)
		if err != nil {
			log.Printf("mcp git-prewarm: %s: %v", branch, err)
			continue
		}
		log.Printf("mcp git-prewarm: %s → %d fresh parse(s)", branch, fresh)
		total += fresh
	}
	log.Printf("mcp git-prewarm: %d branch(es), %d fresh parse(s) total in %s",
		len(chain), total, time.Since(start))
}
