package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
)

// Format on edit: an edit that is KEPT (validation accepted it, or
// validation is off or could not run) is formatted by the language's
// child LSP, when format_on_edit is on for the language and the client.
//
//   - textDocument/rangeFormatting over the lines the edit changed, when
//     the server declares documentRangeFormattingProvider;
//   - else textDocument/formatting of the whole file, when it declares
//     documentFormattingProvider;
//   - skipped (formatSkipped) when the language's child LSP is off for
//     the client (lsp-off), no child serves the file (no-lsp), or the
//     server declares neither (no-capability).
//
// A rejected (reverted) edit is never formatted. Formatting never fails
// or reverts the edit: an error or a timeout (bounded by --validate-wait)
// keeps the accepted, unformatted text and reports formatSkipped.
//
// The format's TextEdits are not a new edit for validation: they are not
// judged against the error baseline. The child is told the formatted
// text (didChange) and, for a single edit, the diagnostics are collected
// again so their positions are the formatted file's; that collection is
// reported, not judged. A formatter is trusted not to add errors. See
// docs/design.md.
//
// Off for a language and client: nothing is done and no field reported.

// Formatting skip reasons (formatSkipped).
const (
	fmtSkipLSPOff        = "lsp-off"         // the client turned the language's child off
	fmtSkipNoLSP         = "no-lsp"          // no child LSP serves the file
	fmtSkipNoCapability  = "no-capability"   // the server declares no (range) formatting
	fmtSkipTimeout       = "timeout"         // no answer within --validate-wait
	fmtSkipError         = "error"           // the server failed, or its edits did not apply
	fmtSkipChangedOnDisk = "changed-on-disk" // the file changed between the edit and the format
)

// formatOutcome is what formatting one kept file did.
type formatOutcome struct {
	Mode    string // "range" | "file" when the server was asked and its edits applied
	Skipped string // a fmtSkip* reason when on but not done
	Note    string // detail for an error or a timeout
	Changed bool   // the edits changed the text
	// edits are the applied edits as byte ranges of before, the text
	// before the format, sorted, non-overlapping; for mapping positions
	// through (mapLine).
	edits  []byteEdit
	before []byte
}

// on reports whether formatting was on (asked or skipped) for the file.
func (f formatOutcome) on() bool { return f.Mode != "" || f.Skipped != "" }

// byteEdit is a TextEdit as a byte range [start, end) and its text.
type byteEdit struct {
	start, end int
	text       string
}

// formatOnEditFor reports whether a kept edit to abs is formatted for the
// current session.
func (s *Server) formatOnEditFor(abs string) bool {
	if s.registry == nil {
		return false
	}
	lang := s.langOfPath(abs)
	if lang == "" {
		return false
	}
	s.sessionWaitsMu.Lock()
	p := s.sessionLSP[s.activeSession]
	s.sessionWaitsMu.Unlock()
	return s.registry.FormatOnEdit(lang, p.FormatOn, p.FormatOff)
}

// formatKept formats abs, whose kept text is cur (orig: before the edit,
// nil for a create), and writes the result. It returns the file's text
// after it: cur unless the format changed it. Call only when
// formatOnEditFor(abs).
func (s *Server) formatKept(abs string, orig, cur []byte) ([]byte, formatOutcome) {
	uri := pathToURI(abs)
	if s.manager == nil {
		return cur, formatOutcome{Skipped: fmtSkipNoLSP}
	}
	if s.lspOffURI(s.lspOff(s.activeSession), uri) {
		return cur, formatOutcome{Skipped: fmtSkipLSPOff}
	}
	child := s.manager.RouteByURI(uri)
	if child == nil {
		return cur, formatOutcome{Skipped: fmtSkipNoLSP}
	}
	rangeOK, fileOK := formattingCaps(s.manager.Capabilities()[s.langOfPath(abs)])
	if !rangeOK && !fileOK {
		return cur, formatOutcome{Skipped: fmtSkipNoCapability}
	}
	// The child formats the text it holds: make that the kept text (a
	// staged batch without validation never sent it).
	if !s.sentContentMatches(uri, cur) {
		s.notifyChildOfEdit(child, uri, cur)
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"options":      formattingOptions(cur),
	}
	fo := formatOutcome{Mode: "file"}
	method := "textDocument/formatting"
	if rangeOK {
		start, end := changedLines(orig, cur)
		params["range"] = map[string]any{"start": lspPosition(cur, start), "end": lspPosition(cur, end)}
		fo.Mode, method = "range", "textDocument/rangeFormatting"
	}
	wait := s.diagWaitDuration()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	raw, err := child.Call(ctx, method, params)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return cur, formatOutcome{Skipped: fmtSkipTimeout, Note: fmt.Sprintf("%s: no answer within %s (--validate-wait); edit kept, not formatted", method, wait)}
		}
		return cur, formatOutcome{Skipped: fmtSkipError, Note: fmt.Sprintf("%s: %v; edit kept, not formatted", method, err)}
	}
	var tes []lspTextEdit
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &tes); err != nil {
			return cur, formatOutcome{Skipped: fmtSkipError, Note: fmt.Sprintf("%s: unreadable result: %v; edit kept, not formatted", method, err)}
		}
	}
	edits, err := toByteEdits(cur, tes)
	if err != nil {
		return cur, formatOutcome{Skipped: fmtSkipError, Note: fmt.Sprintf("%s: %v; edit kept, not formatted", method, err)}
	}
	out := applyByteEdits(cur, edits)
	if bytes.Equal(out, cur) {
		return cur, fo
	}
	// Written only over the kept text: something else that wrote the
	// file since keeps its bytes.
	if now, err := os.ReadFile(abs); err != nil || !bytes.Equal(now, cur) {
		return cur, formatOutcome{Skipped: fmtSkipChangedOnDisk, Note: "the file changed on disk after the edit; edit kept, not formatted"}
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode().Perm()
	}
	if err := s.atomicWrite(abs, out, mode); err != nil {
		return cur, formatOutcome{Skipped: fmtSkipError, Note: fmt.Sprintf("write the formatted text: %v; edit kept, not formatted", err)}
	}
	s.refreshFileInIndex(abs, out)
	fo.Changed, fo.edits, fo.before = true, edits, cur
	return out, fo
}

// formatFiles formats each kept file of a multi-file edit (a batch
// commit, a refactor) once, originals its text before the edit by URI.
// It returns the outcome per workspace-relative file formatting was on
// for, and the formatted texts by URI of those it changed. The caller
// tells the child.
func (s *Server) formatFiles(originals map[string][]byte) (map[string]formatOutcome, map[string][]byte) {
	uris := make([]string, 0, len(originals))
	for uri := range originals {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	outcomes := map[string]formatOutcome{}
	changed := map[string][]byte{}
	for _, uri := range uris {
		abs := uriToPath(uri)
		if !s.formatOnEditFor(abs) {
			continue
		}
		cur, err := os.ReadFile(abs)
		if err != nil {
			continue // deleted by the edit
		}
		rel := relPath(abs, s.getRoot())
		out, fo := s.formatKept(abs, originals[uri], cur)
		outcomes[rel] = fo
		if fo.Changed {
			changed[uri] = out
		}
		if fo.Note != "" {
			log.Printf("mcp: format %s: %s", rel, fo.Note)
		}
	}
	if len(outcomes) == 0 {
		outcomes = nil
	}
	return outcomes, changed
}

// formattingCaps reads documentRangeFormattingProvider and
// documentFormattingProvider from a server's capabilities: true or an
// options object declares one.
func formattingCaps(raw json.RawMessage) (rangeOK, fileOK bool) {
	var c struct {
		Range json.RawMessage `json:"documentRangeFormattingProvider"`
		File  json.RawMessage `json:"documentFormattingProvider"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return false, false
	}
	declared := func(v json.RawMessage) bool {
		t := strings.TrimSpace(string(v))
		return t == "true" || strings.HasPrefix(t, "{")
	}
	return declared(c.Range), declared(c.File)
}

// formattingOptions are the FormattingOptions for text: tabs when a line
// is indented with one, else spaces with the smallest indent found (4
// when none).
func formattingOptions(text []byte) map[string]any {
	size := 0
	for _, line := range bytes.Split(text, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if line[0] == '\t' {
			return map[string]any{"tabSize": 4, "insertSpaces": false}
		}
		n := len(line) - len(bytes.TrimLeft(line, " "))
		if n > 0 && n < len(line) && (size == 0 || n < size) {
			size = n
		}
	}
	if size == 0 || size > 8 {
		size = 4
	}
	return map[string]any{"tabSize": size, "insertSpaces": true}
}

// changedRegion is the byte region [start, end) of cur that differs from
// orig: what is left between their common prefix and suffix. A create
// (orig nil) is all of cur.
func changedRegion(orig, cur []byte) (start, end int) {
	if orig == nil {
		return 0, len(cur)
	}
	pre := 0
	for pre < len(orig) && pre < len(cur) && orig[pre] == cur[pre] {
		pre++
	}
	suf := 0
	for suf < len(orig)-pre && suf < len(cur)-pre && orig[len(orig)-1-suf] == cur[len(cur)-1-suf] {
		suf++
	}
	return pre, len(cur) - suf
}

// lineSpan widens [start, end) of text to whole lines: from the start of
// start's line to past the newline that ends end's line (the end of
// text when none).
func lineSpan(text []byte, start, end int) (int, int) {
	start = min(max(start, 0), len(text))
	end = min(max(end, start), len(text))
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	// An end just past a newline already ends a line; else run to it.
	if end == start || text[end-1] != '\n' {
		if i := bytes.IndexByte(text[end:], '\n'); i >= 0 {
			end += i + 1
		} else {
			end = len(text)
		}
	}
	return start, end
}

// changedLines is the byte span of the whole lines of cur the edit
// changed (changedRegion widened by lineSpan).
func changedLines(orig, cur []byte) (int, int) {
	start, end := changedRegion(orig, cur)
	return lineSpan(cur, start, end)
}

// lspPosition is the LSP position (0-based line, UTF-16 character) of
// byte offset off in text.
func lspPosition(text []byte, off int) map[string]any {
	off = min(max(off, 0), len(text))
	line := bytes.Count(text[:off], []byte("\n"))
	ls := bytes.LastIndexByte(text[:off], '\n') + 1
	col, ok := byteOffsetToUTF16Col(text, line+1, off-ls+1)
	if !ok {
		col = 1
	}
	return map[string]any{"line": line, "character": col - 1}
}

// lspOffset is the byte offset in text of an LSP position. A line past
// the end is the end of text (servers end a whole-file edit there).
func lspOffset(text []byte, p lspPos) (int, bool) {
	if p.Line < 0 || p.Character < 0 {
		return 0, false
	}
	if p.Line > bytes.Count(text, []byte("\n")) {
		return len(text), true
	}
	return utf16ColToByteOffset(text, p.Line+1, p.Character+1)
}

// toByteEdits converts a server's TextEdits on text into sorted,
// non-overlapping byte edits.
func toByteEdits(text []byte, tes []lspTextEdit) ([]byteEdit, error) {
	out := make([]byteEdit, 0, len(tes))
	for _, te := range tes {
		start, ok1 := lspOffset(text, te.Range.Start)
		end, ok2 := lspOffset(text, te.Range.End)
		if !ok1 || !ok2 || start > end {
			return nil, fmt.Errorf("an edit's range %d:%d-%d:%d is not in the file", te.Range.Start.Line, te.Range.Start.Character, te.Range.End.Line, te.Range.End.Character)
		}
		out = append(out, byteEdit{start: start, end: end, text: te.NewText})
	}
	// Stable: edits at one position apply in the order the server sent.
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	for i := 1; i < len(out); i++ {
		if out[i].start < out[i-1].end {
			return nil, errors.New("the server's edits overlap")
		}
	}
	return out, nil
}

// applyByteEdits applies sorted, non-overlapping edits to text.
func applyByteEdits(text []byte, edits []byteEdit) []byte {
	var b bytes.Buffer
	b.Grow(len(text))
	last := 0
	for _, e := range edits {
		b.Write(text[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.Write(text[last:])
	return b.Bytes()
}

// mapOffset maps byte offset off of the text before edits to the text
// after them. An edit at or past off leaves it; one that ends at or
// before off shifts it; one whose range holds off maps it to the start
// of its replacement, or with toEnd to the end.
func mapOffset(edits []byteEdit, off int, toEnd bool) int {
	delta := 0
	for _, e := range edits {
		if e.start >= off {
			break
		}
		if e.end > off {
			if toEnd {
				return e.start + delta + len(e.text)
			}
			return e.start + delta
		}
		delta += len(e.text) - (e.end - e.start)
	}
	return off + delta
}

// mapLine maps a 1-based line of before (the text before edits) to the
// line in the text after them where it starts now.
func mapLine(before []byte, edits []byteEdit, line int) int {
	ls, ok := lineStartOffset(before, line)
	if !ok {
		return line
	}
	off := mapOffset(edits, ls, false)
	after := applyByteEdits(before, edits)
	return 1 + bytes.Count(after[:min(off, len(after))], []byte("\n"))
}

// attachFormat writes the single-file format fields into an edit's
// result: formatted / formatSkipped (+ formatNote), and when the format
// changed the text, formattedText and formattedLines: the lines the edit
// changed, as they read in the formatted file. orig is the text before
// the edit (nil: a create), cur the kept text before the format, final
// the file after it.
func (fo formatOutcome) attach(p map[string]any, orig, cur, final []byte) {
	switch {
	case fo.Mode != "":
		p["formatted"] = fo.Mode
	case fo.Skipped != "":
		p["formatSkipped"] = fo.Skipped
		if fo.Note != "" {
			p["formatNote"] = fo.Note
		}
		return
	default:
		return
	}
	if !fo.Changed {
		return
	}
	start, end := changedRegion(orig, cur)
	start, end = mapOffset(fo.edits, start, false), mapOffset(fo.edits, end, true)
	start, end = lineSpan(final, start, end)
	first := 1 + bytes.Count(final[:start], []byte("\n"))
	last := first
	if seg := final[start:end]; len(seg) > 0 {
		n := bytes.Count(seg, []byte("\n"))
		if seg[len(seg)-1] != '\n' {
			n++
		}
		last = first + n - 1
	}
	p["formattedLines"] = []int{first, last}
	if end-start > maxNodeReadBytes {
		p["formatNote"] = fmt.Sprintf("the formatter changed the edited text; lines %d-%d are too large to return here (%d bytes): node_read them before the next edit", first, last, end-start)
		return
	}
	p["formattedText"] = string(final[start:end])
	p["formatNote"] = "the formatter changed the edited text: formattedText is what the file holds now; base the next oldText on it, not on your newText"
}

// attachFormatFiles writes a multi-file edit's format fields:
// formattedFiles (file → range|file) and formatSkippedFiles (file →
// reason), by workspace-relative file.
func attachFormatFiles(p map[string]any, outcomes map[string]formatOutcome) {
	done, skipped := map[string]string{}, map[string]string{}
	changed := false
	for rel, fo := range outcomes {
		switch {
		case fo.Mode != "":
			done[rel] = fo.Mode
			changed = changed || fo.Changed
		case fo.Skipped != "":
			skipped[rel] = fo.Skipped
		}
	}
	if len(done) > 0 {
		p["formattedFiles"] = done
	}
	if len(skipped) > 0 {
		p["formatSkippedFiles"] = skipped
	}
	if changed {
		p["formatNote"] = "the formatter changed some formattedFiles: node_read them before editing them again"
	}
}
