package mcp

// Files outside the workspace root: node_read and node_edit reach an
// absolute path inside an allowed directory (--allow-path; by default
// os.TempDir() and /tmp), so an agent can read or patch a file a command
// just wrote there with the same tools. Such a file is not indexed, not
// watched and has no child LSP: it is parsed on demand by extension, so
// file#Symbol addresses work, and an edit is checked by a tree-sitter parse
// only. references and rename stay workspace-only.
//
// /tmp is shared by every local user, so access is narrow:
//   - no symlink in any component below the allowed directory (Lstat each,
//     and every open goes through an os.Root with O_NOFOLLOW, which also
//     refuses a symlink swapped in after the check);
//   - regular files owned by this process's uid only;
//   - a new file only in a directory owned by our uid, created with O_EXCL;
//   - an existing file is rewritten through a temp file in its directory
//     (O_EXCL) renamed over it, keeping its mode.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// DefaultAllowPaths are the directories outside the root that node_read and
// node_edit reach unless --exclude-path removes them: os.TempDir() (from
// $TMPDIR, a per-user /var/folders/... on macOS) and /tmp.
func DefaultAllowPaths() []string { return []string{os.TempDir(), "/tmp"} }

// pathPolicy is one client's access outside the root: allowed directories
// minus excluded ones. Each directory is kept in the spelling given and its
// resolved one, so /tmp and /private/tmp (macOS) both match.
type pathPolicy struct {
	allow   []string
	exclude []string
}

func newPathPolicy(allow, exclude []string) *pathPolicy {
	return &pathPolicy{allow: spellings(allow), exclude: spellings(exclude)}
}

// spellings cleans each absolute dir and adds its resolved form, deduped.
// Relative entries are dropped (the flags refuse them).
func spellings(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			continue
		}
		d = filepath.Clean(d)
		add(d)
		if r, err := filepath.EvalSymlinks(d); err == nil {
			add(r)
		}
	}
	return out
}

// base returns the allowed directory abs lies under, by the longest match,
// and false when none does or an excluded directory holds abs (in either
// spelling).
func (p *pathPolicy) base(abs string) (string, bool) {
	if p == nil {
		return "", false
	}
	best := ""
	for _, d := range p.allow {
		if abs != d && relInside(d, abs) && len(d) > len(best) {
			best = d
		}
	}
	if best == "" {
		return "", false
	}
	resolved := abs
	if r, err := filepath.EvalSymlinks(best); err == nil {
		rel, _ := filepath.Rel(best, abs)
		resolved = filepath.Join(r, rel)
	}
	for _, x := range p.exclude {
		if relInside(x, abs) || relInside(x, resolved) {
			return "", false
		}
	}
	return best, true
}

func (p *pathPolicy) describe() string {
	if p == nil || len(p.allow) == 0 {
		return "none (--allow-path adds one)"
	}
	return strings.Join(p.allow, ", ")
}

// SetPathPolicy sets which directories outside the root node_read and
// node_edit reach (--allow-path / --exclude-path, after defaults). nil allow
// = none. In daemon mode each connection carries its own (CallOptions).
func (s *Server) SetPathPolicy(allow, exclude []string) {
	s.pathPolicy = newPathPolicy(allow, exclude)
}

// setSessionPathPolicy records a daemon connection's lists; nil allow
// (the proxy sent none) leaves the server's.
func (s *Server) setSessionPathPolicy(sess sessionID, allow, exclude []string) {
	if allow == nil {
		return
	}
	s.sessionWaitsMu.Lock()
	defer s.sessionWaitsMu.Unlock()
	if s.sessionPaths == nil {
		s.sessionPaths = map[sessionID]*pathPolicy{}
	}
	s.sessionPaths[sess] = newPathPolicy(allow, exclude)
}

func (s *Server) pathPolicyFor(sess sessionID) *pathPolicy {
	s.sessionWaitsMu.Lock()
	defer s.sessionWaitsMu.Unlock()
	if p, ok := s.sessionPaths[sess]; ok {
		return p
	}
	return s.pathPolicy
}

// outsideFile is a vetted path outside the root: rel below the allowed
// directory base. exists is false for a file node_edit may create.
type outsideFile struct {
	base, rel, abs string
	exists         bool
	mode           os.FileMode
}

// outsideFilePart returns the file part of a node address when it is an
// absolute path outside the workspace root.
func (s *Server) outsideFilePart(node string) (string, bool) {
	file := node
	if m := refSiteAddr.FindStringSubmatch(node); m != nil {
		file = m[1]
	} else if h := strings.IndexByte(node, '#'); h >= 0 {
		file = node[:h]
	}
	if s.getRoot() == "" || !filepath.IsAbs(file) {
		return "", false
	}
	if _, err := s.resolveFileArg(file); err == nil {
		return "", false
	}
	return filepath.Clean(file), true
}

// openOutside vets an absolute path outside the root against sess's
// policy and the safety rules above.
func (s *Server) openOutside(sess sessionID, file string) (*outsideFile, error) {
	abs := filepath.Clean(file)
	pol := s.pathPolicyFor(sess)
	base, ok := pol.base(abs)
	if !ok {
		return nil, fmt.Errorf("path escapes the workspace root: %q (paths must resolve inside %s, or inside an allowed directory outside it: %s)",
			file, s.getRoot(), pol.describe())
	}
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("allowed directory %s is not a directory", base)
	}
	rel, _ := filepath.Rel(base, abs)
	f := &outsideFile{base: base, rel: rel, abs: abs}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := base
	for i, part := range parts {
		parent := cur
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		last := i == len(parts)-1
		switch {
		case os.IsNotExist(err) && last:
			// A create: only in a directory we own (base itself may be a
			// symlink the policy names, e.g. /tmp on macOS).
			pinfo, err := os.Stat(parent)
			if err != nil {
				return nil, err
			}
			if !ownedByUs(pinfo) {
				return nil, fmt.Errorf("%s does not exist, and a new file outside the root is created only in a directory owned by this user; %s is not", abs, parent)
			}
			return f, nil
		case err != nil:
			return nil, err
		case info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("refused %s: %s is a symlink (outside the root, no path component may be one)", abs, cur)
		case !last && !info.IsDir():
			return nil, fmt.Errorf("refused %s: %s is not a directory", abs, cur)
		case last && info.IsDir():
			return nil, fmt.Errorf("%s is a directory; outside the root node_read and node_edit take a file", abs)
		case last && !info.Mode().IsRegular():
			return nil, fmt.Errorf("refused %s: not a regular file (%s)", abs, info.Mode().Type())
		case last && !ownedByUs(info):
			return nil, fmt.Errorf("refused %s: owned by another user (outside the root, only this user's own files are read or edited)", abs)
		case last:
			f.exists, f.mode = true, info.Mode().Perm()
		}
	}
	return f, nil
}

func ownedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// noFollow is the open flag every outside access carries: through an
// os.Root it also refuses a symlink in an intermediate component.
const noFollow = syscall.O_NOFOLLOW

func (f *outsideFile) root() (*os.Root, error) { return os.OpenRoot(f.base) }

// read returns the file's bytes, re-checking on the open descriptor that it
// is still a regular file of ours. O_NONBLOCK: a fifo swapped in after the
// check must not hang the open.
func (f *outsideFile) read() ([]byte, error) {
	r, err := f.root()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	h, err := r.OpenFile(f.rel, os.O_RDONLY|noFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.abs, err)
	}
	defer h.Close()
	info, err := h.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedByUs(info) {
		return nil, fmt.Errorf("refused %s: no longer a regular file of this user", f.abs)
	}
	return io.ReadAll(h)
}

// write replaces an existing file: a temp file beside it (O_EXCL, random
// name), the old mode, then a rename over it.
func (f *outsideFile) write(data []byte) error {
	r, err := f.root()
	if err != nil {
		return err
	}
	defer r.Close()
	var b [8]byte
	_, _ = rand.Read(b[:])
	tmp := filepath.Join(filepath.Dir(f.rel), "."+filepath.Base(f.rel)+".poly-lsp-mcp-"+hex.EncodeToString(b[:])+".tmp")
	h, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	_, werr := h.Write(data)
	if werr == nil {
		werr = h.Chmod(f.mode)
	}
	if cerr := h.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		info, err := r.Lstat(f.rel)
		switch {
		case err != nil:
			werr = err
		case !info.Mode().IsRegular() || !ownedByUs(info):
			werr = fmt.Errorf("refused %s: no longer a regular file of this user", f.abs)
		}
	}
	if werr == nil {
		werr = r.Rename(tmp, f.rel)
	}
	if werr != nil {
		_ = r.Remove(tmp)
		return fmt.Errorf("write %s: %w", f.abs, werr)
	}
	return nil
}

// create writes a new file with O_EXCL: it fails rather than open anything
// that appeared at the path since the check.
func (f *outsideFile) create(data []byte) error {
	r, err := f.root()
	if err != nil {
		return err
	}
	defer r.Close()
	h, err := r.OpenFile(f.rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", f.abs, err)
	}
	_, werr := h.Write(data)
	if cerr := h.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = r.Remove(f.rel)
		return fmt.Errorf("create %s: %w", f.abs, werr)
	}
	return nil
}

func (f *outsideFile) remove() error {
	r, err := f.root()
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Remove(f.rel)
}

// resolveModernNodeAs is resolveModernNode for a tool call of session
// sess: an absolute path outside the root resolves through sess's
// --allow-path policy (resolveOutsideNode).
func (s *Server) resolveModernNodeAs(sess sessionID, node string) (*modernNode, error) {
	node = strings.TrimSpace(node)
	if file, ok := s.outsideFilePart(node); ok {
		return s.resolveOutsideNode(sess, node, file)
	}
	return s.resolveModernNode(node)
}

// resolveOutsideNode resolves <abs>, <abs>#<sym> or <abs>@<line>[-<end>]
// for a file outside the root. The address keeps the absolute path, as
// given (cleaned). Selectors do not reach these files: they are not in the
// index.
func (s *Server) resolveOutsideNode(sess sessionID, node, file string) (*modernNode, error) {
	of, err := s.openOutside(sess, file)
	if err != nil {
		return nil, err
	}
	var content []byte
	if of.exists {
		if content, err = of.read(); err != nil {
			return nil, err
		}
	}
	var rn *modernNode
	if m := refSiteAddr.FindStringSubmatch(node); m != nil {
		if !of.exists {
			return nil, fmt.Errorf("no such file: %s", of.abs)
		}
		rn, err = refSiteNode(of.abs, content, m[2], m[3])
	} else {
		sym := ""
		if h := strings.IndexByte(node, '#'); h >= 0 {
			sym = node[h+1:]
		}
		switch {
		case !isClassicSymPath(sym):
			return nil, fmt.Errorf("%q: a file outside the workspace root is not indexed, so selectors do not reach it; address it as <path> or <path>#<Symbol>", node)
		case !of.exists && sym == "":
			rn = &modernNode{class: "file", file: of.abs, addr: of.abs, exists: false}
		case !of.exists:
			return nil, fmt.Errorf("no such file: %s", of.abs)
		default:
			rn, err = s.classicNode(of.abs, of.abs, content, sym, "")
		}
	}
	if err != nil {
		return nil, err
	}
	rn.out = of
	return rn, nil
}

// readAdvice is the read hint's search advice; a file outside the root is
// not indexed, so node_query cannot search it: address a symbol instead.
func readAdvice(rn *modernNode) string {
	if rn.out != nil {
		return fmt.Sprintf("read one declaration as %s#<Name>", rn.file)
	}
	return targetedSearchAdvice(rn.file, true)
}

// attachEnclosingSymbolOf is attachEnclosingSymbol, skipped for a file
// outside the root (it would re-open the path without the checks).
func (s *Server) attachEnclosingSymbolOf(out map[string]any, rn *modernNode, abs string, line int) {
	if rn.out == nil {
		s.attachEnclosingSymbol(out, rn.file, abs, line)
	}
}

// outsideMark adds the outside-the-root note to a read payload.
func outsideMark(out map[string]any, rn *modernNode) map[string]any {
	if rn.out != nil {
		out["outsideRoot"] = outsideReadNote
	}
	return out
}

// outsideReadNote says what a read of an outside file is not.
const outsideReadNote = "outside the workspace root: not indexed, no child LSP; node_edit checks a tree-sitter parse only"

// editOutside runs one node_edit op on a file outside the root. No batch
// (commit:false), no refactor (rename / params / return are workspace-only),
// no conflict accept, no whole-file delete. Validation is a tree-sitter
// parse: under --validate (or validate:true) an edit that makes a cleanly
// parsing file fail to parse, or creates one that does not parse, is
// reverted.
func (s *Server) editOutside(sess sessionID, p *modernEditArgs, commit bool) ([]Content, bool, error) {
	rn, err := s.resolveModernNodeAs(sess, p.Node)
	if err != nil {
		return nil, true, err
	}
	if !commit || s.currentBatch() != nil {
		return nil, true, errors.New("a file outside the workspace root cannot join a commit:false batch; commit or roll back the batch, then edit it on its own")
	}
	switch {
	case p.Rename != nil || p.Params != nil || p.Return != nil:
		return nil, true, errors.New("rename / params / return are workspace-only: a file outside the root is not indexed, so its references are unknown. Edit its text with oldText/newText")
	case p.Accept != nil:
		return nil, true, errors.New("accept: is workspace-only")
	}
	of := rn.out
	if p.NewText != nil && p.OldText != nil && *p.OldText == "" && !rn.exists {
		p.OldText = nil
	}
	op := "edit"
	if !rn.exists {
		op = "create"
	}
	if cancel, note := s.fileAccessGuardAbs(op, of.abs); cancel {
		return HookBlockedResult(note), false, nil
	}

	// ---- create
	if !rn.exists {
		if p.NewText == nil || p.OldText != nil {
			return nil, true, fmt.Errorf("no such file: %s", of.abs)
		}
		if *p.NewText == "" {
			return nil, true, fmt.Errorf("creating %s needs non-empty newText", of.abs)
		}
		out := []byte(*p.NewText)
		if err := of.create(out); err != nil {
			return nil, true, err
		}
		of.exists, of.mode = true, 0o644
		return s.outsideResult(rn, nil, out, "created", of.abs, p.Validate)
	}

	content, err := of.read()
	if err != nil {
		return nil, true, err
	}
	var updated string
	switch {
	case p.Delete != nil:
		if rn.wholeFile() {
			return nil, true, fmt.Errorf("delete:true on a whole file outside the workspace root is refused; remove %s with your shell", of.abs)
		}
		updated = ""
	case rn.class == "ref" && p.NewText != nil && p.OldText == nil:
		updated = *p.NewText
	case p.NewText != nil && p.OldText == nil:
		return nil, true, fmt.Errorf("node already exists; supply oldText to modify it (%s). To replace it entirely, pass its whole current text as oldText", rn.addr)
	case p.OldText != nil:
		cur := string(content)
		if !rn.wholeFile() {
			if cur, err = readRangeText(content, rn.decl); err != nil {
				return nil, true, err
			}
		}
		offs := occurrencesOf(cur, *p.OldText)
		switch len(offs) {
		case 1:
		case 0:
			return nil, true, oldTextNotFoundErr(rn.addr, cur)
		default:
			return nil, true, oldTextAmbiguousErr(rn.addr, cur, *p.OldText, offs)
		}
		updated = cur[:offs[0]] + *p.NewText + cur[offs[0]+len(*p.OldText):]
		if rn.wholeFile() && updated == "" {
			return nil, true, fmt.Errorf("that edit would empty %s; outside the root a file is not deleted", of.abs)
		}
	default:
		return nil, true, errors.New("node_edit takes " + editOpShapes)
	}

	out := []byte(updated)
	label := of.abs
	if !rn.wholeFile() {
		a := rn.decl
		start, ok1 := lineColToByteOffset(content, a.StartLine, a.StartCol)
		end, ok2 := lineColToByteOffset(content, a.EndLine, a.EndCol)
		if !ok1 || !ok2 || start > end {
			return nil, true, fmt.Errorf("%s: range %d:%d-%d:%d is out of the file", rn.addr, a.StartLine, a.StartCol, a.EndLine, a.EndCol)
		}
		end = min(end, len(content))
		out = append(append(append([]byte{}, content[:start]...), updated...), content[end:]...)
		label = fmt.Sprintf("%s:%d-%d", of.abs, a.StartLine, a.EndLine)
	}
	if err := of.write(out); err != nil {
		return nil, true, err
	}
	return s.outsideResult(rn, content, out, "updated", label, p.Validate)
}

// outsideResult checks the parse, reverts a parse-breaking edit when
// validating, and builds the response. orig nil = a create.
func (s *Server) outsideResult(rn *modernNode, orig, out []byte, verb, label string, validate bool) ([]Content, bool, error) {
	of := rn.out
	p := map[string]any{
		"node":                 rn.addr,
		"bytesRemoved":         len(orig),
		"bytesAdded":           len(out),
		"diagnosticsAvailable": false,
		"diagnostics":          []diagnosticJSON{},
		"validation":           "parse-only",
		"note":                 editNote(verb, label, len(orig), len(out)) + " (outside the workspace root: parse check only, no child LSP)",
	}
	if verb == "created" {
		p["created"] = true
	}
	lang := s.languageForFile(of.abs)
	if symbols.LanguageByName(lang) == nil {
		p["validation"] = "none"
		p["validated"] = false
		p["validationSkipped"] = "no-grammar"
		return jsonContent(p), false, nil
	}
	clean := symbols.ParsesCleanly(lang, out)
	p["parsesCleanly"] = clean
	broke := !clean && (orig == nil || symbols.ParsesCleanly(lang, orig))
	if !broke || !(validate || s.validateEdits) {
		return jsonContent(p), false, nil
	}
	var rerr error
	if orig == nil {
		rerr = of.remove()
	} else {
		rerr = of.write(orig)
	}
	rej := map[string]any{"rejected": true, "node": rn.addr, "validation": "parse-only", "parsesCleanly": false}
	if rerr != nil {
		rej["revertFailed"], rej["revertError"] = true, rerr.Error()
		rej["note"] = "REJECTED — the edit does not parse (tree-sitter) AND revert FAILED (" + label + ")"
	} else {
		rej["reverted"] = true
		rej["note"] = "REJECTED — reverted; the edit does not parse (tree-sitter, " + lang + ") (" + label + ")"
	}
	return jsonContent(rej), true, nil
}
