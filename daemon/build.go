package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Protocol is the daemon's wire revision: what a proxy may ask of it. It
// moves only when an endpoint or a payload changes meaning; a proxy of a
// different protocol still dials, and the hello handshake decides what it
// can ask for.
const Protocol = 2

// Build identity.
//
// Every binary says which build it is, with no stamp: a build target may
// never stamp the binary (TestNoBuildTargetStampsTheBinary), and dun's
// lesson is that an unstamped dev build that reads as "dev" silently turns
// upgrades off. So the identity is derived from the binary itself:
//
//	<vcs revision, 12>[+dirty]/<sha256 of the executable, 8>
//
// The revision makes it readable; the hash makes it exact. Two different
// binaries from one dirty tree (an edit, a rebuild) differ, which a
// revision alone would hide; a reproducible rebuild of the same source
// gives the same hash and so no needless upgrade. Without VCS settings (a
// `go install pkg@version`) the module version stands in for the revision.

var (
	buildOnce sync.Once
	buildID   string
	exeHash   string
	vcsTime   string
)

// BuildID is this binary's identity; see above.
func BuildID() string {
	buildOnce.Do(computeBuild)
	return buildID
}

// ExeHash is the full sha256 of this process's executable as it was at
// first use, "" when it could not be read.
func ExeHash() string {
	buildOnce.Do(computeBuild)
	return exeHash
}

// VCSTime is the commit time this binary was built from, RFC 3339, or "".
// An upgrade to a binary of an OLDER commit is refused, so two installs on
// one machine cannot hand the daemon back and forth.
func VCSTime() string {
	buildOnce.Do(computeBuild)
	return vcsTime
}

func computeBuild() {
	prefix := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "+dirty"
				}
			}
		}
		switch {
		case rev != "":
			if len(rev) > 12 {
				rev = rev[:12]
			}
			prefix = rev + dirty
		case bi.Main.Version != "":
			prefix = bi.Main.Version
		}
	}
	if exe, err := selfExe(); err == nil {
		exeHash, _ = HashFile(exe)
	}
	short := "nohash"
	if len(exeHash) >= 8 {
		short = exeHash[:8]
	}
	buildID = prefix + "/" + short
}

// selfExe is this process's executable, symlinks resolved. On Linux it is
// read from /proc/self/exe, which Go reports without the " (deleted)" a
// replaced file gets, so it stays the path the binary was started from.
func selfExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r, nil
	}
	return exe, nil
}

// HashFile is the hex sha256 of a file's content.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ProbeBuild runs `exe daemon --build-id` and returns what it prints, ""
// when it does not answer within 10 s. Running the candidate is also the
// first vetting step of an upgrade: a binary that cannot print its build
// will not serve either. A file just renamed into place can be refused for
// a moment ("text file busy") while another fork still holds its
// descriptor, so a failure is asked again twice.
func ProbeBuild(exe string) string {
	for i := 0; ; i++ {
		if v := probeBuildOnce(exe); v != "" || i == 2 {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func probeBuildOnce(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "daemon", "--build-id")
	cmd.Env = childEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// childEnv is the environment for a binary this daemon runs: its own,
// without LSP_REBUILD_PATH. With that set the child would rebuild itself
// from a checkout before it said anything, and the build it then reported
// would not be the file that was vetted.
func childEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "LSP_REBUILD_PATH=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
