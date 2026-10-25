package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	t.Setenv(HomeEnv, t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	held, err := TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	defer func(d time.Duration) { lockWait = d }(lockWait)
	lockWait = 100 * time.Millisecond
	cfg, _, _ := config.LoadOrDefault("nonexistent.yaml")
	reg, _ := cfg.Build()
	socket := testSocket(t)
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
	t.Setenv(HomeEnv, "") // the $HOME and $XDG_RUNTIME_DIR defaults
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
	t.Setenv(HomeEnv, t.TempDir())
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

// $POLY_LSP_HOME moves every daemon path, the socket included, and a home
// too deep for sun_path gets the hashed fallback, distinct per home.
// SetHome makes a relative dir absolute and creates it 0700.
func TestDaemonHomeMovesEveryPath(t *testing.T) {
	t.Setenv(HomeEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cwd := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	cwd, _ = os.Getwd() // a symlinked $TMPDIR resolves

	if h, err := SetHome(""); err != nil || h != "" {
		t.Fatalf("SetHome(\"\") with no env = %q, %v; want the default", h, err)
	}
	h, err := SetHome("rel/dh")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, "rel", "dh")
	if h != want || os.Getenv(HomeEnv) != want {
		t.Fatalf("SetHome = %q, $%s = %q; want %q", h, HomeEnv, os.Getenv(HomeEnv), want)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("daemon home not created 0700: %v", err)
	}
	for _, p := range []string{StatePath(), LogPath(), LockPath(), PrevExePath(), UpgradeFailuresPath(), UsagePath(), StatusDir()} {
		if filepath.Dir(p) != want {
			t.Errorf("%s is not in the daemon home %s", p, want)
		}
	}
	if s := SocketPath(); len(want)+len("/daemon.sock") <= maxSocketPath() && s != filepath.Join(want, "daemon.sock") {
		t.Errorf("SocketPath = %q, want it in the daemon home", s)
	}
	// The flag wins over the env.
	other := t.TempDir()
	if h, _ := SetHome(other); h != other {
		t.Errorf("SetHome(flag) = %q, want %q", h, other)
	}

	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 110))
	t.Setenv(HomeEnv, deep)
	p1 := SocketPath()
	t.Setenv(HomeEnv, deep+"2")
	p2 := SocketPath()
	if len(p1) > maxSocketPath() || len(p2) > maxSocketPath() || p1 == p2 || !strings.HasPrefix(filepath.Base(p1), "poly-lsp-") {
		t.Fatalf("deep daemon homes: SocketPath %q, %q", p1, p2)
	}
}
