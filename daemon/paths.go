// Package daemon runs one long-lived poly-lsp process per user that
// hosts many workspace roots (projects, and worktree branches of those
// projects — each a distinct absolute path, hence a distinct root) over
// a UNIX socket. Clients (the `poly-lsp-mcp mcp` stdio proxy, or any
// non-MCP caller) dial the socket and address a root by its absolute
// path. The design is a straight port of raglit's daemon lifecycle
// (state file, detached auto-start, --stop/--restart, health poll) with
// the TCP listener swapped for a unix socket gated by peer credentials
// and declared root prefixes. See docs/design.md, "Daemon".
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// HomeEnv names the daemon home: when set, ConfigHome is this directory
// instead of ~/.poly-lsp, and the socket and status files live in it too
// (see SetHome). It is also how the choice reaches every process a client
// or daemon starts — an auto-started daemon, a takeover build, a restart —
// since each inherits the environment.
const HomeEnv = "POLY_LSP_HOME"

// SetHome resolves the daemon home for this process: dir (a --daemon-home
// flag) wins over $POLY_LSP_HOME; both empty leaves the default ($HOME,
// $XDG_RUNTIME_DIR) and returns "". The directory is made absolute and
// created 0700 if missing, and written back to $POLY_LSP_HOME so the
// processes this one starts use it as well.
func SetHome(dir string) (string, error) {
	if dir == "" {
		dir = os.Getenv(HomeEnv)
	}
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("daemon home %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("daemon home: %w", err)
	}
	if err := os.Setenv(HomeEnv, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// customHome is $POLY_LSP_HOME made absolute, or "" when unset.
func customHome() string {
	h := os.Getenv(HomeEnv)
	if h == "" {
		return ""
	}
	if abs, err := filepath.Abs(h); err == nil {
		return abs
	}
	return h
}

// ConfigHome is the persistent directory holding the state file and log
// (~/.poly-lsp, or $POLY_LSP_HOME). Persistent, unlike the socket, because
// XDG_RUNTIME_DIR is cleared on logout while state/log want to survive.
func ConfigHome() string {
	if h := customHome(); h != "" {
		return h
	}
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
// works. With $POLY_LSP_HOME set the socket is in that directory, so two
// daemon homes never share one.
//
// bind(2) refuses a path longer than sun_path (maxSocketPath), and a deep
// $HOME, $POLY_LSP_HOME or $XDG_RUNTIME_DIR reaches it; then the socket is
// a short name derived from ConfigHome, in the runtime dir, else the temp
// dir, else /tmp — the fallback dun's dund uses.
func SocketPath() string {
	p := filepath.Join(ConfigHome(), "daemon.sock")
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && customHome() == "" {
		p = filepath.Join(rt, "poly-lsp", "daemon.sock")
	}
	if len(p) <= maxSocketPath() {
		return p
	}
	sum := sha256.Sum256([]byte(ConfigHome()))
	name := "poly-lsp-" + hex.EncodeToString(sum[:6]) + ".sock"
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if p := filepath.Join(rt, name); len(p) <= maxSocketPath() {
			return p
		}
	}
	if p := filepath.Join(os.TempDir(), name); len(p) <= maxSocketPath() {
		return p
	}
	// $TMPDIR is too deep as well.
	return filepath.Join("/tmp", name)
}

// maxSocketPath is the longest unix socket path bind accepts: sun_path less
// its NUL — 108 bytes on Linux, 104 on macOS and the BSDs.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// StatusDir holds one child-LSP status file per manager (per stdio mcp
// process, per daemon root); `poly-lsp-mcp status` reads them. Runtime
// dir, like the socket: the files describe live processes only. With
// $POLY_LSP_HOME set, in that directory.
func StatusDir() string {
	if h := customHome(); h != "" {
		return filepath.Join(h, "children")
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "poly-lsp", "children")
	}
	return filepath.Join(ConfigHome(), "children")
}

// StatePath is the discovery file (~/.poly-lsp/daemon.json): pid, socket
// path, start time, version. A client reads it to find a running daemon;
// its socket field is authoritative over SocketPath() so a daemon
// started with a custom socket is still discoverable.
func StatePath() string {
	return filepath.Join(ConfigHome(), "daemon.json")
}

// UsagePath is the node_query usage log (one JSON line per query), shared
// by every server this user runs with the same daemon home. See
// mcp/usage.go.
func UsagePath() string {
	return filepath.Join(ConfigHome(), "usage.jsonl")
}

// LogPath is where a detached auto-started daemon sends stdout+stderr.
func LogPath() string {
	return filepath.Join(ConfigHome(), "daemon.log")
}
