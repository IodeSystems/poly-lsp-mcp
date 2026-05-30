package symbols

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
)

// PrewarmFromBranch walks every file in `branch` of the supplied
// repo, runs the matching language's extractor on each, and seeds
// the parse cache. Files whose extension doesn't route to a
// registered language or whose content size exceeds maxFileSize
// are silently skipped — same policy as Build.
//
// The cache is content-addressed by (language, sha256(content)), so
// when the user later switches to a branch that shares any of these
// files (by content, regardless of path) the parse is reused for
// free. Pre-filling the cache from ancestor branches makes that
// reuse work BEFORE the user visits them — the central Phase 3
// payoff for stacked-branch workflows.
//
// Returns the count of fresh parses (cache misses that became hits).
// Hits and skips don't contribute. Errors from individual file
// reads are logged-and-skipped; only repo-level errors (ls-tree
// failing) bubble up.
func PrewarmFromBranch(repo *git.Repo, branch string, reg *config.Registry, cache *ParseCache) (int, error) {
	if repo == nil || reg == nil {
		return 0, nil
	}
	paths, err := repo.ListFiles(branch)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", branch, err)
	}
	fresh := 0
	for _, path := range paths {
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if ext == "" {
			continue
		}
		lang := reg.LookupByExt(ext)
		if lang == nil {
			continue
		}
		ex := DefaultExtractor(lang.Name)
		if ex == nil {
			continue
		}
		content, err := repo.FileAt(branch, path)
		if err != nil {
			// Submodule pointers and other special entries can't be
			// shown as blobs; just skip them.
			continue
		}
		if len(content) > maxFileSize {
			continue
		}
		if _, hit := cache.Get(lang.Name, content); hit {
			continue
		}
		hits := ex.Extract(content)
		cache.Put(lang.Name, content, hits)
		fresh++
	}
	return fresh, nil
}
