package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// goplsRenameEdits attempts a TYPE-SCOPED rename via the child language
// server (gopls et al.) at the addressed identifier — the correct fix for
// the lexical-rename collision bug (renaming payments.Gateway.IsLive must
// NOT touch the unrelated llm.Rewriter.IsLive). The server resolves the
// symbol by its declaring type and returns edits for exactly that symbol's
// declaration, implementations, and usages.
//
// Returns:
//   - tried=false when no child LSP serves the file (a tree-sitter-only
//     language): the caller falls back to the lexical path + its guard.
//   - tried=true, err!=nil when a child exists but refused/failed: the
//     caller surfaces the error and does NOT fall back — lexical is the
//     unsafe path this replaces, so silently degrading to it would
//     reintroduce the very corruption we're preventing.
//   - tried=true, edits when the server returned a WorkspaceEdit, converted
//     to byte-ranged resolvedEdits ready for the normal apply/txn pipeline.
func (s *Server) goplsRenameEdits(a rangeArgs, newName string) (edits []resolvedEdit, tried bool, err error) {
	if s.manager == nil {
		return nil, false, nil
	}
	abs, ferr := s.resolveFileArg(a.File)
	if ferr != nil {
		return nil, false, nil
	}
	uri := pathToURI(abs)
	child := s.manager.RouteByURI(uri)
	if child == nil {
		return nil, false, nil // tree-sitter-only language: no server to ask
	}
	content, rerr := os.ReadFile(abs)
	if rerr != nil {
		return nil, false, nil
	}
	// gopls answers about files it's been told of; didOpen is idempotent.
	s.notifyChildOfOpen(child, uri, content)

	ctx, cancel := context.WithTimeout(context.Background(), lspResolveTimeout)
	defer cancel()
	// LSP positions are 0-based; ours are 1-based. `character` is
	// counted in UTF-16 code units, NOT bytes — sending a byte column
	// points gopls at the wrong offset on any line with a non-ASCII
	// character before the identifier.
	utf16Col, ok := byteOffsetToUTF16Col(content, a.StartLine, a.StartCol)
	if !ok {
		return nil, false, nil
	}
	raw, cerr := child.Call(ctx, "textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": a.StartLine - 1, "character": utf16Col - 1},
		"newName":      newName,
	})
	if cerr != nil {
		return nil, true, fmt.Errorf("the language server declined the rename: %v", cerr)
	}
	edits, perr := s.workspaceEditToResolved(raw, newName)
	if perr != nil {
		return nil, true, perr
	}
	if len(edits) == 0 {
		return nil, true, fmt.Errorf(
			"the language server returned no edits renaming at %s:%d:%d — it may not consider that position a renameable symbol",
			a.File, a.StartLine, a.StartCol)
	}
	return edits, true, nil
}

// lspPos is an LSP 0-based position. lspTextEdit / lspWorkspaceEdit mirror
// the subset of the protocol gopls returns for a rename.
type lspPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspTextEdit struct {
	Range struct {
		Start lspPos `json:"start"`
		End   lspPos `json:"end"`
	} `json:"range"`
	NewText string `json:"newText"`
}

type lspWorkspaceEdit struct {
	Changes         map[string][]lspTextEdit `json:"changes"`
	DocumentChanges []struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Edits []lspTextEdit `json:"edits"`
	} `json:"documentChanges"`
}

// workspaceEditToResolved converts a server WorkspaceEdit into the byte-
// offset resolvedEdit shape the apply pipeline uses. It handles both the
// `changes` map and the newer `documentChanges` array (gopls emits the
// latter). Each edit's OldText is read from the file at its range so the
// downstream apply's text-match guard passes. newName is unused here (the
// edit carries its own NewText) but kept for symmetry / future validation.
func (s *Server) workspaceEditToResolved(raw json.RawMessage, _ string) ([]resolvedEdit, error) {
	var we lspWorkspaceEdit
	if err := json.Unmarshal(raw, &we); err != nil {
		return nil, fmt.Errorf("could not parse the language server's rename result: %v", err)
	}
	byURI := map[string][]lspTextEdit{}
	for uri, edits := range we.Changes {
		byURI[uri] = append(byURI[uri], edits...)
	}
	for _, dc := range we.DocumentChanges {
		byURI[dc.TextDocument.URI] = append(byURI[dc.TextDocument.URI], dc.Edits...)
	}

	root := s.getRoot()
	var out []resolvedEdit
	for uri, edits := range byURI {
		abs := uriToPath(uri)
		if abs == "" {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		rel := relPath(abs, root)
		for _, e := range edits {
			// The server's `character` is a UTF-16 code-unit count.
			// Reading it as bytes produces ranges that slice
			// mid-character and corrupt the file being renamed.
			startOff, ok1 := utf16ColToByteOffset(content, e.Range.Start.Line+1, e.Range.Start.Character+1)
			endOff, ok2 := utf16ColToByteOffset(content, e.Range.End.Line+1, e.Range.End.Character+1)
			if !ok1 || !ok2 || startOff > endOff || endOff > len(content) {
				continue
			}
			// Col is reported to the caller in OUR convention: 1-based
			// bytes within the line.
			byteCol := startOff
			if ls, ok := lineStartOffset(content, e.Range.Start.Line+1); ok {
				byteCol = startOff - ls + 1
			}
			out = append(out, resolvedEdit{
				AbsFile: abs,
				RelFile: rel,
				Line:    e.Range.Start.Line + 1,
				Col:     byteCol,
				OldText: string(content[startOff:endOff]),
				NewText: e.NewText,
			})
		}
	}
	return out, nil
}
