package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// This server is the file watcher of its child LSPs: a child that
// registers workspace/didChangeWatchedFiles watchers
// (multiplex/watched_files.go) gets its events from the fsnotify watcher
// in file_watch.go, and creates no inotify watches of its own. What that
// watcher does not watch — skipped dirs (node_modules, .claude, ...),
// gitignored dirs, anything outside the root — no child hears about.
// docs/design.md has the measurements.

// childEventBatch collects file events for the children over one
// fileWatchDebounce window, then sends them as one batch: a checkout or
// an install touches many files at once, and a child should get one
// didChangeWatchedFiles for it, not one per file.
type childEventBatch struct {
	mu    sync.Mutex
	order []string
	types map[string]multiplex.FileChangeType
}

// add merges one event into the batch and reports whether the batch was
// empty (the caller then starts the flush timer).
func (b *childEventBatch) add(ev multiplex.FileEvent) (first bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.types == nil {
		b.types = map[string]multiplex.FileChangeType{}
	}
	first = len(b.order) == 0
	old, seen := b.types[ev.Path]
	if !seen {
		b.order = append(b.order, ev.Path)
		b.types[ev.Path] = ev.Type
		return first
	}
	b.types[ev.Path] = coalesceChange(old, ev.Type)
	return first
}

// take empties the batch and returns its events in arrival order.
func (b *childEventBatch) take() []multiplex.FileEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]multiplex.FileEvent, 0, len(b.order))
	for _, p := range b.order {
		out = append(out, multiplex.FileEvent{Path: p, Type: b.types[p]})
	}
	b.order, b.types = nil, nil
	return out
}

// coalesceChange is the one event that stands for old followed by next
// on the same path within a batch. A file created and then written is
// still new to the child; one deleted and created again has changed.
func coalesceChange(old, next multiplex.FileChangeType) multiplex.FileChangeType {
	switch {
	case old == multiplex.FileCreated && next == multiplex.FileChanged:
		return multiplex.FileCreated
	case old == multiplex.FileDeleted && next == multiplex.FileCreated:
		return multiplex.FileChanged
	}
	return next
}

// childFileEvent turns one fsnotify event into the event a child may
// get, or false when no child should hear of it: a chmod, a path
// outside the root, or a path inside a skipped or gitignored dir. A
// skipped dir's own create or delete is kept: tsserver needs to see
// node_modules appear (see manifestDirs).
func (s *Server) childFileEvent(ev fsnotify.Event) (multiplex.FileEvent, bool) {
	var t multiplex.FileChangeType
	switch {
	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		t = multiplex.FileDeleted
	case ev.Op&fsnotify.Create != 0:
		t = multiplex.FileCreated
	case ev.Op&fsnotify.Write != 0:
		t = multiplex.FileChanged
	default:
		return multiplex.FileEvent{}, false
	}
	root := s.getRoot()
	rel, err := filepath.Rel(root, ev.Name)
	if root == "" || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return multiplex.FileEvent{}, false
	}
	segs := strings.Split(rel, string(filepath.Separator))
	for _, seg := range segs[:len(segs)-1] {
		if skipProactiveOpenDir(seg) {
			return multiplex.FileEvent{}, false
		}
	}
	if parent := filepath.Dir(ev.Name); parent != root && s.getIgnores().DirIgnored(root, parent) {
		return multiplex.FileEvent{}, false
	}
	leafDir := false
	if t != multiplex.FileDeleted {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			leafDir = true
		}
	}
	if !leafDir && s.pathIgnored(ev.Name) {
		return multiplex.FileEvent{}, false
	}
	return multiplex.FileEvent{Path: ev.Name, Type: t}, true
}

// packageManifests are the files a package install rewrites beside the
// node_modules it fills.
var packageManifests = map[string]bool{
	"package.json":      true,
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
}

// withNodeModulesEvents adds a Changed event for <dir>/node_modules when
// a package manifest in dir changed and that node_modules exists.
//
// node_modules is not watched, so an install's writes inside it reach no
// child. The manifest events alone are not enough: measured with
// typescript-language-server 6.1.0 and TypeScript 6.0.2, tsserver
// re-resolved a newly installed package after package.json and
// package-lock.json events only when node_modules did not exist before
// the install. One Changed event for node_modules made it re-resolve in
// both cases (docs/design.md).
func withNodeModulesEvents(events []multiplex.FileEvent) []multiplex.FileEvent {
	have := map[string]bool{}
	for _, ev := range events {
		have[ev.Path] = true
	}
	out := events
	for _, ev := range events {
		if !packageManifests[filepath.Base(ev.Path)] {
			continue
		}
		nm := filepath.Join(filepath.Dir(ev.Path), "node_modules")
		if have[nm] {
			continue
		}
		if info, err := os.Stat(nm); err != nil || !info.IsDir() {
			continue
		}
		have[nm] = true
		out = append(out, multiplex.FileEvent{Path: nm, Type: multiplex.FileChanged})
	}
	return out
}

// dropEchoes removes Changed events for open documents whose bytes the
// child already has: the watcher sees the tool's own writes too, ~200ms
// behind each edit, and the child has had that text since the edit. The
// same guard as notifyChildOfExternalChange.
func (s *Server) dropEchoes(events []multiplex.FileEvent) []multiplex.FileEvent {
	out := events[:0]
	for _, ev := range events {
		if ev.Type == multiplex.FileChanged {
			uri := pathToURI(ev.Path)
			s.openDocsMu.Lock()
			_, sent := s.sentDocs[uri]
			s.openDocsMu.Unlock()
			if sent {
				if content, err := os.ReadFile(ev.Path); err == nil && s.sentContentMatches(uri, content) {
					continue
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

// queueChildFileEvent adds one fsnotify event to the children's batch,
// starting the flush timer when the batch was empty. No-op without a
// manager.
func (s *Server) queueChildFileEvent(ev fsnotify.Event) {
	if s.manager == nil {
		return
	}
	fe, ok := s.childFileEvent(ev)
	if !ok {
		return
	}
	if s.childEvents.add(fe) {
		time.AfterFunc(fileWatchDebounce, s.flushChildFileEvents)
	}
}

// flushChildFileEvents sends the batch to every child whose watchers
// match.
func (s *Server) flushChildFileEvents() {
	events := s.dropEchoes(withNodeModulesEvents(s.childEvents.take()))
	if len(events) == 0 || s.manager == nil {
		return
	}
	s.manager.NotifyFileEvents(events)
}
