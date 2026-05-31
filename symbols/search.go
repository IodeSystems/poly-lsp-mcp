package symbols

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SearchHit is one regex match inside a workspace file. Position is
// 1-based — Line counts from 1, Col is the byte offset (not rune
// offset) of the first byte of the match within Line. MatchEndCol is
// the byte offset one past the match. Text is the entire line
// containing the match (without trailing newline), letting the
// caller render context without re-reading the file.
type SearchHit struct {
	File        string
	Line        int
	Col         int
	MatchEndCol int
	Text        string

	// Before / After are the N lines preceding and following the
	// matched line, when the caller asked for context. Empty
	// otherwise.
	Before []string
	After  []string
}

// SearchOptions tunes Search's behavior. Zero values mean "use
// defaults" — Limit=0 means unbounded, ContextLines=0 means no
// context lines.
type SearchOptions struct {
	// Glob is a filepath.Match pattern matched against each file's
	// basename. Empty matches every file. Supports `?`, `*`,
	// character classes — no `**` recursion (which is implicit
	// from the workspace walk anyway).
	Glob string
	// Limit caps the total number of hits returned. Excess hits
	// are counted in the dropped return value but not emitted.
	// Zero means unbounded; pass a positive integer for any real
	// workload.
	Limit int
	// ContextLines is the number of lines before AND after each
	// matched line to include. Useful for previewing without a
	// follow-up node_read.
	ContextLines int
}

// Search walks `root` recursively and returns every regex hit in
// file contents. Skips the same noise directories as Build
// (.git / node_modules / vendor / __pycache__ / etc.) and the same
// per-file size cap (1 MiB; bigger files are silently dropped to
// keep memory bounded on huge generated blobs).
//
// Returns the matching hits (sorted by file then line then column),
// the count of hits dropped past Limit (zero when Limit is zero or
// unreached), and any I/O error from the walk itself. Per-file read
// errors are silently skipped so one unreadable file doesn't abort
// the search.
//
// pattern is required. Pass `(?i)…` for case-insensitive matching
// (Go's regexp syntax supports inline flags natively — no separate
// option needed).
func Search(root string, pattern *regexp.Regexp, opts SearchOptions) ([]SearchHit, int, error) {
	if pattern == nil {
		return nil, 0, fmt.Errorf("search: pattern is required")
	}
	var hits []SearchHit
	dropped := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if opts.Glob != "" {
			match, err := filepath.Match(opts.Glob, d.Name())
			if err != nil {
				return fmt.Errorf("invalid glob %q: %w", opts.Glob, err)
			}
			if !match {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Quick heuristic: skip files that look binary. Real text
		// files don't have null bytes in the first 8 KB.
		probe := content
		if len(probe) > 8192 {
			probe = probe[:8192]
		}
		if bytes.IndexByte(probe, 0) >= 0 {
			return nil
		}

		fileHits := searchFile(path, content, pattern, opts.ContextLines)
		for _, h := range fileHits {
			if opts.Limit > 0 && len(hits) >= opts.Limit {
				dropped++
				continue
			}
			hits = append(hits, h)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Col < hits[j].Col
	})
	return hits, dropped, nil
}

// searchFile scans content line by line and produces one SearchHit
// per match. Lines are split on \n; trailing \r stripped from each
// line so Text matches the editor view of CRLF files.
func searchFile(path string, content []byte, pattern *regexp.Regexp, contextLines int) []SearchHit {
	lines := strings.Split(string(content), "\n")
	// Drop trailing empty entry from a final \n so line numbers
	// match editor reckoning.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}

	var out []SearchHit
	for lineIdx, line := range lines {
		spans := pattern.FindAllStringIndex(line, -1)
		if len(spans) == 0 {
			continue
		}
		for _, span := range spans {
			out = append(out, SearchHit{
				File:        path,
				Line:        lineIdx + 1,
				Col:         span[0] + 1,
				MatchEndCol: span[1] + 1,
				Text:        line,
				Before:      contextBefore(lines, lineIdx, contextLines),
				After:       contextAfter(lines, lineIdx, contextLines),
			})
		}
	}
	return out
}

func contextBefore(lines []string, idx, n int) []string {
	if n <= 0 || idx == 0 {
		return nil
	}
	start := idx - n
	if start < 0 {
		start = 0
	}
	out := make([]string, idx-start)
	copy(out, lines[start:idx])
	return out
}

func contextAfter(lines []string, idx, n int) []string {
	if n <= 0 {
		return nil
	}
	end := idx + 1 + n
	if end > len(lines) {
		end = len(lines)
	}
	if idx+1 >= end {
		return nil
	}
	out := make([]string, end-(idx+1))
	copy(out, lines[idx+1:end])
	return out
}
