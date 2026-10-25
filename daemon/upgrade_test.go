package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildDecision(t *testing.T) {
	for _, c := range []struct {
		mine, daemon, onDisk string
		want                 buildAction
	}{
		{"a", "a", "a", buildsAgree},
		{"a", "", "a", buildsAgree},   // a daemon too old to say
		{"a", "b", "b", proxyIsStale}, // this proxy's file is the daemon's build now
		{"a", "b", "a", upgradeDaemon},
		{"a", "b", "", upgradeDaemon}, // the file would not say: ask anyway
		{"a", "b", "c", buildsDiffer},
	} {
		if got := buildDecision(c.mine, c.daemon, c.onDisk); got != c.want {
			t.Errorf("buildDecision(%q, %q, %q) = %d, want %d", c.mine, c.daemon, c.onDisk, got, c.want)
		}
	}
}

// fakeBinary is a regular file standing in for a candidate binary.
func fakeBinary(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "poly-lsp-mcp")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// An upgrade is vetted before anything runs: not to this build, not to an
// older commit, not to a missing file, not to a file that says another
// build than claimed, one at a time.
func TestUpgradeRequestIsVetted(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir()) // the refused-builds record
	ran := make(chan UpgradeRequest, 4)
	release := make(chan struct{})
	u := newUpgrader(func(r UpgradeRequest) error { ran <- r; <-release; return nil })
	u.selfTime = "2026-10-24T00:00:00Z"
	probed := "new/00000000"
	u.probe = func(string) string { return probed }
	exe := fakeBinary(t, "x")

	refused := func(req UpgradeRequest, why string) {
		t.Helper()
		if a := u.request(req); a.Accepted || !strings.Contains(a.Reason, why) {
			t.Fatalf("request %+v = %+v, want refused: %s", req, a, why)
		}
	}
	refused(UpgradeRequest{Exe: exe, Build: BuildID()}, "already this build")
	refused(UpgradeRequest{Exe: exe, Build: "old/1", Time: "2026-01-01T00:00:00Z"}, "older commit")
	refused(UpgradeRequest{Exe: filepath.Join(t.TempDir(), "gone"), Build: "x/1"}, "no binary")

	// The file says another build than claimed: nothing runs.
	if a := u.request(UpgradeRequest{Exe: exe, Build: "claimed/1"}); !a.Accepted {
		t.Fatalf("not accepted: %+v", a)
	}
	waitIdle(t, u)
	select {
	case r := <-ran:
		t.Fatalf("ran an upgrade to a mismatched build: %+v", r)
	default:
	}

	// A good one runs, with the build the file says; a second waits its turn.
	if a := u.request(UpgradeRequest{Exe: exe, Build: probed, Why: "test"}); !a.Accepted {
		t.Fatalf("not accepted: %+v", a)
	}
	r := <-ran
	if r.Build != probed || r.Exe != exe {
		t.Fatalf("ran %+v", r)
	}
	refused(UpgradeRequest{Exe: exe, Build: probed}, "already upgrading")
	close(release)
	waitIdle(t, u)
}

func waitIdle(t *testing.T, u *upgrader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		idle := u.pending == nil
		u.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("upgrade still pending")
}

// A daemon whose binary is replaced by another build asks itself to move
// to it; a touch that leaves the content alone does not.
func TestWatchSelfAsksForAnUpgradeWhenTheBinaryIsReplaced(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir()) // the refused-builds record
	old := selfCheck
	selfCheck = 10 * time.Millisecond
	defer func() { selfCheck = old }()

	ran := make(chan UpgradeRequest, 1)
	u := newUpgrader(func(r UpgradeRequest) error { ran <- r; return nil })
	u.probe = func(string) string { return "replaced/12345678" }
	exe := fakeBinary(t, "v1")
	// The watched file must start as "this binary": its hash is the test
	// binary's, so copy the test binary there.
	self, _ := selfExe()
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSelf(ctx, u, exe)

	// Touched, same content: no upgrade.
	time.Sleep(30 * time.Millisecond)
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(exe, now, now)
	select {
	case r := <-ran:
		t.Fatalf("upgraded on a touch: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// Replaced by a rename, as `make install` does.
	tmp := exe + ".tmp"
	if err := os.WriteFile(tmp, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ran:
		if r.Build != "replaced/12345678" || r.Exe != exe || !strings.Contains(r.Why, "replaced") {
			t.Fatalf("ran %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no upgrade after the binary was replaced")
	}
}
