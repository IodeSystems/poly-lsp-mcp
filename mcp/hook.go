package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileHook is an optional host-provided interceptor consulted BEFORE a
// file read (node_read) or edit (node_edit) touches disk. The host — the
// process that spawned this server — supplies an external executable via
// the --hook flag or the POLY_LSP_HOOK env var. This package stays
// policy-free: it only asks "cancel?" and relays the answer.
//
// Protocol: the executable is invoked as <path> <op> <absPath> with its
// working directory set to the file's directory.
//   - exit 0  → proceed (stdout ignored)
//   - exit 2  → cancel; stdout (trimmed) is a note injected into the
//     tool result, prefixed with "[hook] "
//   - anything else (missing binary, non-zero/exit-2 code, timeout) →
//     proceed. The hook must never be able to break a working tool.
//
// Invocations are bounded by a 2-second timeout so a wedged hook cannot
// stall the server or its child LSPs.
type FileHook struct {
	Path string
}

// hookTimeout bounds one external hook invocation.
const hookTimeout = 2 * time.Second

// hookCancelExit is the exit code that means "cancel the operation".
const hookCancelExit = 2

// Cancelled reports whether a hook is configured.
func (h *FileHook) Cancelled() bool {
	return h != nil && h.Path != ""
}

// Invoke runs the hook for one operation and reports whether to cancel.
// note is only meaningful when cancel is true.
func (h *FileHook) Invoke(op, absPath string) (cancel bool, note string) {
	if !h.Cancelled() {
		return false, ""
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), hookTimeout)
	defer cancelFn()

	cmd := exec.CommandContext(ctx, h.Path, op, absPath)
	cmd.Dir = filepath.Dir(absPath)
	out, err := cmd.Output()
	if err == nil {
		// exit 0: proceed, stdout ignored.
		return false, ""
	}
	var ee *exec.ExitError
	// Only an exit-2 cancels. ENOENT, a timeout kill, and every
	// other code proceed with an empty note.
	if !errors.As(err, &ee) || ee.ExitCode() != hookCancelExit {
		return false, ""
	}
	return true, strings.TrimSpace(string(out))
}

// hookNotePrefix is the prefix every injected note carries, so a model
// (and a human reading the log) can tell hook output from a real tool
// result.
const hookNotePrefix = "[hook] "

// HookBlockedResult builds the tool result for a cancelled operation (the
// daemon proxy, which runs the hook client-side, answers with it too).
// It is deliberately NOT an error: the model should read the note as
// guidance (e.g. "read AGENTS.md first"), not as a failure to retry
// verbatim.
func HookBlockedResult(note string) []Content {
	note = strings.TrimSpace(note)
	if note == "" {
		note = "operation cancelled by host hook"
	}
	return []Content{{Type: "text", Text: hookNotePrefix + note}}
}

// fileAccessGuard runs the configured hook for one file access. relPath
// is workspace-relative (as node addresses carry it); it is resolved to
// an absolute path before the hook is invoked. Returns cancel=true with
// the note only when a configured hook explicitly refused the operation
// — every other outcome (no hook, bad path, hook error) proceeds.
func (s *Server) fileAccessGuard(op, relPath string) (bool, string) {
	h := s.fileAccessHook
	if !h.Cancelled() {
		return false, ""
	}
	abs, err := s.resolveFileArg(relPath)
	if err != nil {
		return false, ""
	}
	return h.Invoke(op, abs)
}

// FileAccessTool reports whether a tool's calls consult the file-access
// hook: node_read and node_edit, the tools that call fileAccessGuard.
func FileAccessTool(name string) bool { return name == "node_read" || name == "node_edit" }

// FileAccess is what the guard of a node_read or node_edit call would ask
// the hook: op (read, edit or create) and the file's absolute path. ok is
// false when the call would reach no guard — no file, a directory, a
// missing file it does not create, a malformed or batch-control-only call
// (those answer their own error, or touch no file). A daemon-hosted root
// has no hook of its own: the proxy, which runs the client's hook,
// asks this first (POST /access), so the hook sees the same op and path
// it would see in-process.
//
// It mirrors the guard sites in handleModernNodeRead and
// handleModernNodeEdit; a change to which branch guards with which op
// there belongs here too (TestFileAccessMatchesTheGuard).
func (s *Server) FileAccess(name string, args json.RawMessage) (op, abs string, ok bool) {
	var p modernEditArgs // node_read's {node} is a subset
	if json.Unmarshal(args, &p) != nil || strings.TrimSpace(p.Node) == "" {
		return "", "", false
	}
	switch name {
	case "node_read":
		op = "read"
	case "node_edit":
		if p.Rollback || len(p.ops()) != 1 {
			return "", "", false
		}
	default:
		return "", "", false
	}
	rn, err := s.resolveModernNode(p.Node)
	if err != nil || rn.isDir {
		return "", "", false
	}
	if name == "node_edit" {
		switch {
		case rn.class == "ref" && p.NewText != nil && p.OldText == nil && p.Delete == nil:
			op = "edit"
		case p.NewText != nil && (p.OldText == nil || *p.OldText == "" && !rn.exists):
			if rn.exists || *p.NewText == "" {
				return "", "", false
			}
			op = "create"
		default:
			op = "edit"
		}
	}
	if op != "create" && !rn.exists {
		return "", "", false
	}
	abs, err = s.resolveFileArg(rn.file)
	if err != nil {
		return "", "", false
	}
	return op, abs, true
}
