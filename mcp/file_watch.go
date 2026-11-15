package mcp

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// fileWatchDebounce coalesces rapid events for the same path before a
// refresh fires. Editors save via write-storms and git operations touch
// many files at once; without debouncing every keystroke-save or branch
// switch would re-parse the same file several times.
const fileWatchDebounce = 200 * time.Millisecond

// kickFileWatch starts the workspace filesystem watcher in a goroutine.
// No-op when disabled or no root. It keeps the symbol index in sync with
// on-disk changes made OUTSIDE the tool's own edits — git checkout / mv,
// a pull, another editor — which headless MCP has no other way to
// observe. (Index.LookupExisting is the query-time backstop that prunes
// dead sites even if the watcher misses an event or never ran; this is
// the proactive layer that also picks up NEW and MODIFIED files.)
func (s *Server) kickFileWatch() {
	if !s.fileWatch || s.getRoot() == "" {
		return
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("mcp file-watch: new watcher: %v", err)
		return
	}
	s.watcherMu.Lock()
	s.watcher = w
	s.watcherMu.Unlock()

	root := s.getRoot()
	s.watcherMu.Lock()
	s.watchBudget = budgetFromHost(inotifyMaxPath)
	s.watcherMu.Unlock()
	added := s.addWatchDirs(w, root)
	log.Printf("mcp file-watch: watching %d dir(s) under %s", added, root)

	done := make(chan struct{})
	s.fileWatchDoneMu.Lock()
	s.fileWatchDone = done
	s.fileWatchDoneMu.Unlock()

	go func() {
		defer close(done)
		s.runFileWatch(w)
	}()
}

// stopFileWatch closes the watcher, which unblocks runFileWatch (its
// Events/Errors channels close). Idempotent and safe to call when the
// watcher never started.
func (s *Server) stopFileWatch() {
	s.watcherMu.Lock()
	w := s.watcher
	s.watcher = nil
	s.watcherMu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// WaitForFileWatch blocks until the watcher goroutine has exited (after
// stopFileWatch), or ctx is done. Returns nil if no watcher was started.
// Mirrors WaitForGitPrewarm; primarily for tests.
func (s *Server) WaitForFileWatch(ctx context.Context) error {
	s.fileWatchDoneMu.Lock()
	ch := s.fileWatchDone
	s.fileWatchDoneMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watchAdder is the part of *fsnotify.Watcher addWatchDirs needs; a test
// injects a fake to simulate ENOSPC.
type watchAdder interface {
	Add(name string) error
}

// Watch budget. A tsserver once held 30k of the 65,536 inotify watches,
// most under .claude/worktrees/*/node_modules; the watch set is shared
// by every process of the user, so this server takes at most a tenth of
// the limit and warns when one top-level dir takes more than 5%.
const (
	inotifyMaxPath       = "/proc/sys/fs/inotify/max_user_watches"
	inotifyMaxFallback   = 8192
	watchBudgetPercent   = 10
	watchPerDirWarnShare = 5 // percent of max
)

// readInotifyMax returns the host's max_user_watches, or ok=false when
// the file cannot be read or parsed.
func readInotifyMax(path string) (max int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// watchBudget counts the watches one server holds and refuses more once
// the limit is reached. Safe for concurrent use: the root walk and the
// event loop's dir-create handler both call addWatchDirs.
type watchBudget struct {
	mu         sync.Mutex
	max        int // host max_user_watches
	limit      int // watches this server may hold
	watched    map[string]bool
	perTop     map[string]int // watches per top-level dir; "." is the root itself
	warnedTop  map[string]bool
	full       bool
	fullWarned bool
}

// newWatchBudget makes a budget of watchBudgetPercent of max.
func newWatchBudget(max int) *watchBudget {
	limit := max * watchBudgetPercent / 100
	if limit < 1 {
		limit = 1
	}
	return &watchBudget{max: max, limit: limit, watched: map[string]bool{},
		perTop: map[string]int{}, warnedTop: map[string]bool{}}
}

// budgetFromHost builds the budget from the host limit, logging when the
// fallback is used.
func budgetFromHost(path string) *watchBudget {
	max, ok := readInotifyMax(path)
	if !ok {
		max = inotifyMaxFallback
		log.Printf("mcp file-watch: cannot read %s; assuming max_user_watches=%d", path, max)
	}
	return newWatchBudget(max)
}

// topDir names the top-level directory of path under root; "." for root.
func topDir(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return "."
	}
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		rel = rel[:i]
	}
	return rel
}

// forget drops a watched dir (deleted or renamed away) from the count,
// freeing its slot.
func (b *watchBudget) forget(root, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watched[path] {
		delete(b.watched, path)
		b.perTop[topDir(root, path)]--
		b.full = false
	}
}

// addWatchDirs registers root and every descendant directory (minus the
// noise dirs) with the watcher, within the watch budget. fsnotify is
// non-recursive, so each dir is added explicitly; newly created subtrees
// are added on the fly in handleWatchEvent. Returns the count of
// directories newly added. When the budget is reached, or Add returns
// ENOSPC, it stops and logs one warning listing the top-level dirs left
// unwatched.
func (s *Server) addWatchDirs(w watchAdder, root string) int {
	s.watcherMu.Lock()
	if s.watchBudget == nil {
		s.watchBudget = budgetFromHost(inotifyMaxPath)
	}
	b := s.watchBudget
	s.watcherMu.Unlock()
	wroot := s.getRoot()

	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	stopAt := ""
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != root && skipProactiveOpenDir(d.Name()) {
			return filepath.SkipDir
		}
		// Don't spend watch descriptors on trees the index excludes.
		if path != root && s.getIgnores().DirIgnored(wroot, path) {
			return filepath.SkipDir
		}
		if b.watched[path] {
			return nil
		}
		if b.full || len(b.watched) >= b.limit {
			b.full = true
			stopAt = path
			return filepath.SkipAll
		}
		if err := w.Add(path); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				log.Printf("mcp file-watch: inotify watch limit reached at %s (ENOSPC); not watching further", path)
				b.full, b.fullWarned = true, true
				stopAt = path
				return filepath.SkipAll
			}
			if !errors.Is(err, os.ErrNotExist) { // a dir deleted mid-walk is routine
				log.Printf("mcp file-watch: add %s: %v", path, err)
			}
			return nil
		}
		b.watched[path] = true
		count++
		top := topDir(wroot, path)
		b.perTop[top]++
		if n := b.perTop[top]; top != "." && !b.warnedTop[top] && n*100 > b.max*watchPerDirWarnShare {
			b.warnedTop[top] = true
			log.Printf("mcp file-watch: %s uses %d inotify watches (over %d%% of max_user_watches=%d)",
				top, n, watchPerDirWarnShare, b.max)
		}
		return nil
	})
	if stopAt != "" && !b.fullWarned {
		b.fullWarned = true
		log.Printf("mcp file-watch: watch budget of %d reached (%d%% of max_user_watches=%d); unwatched top-level dirs: %s",
			b.limit, watchBudgetPercent, b.max, strings.Join(s.unwatchedTops(b, wroot, stopAt), ", "))
	}
	return count
}

// unwatchedTops lists the top-level dirs of root holding no watch, plus
// the one the walk stopped in (partly watched). Caller holds b.mu.
func (s *Server) unwatchedTops(b *watchBudget, root, stopAt string) []string {
	var out []string
	stopTop := topDir(root, stopAt)
	entries, err := os.ReadDir(root)
	if err != nil {
		return []string{stopTop}
	}
	for _, e := range entries {
		if !e.IsDir() || skipProactiveOpenDir(e.Name()) ||
			s.getIgnores().DirIgnored(root, filepath.Join(root, e.Name())) {
			continue
		}
		switch {
		case e.Name() == stopTop:
			out = append(out, e.Name()+" (partial)")
		case b.perTop[e.Name()] == 0:
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		out = []string{stopTop}
	}
	return out
}

// runFileWatch is the event loop. Each file event schedules a debounced
// refresh; a fresh event for the same path resets the timer. Newly
// created directories are added to the watch set so their files stay
// covered. Returns when the watcher is closed.
func (s *Server) runFileWatch(w *fsnotify.Watcher) {
	var mu sync.Mutex
	pending := map[string]*time.Timer{}
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			s.handleWatchEvent(w, ev, pending, &mu)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("mcp file-watch: %v", err)
		}
	}
}

// handleWatchEvent routes one fsnotify event: directory creates extend
// the watch set, file events schedule a debounced index update. Removes
// and renames evict the (old) path; creates and writes re-extract it.
func (s *Server) handleWatchEvent(w *fsnotify.Watcher, ev fsnotify.Event, pending map[string]*time.Timer, mu *sync.Mutex) {
	// A removed or renamed dir loses its kernel watch; free its slot.
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		s.watcherMu.Lock()
		b := s.watchBudget
		s.watcherMu.Unlock()
		if b != nil {
			b.forget(s.getRoot(), ev.Name)
		}
	}
	// A newly created directory: extend coverage to it (and its
	// subtree). fsnotify won't recurse on its own.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if !skipProactiveOpenDir(filepath.Base(ev.Name)) {
				s.addWatchDirs(w, ev.Name)
			}
			return
		}
	}
	// A .gitignore change — most often a branch switch — invalidates the
	// filter the walk and this watcher both use. Re-resolve it, and do
	// so BEFORE the watchable check, since .gitignore is not itself an
	// indexed file.
	if filepath.Base(ev.Name) == ".gitignore" {
		if root := s.getRoot(); root != "" {
			s.setIgnores(s.loadIgnores(root))
		}
		return
	}
	if !s.watchedFile(ev.Name) {
		return
	}
	removed := ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0

	mu.Lock()
	if t, ok := pending[ev.Name]; ok {
		t.Stop()
	}
	name := ev.Name
	pending[name] = time.AfterFunc(fileWatchDebounce, func() {
		mu.Lock()
		delete(pending, name)
		mu.Unlock()
		if removed {
			s.removeFileFromIndex(name)
			s.notifyChildOfExternalDelete(name)
		} else {
			s.watchRefreshFile(name)
		}
	})
	mu.Unlock()
}

// watchableFile reports whether path routes to a registered language
// with a working extractor — the only files whose changes can affect
// the symbol index — AND is not gitignored.
//
// The ignore check has to be here, not only in the build walk: `Build`
// excludes an ignored file, and without this the very next write to it
// would put it straight back. A session that runs a build, or switches
// branches, would drift back toward indexing everything it was meant to
// skip.
func (s *Server) watchableFile(path string) bool {
	lang := s.languageForFile(path)
	if lang == "" || symbols.DefaultExtractor(lang) == nil {
		return false
	}
	if s.pathIgnored(path) {
		return false
	}
	// tsc rewriting foo.js beside foo.ts must not put the shadowed file
	// back. A fresh checker: the event may be the sibling's creation.
	_, shadowed := s.registry.NewShadowChecker().Shadowed(path)
	return !shadowed
}

// watchRefreshFile reads a changed file off disk and refreshes its slice
// of the index via the same path the tool's own edits use (so a watched
// change is indistinguishable from an in-tool one — comment refs and the
// parse cache included). A file that vanished or grew past the scan cap
// is treated as a removal.
func (s *Server) watchRefreshFile(path string) {
	// Belt to watchableFile's braces: this is the one place a WATCHED
	// change enters the index, so the ignore check lives here too. An
	// explicit tool edit still refreshes whatever it touched — that is a
	// deliberate act on a named file, not a stray write from a build.
	if s.pathIgnored(path) {
		return
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) > maxScanSize {
		s.removeFileFromIndex(path)
		s.notifyChildOfExternalDelete(path)
		return
	}
	s.refreshFileInIndex(path, content)
	// The index is only half the picture. Push the same bytes at the
	// child LSP, or its answers go stale the moment anything but this
	// tool writes the file — see notifyChildOfExternalChange.
	s.notifyChildOfExternalChange(path, content)
	// And tell the CLIENT when a merge conflict appears or clears. This is
	// the one file event an agent cannot afford to learn about late: from
	// the moment markers land, every symbol in the file may combine both
	// sides, and an edit can corrupt the merge.
	s.noteConflictTransition(path, content)
}

// watchedFile reports whether a path is worth watching at all: it feeds
// the symbol index, or it routes to a child LSP, or both. The two sets
// overlap heavily but neither contains the other — a language can have
// an extractor and no server, or a server and no extractor — and a
// change to either kind of file has somewhere to go.
func (s *Server) watchedFile(path string) bool {
	if s.watchableFile(path) {
		return true
	}
	if s.manager == nil || s.pathIgnored(path) {
		return false
	}
	return s.manager.Serves(pathToURI(path))
}

// removeFileFromIndex evicts every site for path from the index. Used
// when a watched file is deleted or renamed away.
func (s *Server) removeFileFromIndex(path string) {
	if idx := s.getIndex(); idx != nil {
		idx.RemoveFiles([]string{path})
	}
}
