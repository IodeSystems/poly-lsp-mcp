package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// The identity is never empty and never just "dev": a revision (or
// "unknown") and the executable's hash, so two different binaries differ.
func TestBuildIDNamesRevisionAndHash(t *testing.T) {
	id := BuildID()
	prefix, hash, ok := strings.Cut(id, "/")
	if !ok || prefix == "" || len(hash) != 8 {
		t.Fatalf("BuildID = %q, want <revision>/<8 hex>", id)
	}
	if len(ExeHash()) != 64 || !strings.HasPrefix(ExeHash(), hash) {
		t.Fatalf("ExeHash %q does not match BuildID %q", ExeHash(), id)
	}
}

// Of two takers of the daemon lock, the second is refused until the first
// lets go — what keeps two daemons started at once from both serving.
func TestLockExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	a, err := lockPath(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockPath(path, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: %v, want ErrLocked", err)
	}
	a.Release()
	b, err := lockPath(path, 0)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	b.Release()
	b.Release() // twice is safe
}

// A daemon that cannot take the lock stops before it binds anything.
func TestServeRefusesWhileTheLockIsHeld(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	held, err := TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	cfg, _, _ := config.LoadOrDefault("nonexistent.yaml")
	reg, _ := cfg.Build()
	socket := filepath.Join(t.TempDir(), "d.sock")
	err = Serve(Config{Socket: socket, Allow: NewAllowList([]string{t.TempDir()}), Reg: NewRegistry(cfg, reg, false, false)})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Serve = %v, want already running", err)
	}
	if _, err := os.Stat(socket); err == nil {
		t.Fatal("a refused daemon bound the socket")
	}
}

// sun_path is 108 bytes on Linux: a deeper socket path cannot be bound, so
// the default falls back to a short hashed name.
func TestSocketPathFitsSunPath(t *testing.T) {
	deep := "/" + strings.Repeat("d", 120)
	t.Setenv("XDG_RUNTIME_DIR", deep)
	p := SocketPath()
	if len(p) > maxSocketPath() {
		t.Fatalf("SocketPath %q (%d bytes) is too long", p, len(p))
	}
	// A deep home with no runtime dir: still short, and stable for one
	// home.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", deep)
	p = SocketPath()
	if len(p) > maxSocketPath() {
		t.Fatalf("SocketPath %q (%d bytes) is too long", p, len(p))
	}
	if !strings.HasPrefix(filepath.Base(p), "poly-lsp-") || p != SocketPath() {
		t.Fatalf("SocketPath %q is not the stable hashed fallback", p)
	}
	t.Setenv("HOME", "/h")
	if got := SocketPath(); got != "/h/.poly-lsp/daemon.sock" {
		t.Fatalf("short home: SocketPath = %q", got)
	}
}

// daemon.json carries the build and protocol, and a daemon's exit removes
// it only while it still names that daemon: after a takeover it names the
// new one.
func TestStateCarriesBuildAndIsRemovedOnlyByItsOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remove, err := WriteState("/s", nil)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := ReadState()
	if !ok || st.Build != BuildID() || st.Protocol != Protocol || st.PID != os.Getpid() {
		t.Fatalf("state = %+v", st)
	}
	// Another daemon took over: the file names it.
	st.PID = os.Getpid() + 1
	b, _ := json.Marshal(st)
	if err := os.WriteFile(StatePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, ok := ReadState(); !ok {
		t.Fatal("the old daemon's exit removed the new daemon's state")
	}
}
