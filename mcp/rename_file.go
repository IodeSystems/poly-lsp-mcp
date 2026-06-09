package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// node_rename_file moves a file and rewrites the import specifiers that point at
// it across the workspace — the workspace/willRenameFiles capability. A file-level
// LSP rename without this leaves every importer with a dangling path. TS/JS today
// (extensionless or extensioned relative specifiers, incl. dynamic import() and
// require); Go imports are package-based so a same-package move needs no edit;
// Python's dotted imports are not yet handled.

type nodeRenameFileArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// importSpecRe captures the quoted specifier in `… from '…'`, side-effect
// `import '…'`, dynamic `import('…')`, and `require('…')`.
var importSpecRe = regexp.MustCompile(`(?m)(?:\bfrom\s*|\bimport\s*\(\s*|\brequire\s*\(\s*|^\s*import\s+)['"]([^'"\n]+)['"]`)

// tsResolveExts are tried (in order) when a specifier omits the extension.
var tsResolveExts = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}

func handleNodeRenameFile(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p nodeRenameFileArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.From == "" || p.To == "" {
		return nil, true, errors.New("from and to are required")
	}
	root := s.getRoot()
	fromAbs := s.resolveFileArg(p.From)
	toAbs := s.resolveFileArg(p.To)

	content, err := os.ReadFile(fromAbs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", p.From, err)
	}
	if _, err := os.Stat(toAbs); err == nil {
		return nil, true, fmt.Errorf("target already exists: %s", p.To)
	}

	// Compute importer edits against the CURRENT layout (before the move).
	edits := importEditsForRename(root, fromAbs, toAbs)

	// Move the file.
	if err := os.MkdirAll(filepath.Dir(toAbs), 0o755); err != nil {
		return nil, true, fmt.Errorf("mkdir parent: %w", err)
	}
	if err := os.Rename(fromAbs, toAbs); err != nil {
		return nil, true, fmt.Errorf("move: %w", err)
	}
	s.refreshFileInIndex(fromAbs, []byte{}) // drop old slice
	s.refreshFileInIndex(toAbs, content)

	// Apply the importer edits.
	results := []map[string]any{}
	totalEdits := 0
	for absFile, fileEdits := range edits {
		n, err := applyFileEdits(absFile, fileEdits)
		if err != nil || n == 0 {
			continue
		}
		if data, rerr := os.ReadFile(absFile); rerr == nil {
			s.refreshFileInIndex(absFile, data)
		}
		rel, _ := filepath.Rel(root, absFile)
		results = append(results, map[string]any{"file": filepath.ToSlash(rel), "edits": n})
		totalEdits += n
	}

	return jsonContent(map[string]any{
		"from":             p.From,
		"to":               p.To,
		"importersUpdated": len(results),
		"importEdits":      totalEdits,
		"results":          results,
	}), false, nil
}

// importEditsForRename scans the workspace's TS/JS files for relative import
// specifiers that resolve to fromAbs and produces edits rewriting them to the new
// relative path to toAbs (preserving whether the original carried an extension).
func importEditsForRename(root, fromAbs, toAbs string) map[string][]resolvedEdit {
	out := map[string][]resolvedEdit{}
	fromExt := filepath.Ext(fromAbs)
	fromNoExt := strings.TrimSuffix(fromAbs, fromExt)
	toExt := filepath.Ext(toAbs)
	toNoExt := strings.TrimSuffix(toAbs, toExt)

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == ".git" || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if path == fromAbs || path == toAbs {
			return nil
		}
		if !isImportLang(path) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		base := filepath.Dir(path)
		for _, m := range importSpecRe.FindAllSubmatchIndex(data, -1) {
			specStart, specEnd := m[2], m[3] // capture group 1
			spec := string(data[specStart:specEnd])
			if !strings.HasPrefix(spec, ".") {
				continue // only relative specifiers
			}
			resolved := filepath.Clean(filepath.Join(base, spec))
			if resolved != fromNoExt && resolved != fromAbs {
				continue
			}
			hadExt := resolved == fromAbs // spec carried the extension
			target := toNoExt
			if hadExt {
				target = toAbs
			}
			rel, rerr := filepath.Rel(base, target)
			if rerr != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, ".") {
				rel = "./" + rel
			}
			line, col := offsetToLineCol(data, specStart)
			out[path] = append(out[path], resolvedEdit{
				AbsFile: path, Line: line, Col: col, OldText: spec, NewText: rel,
			})
		}
		return nil
	})
	return out
}

func isImportLang(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		return true
	}
	return false
}

// offsetToLineCol converts a 0-based byte offset to a 1-based line and 1-based byte
// column (matching applyFileEdits' lineColToByteOffset convention).
func offsetToLineCol(content []byte, off int) (int, int) {
	line, lineStart := 1, 0
	for i := 0; i < off && i < len(content); i++ {
		if content[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	return line, off - lineStart + 1
}
