package daemon

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A build that fails to come up is tried again, and after
// MaxUpgradeFailures refused — the old daemon serving all along — and the
// serving binary is kept as exe.prev to go back to.
func TestFailingBuildIsRefusedAfterTwoAttempts(t *testing.T) {
	root, resolved := goWorkspace(t)
	socket := serveInProcess(t, resolved)
	c := NewClient(socket)
	h := startProxy(t, c, root, OpenOptions{})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	h.logged(indexLogger, 30*time.Second, nil)

	const bad = "bad/00000002"
	exe := badBuild(t, bad)
	ask := func() UpgradeAnswer {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			a, err := c.RequestUpgrade(UpgradeRequest{Exe: exe, Build: bad, Why: "test"})
			if err != nil {
				t.Fatal(err)
			}
			// The previous attempt is still finishing.
			if a.Accepted || !strings.HasPrefix(a.Reason, "already upgrading") || time.Now().After(deadline) {
				return a
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for attempt := 1; attempt <= MaxUpgradeFailures; attempt++ {
		if a := ask(); !a.Accepted {
			t.Fatalf("attempt %d refused: %s", attempt, a.Reason)
		}
		n := float64(attempt)
		failed := h.logged(daemonLogger, 30*time.Second, func(d map[string]any) bool { return d["state"] == "failed" && d["attempt"] == n })
		if refused := failed["refused"] == true; refused != (attempt == MaxUpgradeFailures) {
			t.Errorf("attempt %d: refused=%v: %v", attempt, failed["refused"], failed)
		}
	}
	if a := ask(); a.Accepted || !strings.Contains(a.Reason, "failed to come up 2 times") {
		t.Fatalf("a twice-failed build was not refused: %+v", a)
	}
	if f := LoadUpgradeFailures()[bad]; f.Failures != 2 || !strings.Contains(f.Error, "exited") {
		t.Errorf("failure record: %+v", f)
	}

	// exe.prev is the serving binary.
	prev, err := HashFile(PrevExePath())
	if err != nil || prev != ExeHash() {
		t.Errorf("exe.prev: %v, hash match %v", err, prev == ExeHash())
	}
	if st, err := os.Stat(PrevExePath()); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("exe.prev is not executable: %v", err)
	}

	// Still serving, still this build.
	if hl, err := c.health(); err != nil || hl.Build != BuildID() {
		t.Fatalf("health: %+v %v", hl, err)
	}

	// A build that takes over clears its own record.
	clearUpgradeFailures(bad)
	if upgradeRefused(bad) != "" {
		t.Fatal("record not cleared")
	}
}
