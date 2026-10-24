package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Rollback. Blue/green already keeps the old daemon serving when the new
// build fails to come up; what is left is not trying the same broken build
// forever, and a way back after a build that came up and then went bad.
//
//   - Each failed takeover is counted against the build, in
//     ~/.poly-lsp/upgrade-failures.json. At MaxUpgradeFailures that build
//     is refused: every client of it would otherwise ask again, and each
//     attempt costs a warm-up. A build that does take over clears its own
//     count. Delete the file to try a refused build again.
//   - Before it starts a new build, the serving daemon copies its own
//     binary to ~/.poly-lsp/exe.prev: the last build known to serve. If the
//     new one misbehaves after taking over, `~/.poly-lsp/exe.prev daemon
//     --restart` goes back to it.
//
// dund has neither (its open gap); doctor reports both.

// MaxUpgradeFailures is how many failed takeovers refuse a build.
const MaxUpgradeFailures = 2

// UpgradeFailure is one build's record in upgrade-failures.json.
type UpgradeFailure struct {
	Failures int    `json:"failures"`
	Error    string `json:"error"` // the last failure
	At       string `json:"at"`
}

// UpgradeFailuresPath is the per-build failure record.
func UpgradeFailuresPath() string { return filepath.Join(ConfigHome(), "upgrade-failures.json") }

// PrevExePath is a copy of the last binary that served, kept so an upgrade
// that turns out bad can be undone by hand: `<exe.prev> daemon --restart`.
func PrevExePath() string { return filepath.Join(ConfigHome(), "exe.prev") }

// LoadUpgradeFailures reads the record; empty when there is none.
func LoadUpgradeFailures() map[string]UpgradeFailure {
	out := map[string]UpgradeFailure{}
	b, err := os.ReadFile(UpgradeFailuresPath())
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveUpgradeFailures(m map[string]UpgradeFailure) {
	path := UpgradeFailuresPath()
	if len(m) == 0 {
		_ = os.Remove(path)
		return
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("daemon: write %s: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// recordUpgradeFailure counts a failed takeover against build and returns
// the count. Called by the daemon holding the lock, as every writer is.
func recordUpgradeFailure(build string, cause error) int {
	m := LoadUpgradeFailures()
	f := m[build]
	f.Failures++
	f.Error = cause.Error()
	f.At = time.Now().UTC().Format(time.RFC3339)
	m[build] = f
	saveUpgradeFailures(m)
	return f.Failures
}

// clearUpgradeFailures forgets build's failures: it took over.
func clearUpgradeFailures(build string) {
	m := LoadUpgradeFailures()
	if _, ok := m[build]; ok {
		delete(m, build)
		saveUpgradeFailures(m)
	}
}

// upgradeRefused says why an upgrade to build is refused, "" when it is not.
func upgradeRefused(build string) string {
	f, ok := LoadUpgradeFailures()[build]
	if !ok || f.Failures < MaxUpgradeFailures {
		return ""
	}
	return fmt.Sprintf("build %s failed to come up %d times (last: %s); it is refused until %s is removed",
		build, f.Failures, f.Error, UpgradeFailuresPath())
}

// keepPrevExe copies this daemon's binary — the build serving now — to
// exe.prev, before a new build is tried. On Linux it is read through
// /proc/self/exe, which is this process's file even after a rebuild
// renamed another over its path; elsewhere the path is copied only while
// it is still this build.
func keepPrevExe() error {
	src := "/proc/self/exe"
	if runtime.GOOS != "linux" {
		exe, err := selfExe()
		if err != nil {
			return err
		}
		if h, err := HashFile(exe); err != nil || h != ExeHash() {
			return fmt.Errorf("%s is no longer this build", exe)
		}
		src = exe
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dst := PrevExePath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".exe.prev-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o700)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
