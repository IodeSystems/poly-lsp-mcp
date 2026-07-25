// Package daemon runs one long-lived poly-lsp process per user that
// hosts many workspace roots (projects, and worktree branches of those
// projects — each a distinct absolute path, hence a distinct root) over
// a UNIX socket. Clients (the `poly-lsp-mcp mcp` stdio proxy, or any
// non-MCP caller) dial the socket and address a root by its absolute
// path. The design is a straight port of raglit's daemon lifecycle
// (state file, detached auto-start, --stop/--restart, health poll) with
// the TCP listener swapped for a unix socket gated by peer credentials
// and declared root prefixes. See plan/plan.md "Daemon mode".
package daemon

import (
	"os"
	"path/filepath"
)

// Version is the daemon protocol/build version recorded in the state
// file so a client can detect a stale daemon after an upgrade.
const Version = "0.1.0"

// ConfigHome is the persistent directory holding the state file and log
// (~/.poly-lsp). Persistent, unlike the socket, because XDG_RUNTIME_DIR
// is cleared on logout while state/log want to survive.
func ConfigHome() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".poly-lsp")
	}
	// Last resort: a temp dir. A daemon with no home is degenerate but
	// shouldn't panic.
	return filepath.Join(os.TempDir(), "poly-lsp")
}

// SocketPath is where the daemon listens. Prefer $XDG_RUNTIME_DIR (the
// correct home for ephemeral sockets — tmpfs, per-user, cleared on
// logout); fall back to ConfigHome so a machine without XDG set still
// works.
func SocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "poly-lsp", "daemon.sock")
	}
	return filepath.Join(ConfigHome(), "daemon.sock")
}

// StatePath is the discovery file (~/.poly-lsp/daemon.json): pid, socket
// path, start time, version. A client reads it to find a running daemon;
// its socket field is authoritative over SocketPath() so a daemon
// started with a custom socket is still discoverable.
func StatePath() string {
	return filepath.Join(ConfigHome(), "daemon.json")
}

// LogPath is where a detached auto-started daemon sends stdout+stderr.
func LogPath() string {
	return filepath.Join(ConfigHome(), "daemon.log")
}
