package mcp

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
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

// addWatchDirs registers root and every descendant directory (minus the
// noise dirs) with the watcher. fsnotify is non-recursive, so each dir
// is added explicitly; newly created subtrees are added on the fly in
// handleWatchEvent. Returns the count of directories added.
func (s *Server) addWatchDirs(w *fsnotify.Watcher, root string) int {
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != root && skipProactiveOpenDir(d.Name()) {
			return filepath.SkipDir
		}
		if err := w.Add(path); err == nil {
			count++
		}
		return nil
	})
	return count
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
	if !s.watchableFile(ev.Name) {
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
		} else {
			s.watchRefreshFile(name)
		}
	})
	mu.Unlock()
}

// watchableFile reports whether path routes to a registered language
// with a working extractor — the only files whose changes can affect
// the symbol index.
func (s *Server) watchableFile(path string) bool {
	lang := s.languageForFile(path)
	return lang != "" && symbols.DefaultExtractor(lang) != nil
}

// watchRefreshFile reads a changed file off disk and refreshes its slice
// of the index via the same path the tool's own edits use (so a watched
// change is indistinguishable from an in-tool one — comment refs and the
// parse cache included). A file that vanished or grew past the scan cap
// is treated as a removal.
func (s *Server) watchRefreshFile(path string) {
	content, err := os.ReadFile(path)
	if err != nil || len(content) > maxScanSize {
		s.removeFileFromIndex(path)
		return
	}
	s.refreshFileInIndex(path, content)
}

// removeFileFromIndex evicts every site for path from the index. Used
// when a watched file is deleted or renamed away.
func (s *Server) removeFileFromIndex(path string) {
	if idx := s.getIndex(); idx != nil {
		idx.RemoveFiles([]string{path})
	}
}
