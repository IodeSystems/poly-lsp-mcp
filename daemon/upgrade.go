package daemon

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// Upgrades. A daemon outlives the binary it was started from: `make
// install` or `go install` puts a new build in place while it serves. Two
// things notice. A proxy of another build says so in its hello and asks
// the daemon to move to the proxy's binary (requestUpgrade); and the
// daemon watches its own executable (watchSelf) and asks itself when the
// file is replaced. Either way the request lands here, is vetted, and runs
// the move (upgrader.run) once at a time.

// UpgradeRequest is POST /upgrade, and what watchSelf asks of itself.
type UpgradeRequest struct {
	Exe   string `json:"exe"`               // the binary to move to
	Build string `json:"build"`             // the build the asker says it is
	Why   string `json:"why"`               // for the log and the notice
	Time  string `json:"vcsTime,omitempty"` // its commit time, when it knows it
}

// UpgradeAnswer is the /upgrade reply.
type UpgradeAnswer struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"` // why not, when not
}

type upgrader struct {
	mu      sync.Mutex
	pending *UpgradeRequest // the move in progress, nil when none
	// run performs a vetted move; it returns when it ended, nil on success.
	run func(UpgradeRequest) error
	// probe asks a binary its build (ProbeBuild; a variable for tests).
	probe func(exe string) string
	// selfTime is this daemon's commit time (VCSTime; a field for tests).
	selfTime string
	// refused says why a build is refused after failing before, "" when
	// it is not (upgradeRefused; see rollback.go).
	refused func(build string) string
}

func newUpgrader(run func(UpgradeRequest) error) *upgrader {
	return &upgrader{run: run, probe: ProbeBuild, selfTime: VCSTime(), refused: upgradeRefused}
}

// request vets an upgrade and starts it. Refused: the same build, a move
// already running, an older commit than this one (so two installs on one
// machine cannot hand the daemon back and forth), a build that already
// failed to take over MaxUpgradeFailures times, a binary that is not
// there. The candidate is then run (`daemon --build-id`) in the
// background, and the move starts only if it says the build it was
// claimed to be.
func (u *upgrader) request(req UpgradeRequest) UpgradeAnswer {
	if req.Build == BuildID() {
		return UpgradeAnswer{Reason: "already this build"}
	}
	if req.Time != "" && u.selfTime != "" && req.Time < u.selfTime {
		return UpgradeAnswer{Reason: "build " + req.Build + " is an older commit (" + req.Time + ") than this daemon's (" + u.selfTime + ")"}
	}
	if why := u.refused(req.Build); why != "" {
		return UpgradeAnswer{Reason: why}
	}
	if st, err := os.Stat(req.Exe); err != nil || !st.Mode().IsRegular() {
		return UpgradeAnswer{Reason: "no binary at " + req.Exe}
	}
	u.mu.Lock()
	if u.pending != nil {
		to := u.pending.Build
		u.mu.Unlock()
		return UpgradeAnswer{Reason: "already upgrading to " + to}
	}
	r := req
	u.pending = &r
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.pending = nil
			u.mu.Unlock()
		}()
		got := u.probe(req.Exe)
		if got == "" {
			log.Printf("daemon: upgrade to %s refused: %s did not say which build it is", req.Build, req.Exe)
			return
		}
		if req.Build != "" && got != req.Build {
			log.Printf("daemon: upgrade refused: %s is build %s, not %s (replaced meanwhile?)", req.Exe, got, req.Build)
			return
		}
		if got == BuildID() {
			return
		}
		if why := u.refused(got); why != "" {
			log.Printf("daemon: upgrade refused: %s", why)
			return
		}
		req.Build = got
		log.Printf("daemon: upgrading %s → %s (%s), from %s", BuildID(), req.Build, req.Why, req.Exe)
		if err := u.run(req); err != nil {
			log.Printf("daemon: upgrade to %s failed: %v", req.Build, err)
		}
	}()
	return UpgradeAnswer{Accepted: true}
}

// selfCheck is how often the daemon looks at its own binary.
var selfCheck = 2 * time.Second

// watchSelf asks for an upgrade each time this daemon's executable is
// replaced by another build (a rebuild renames a new binary over it),
// until ctx ends. A stat every selfCheck; the file is hashed only when the
// stat changed, and run only when the hash is not this binary's.
func watchSelf(ctx context.Context, u *upgrader, exe string) {
	last, err := os.Stat(exe)
	if err != nil {
		return
	}
	t := time.NewTicker(selfCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := os.Stat(exe)
		if err != nil || os.SameFile(last, now) && now.ModTime().Equal(last.ModTime()) && now.Size() == last.Size() {
			continue
		}
		last = now
		if h, err := HashFile(exe); err != nil || h == ExeHash() {
			continue
		}
		build := u.probe(exe)
		if build == "" || build == BuildID() {
			continue
		}
		log.Printf("daemon: a new build (%s) is in place at %s", build, exe)
		if a := u.request(UpgradeRequest{Exe: exe, Build: build, Why: "its binary was replaced"}); !a.Accepted {
			log.Printf("daemon: not upgrading: %s", a.Reason)
		}
	}
}

// buildAction is what a proxy does about a daemon of another build.
type buildAction int

const (
	buildsAgree   buildAction = iota // same build: nothing
	proxyIsStale                     // the proxy's own file is now the daemon's build: nothing to ask
	upgradeDaemon                    // the proxy's binary is the newer one in place: ask the daemon to move to it
	buildsDiffer                     // three builds: say so, once
)

// buildDecision is dun's: mine is this proxy's build, daemon the daemon's,
// onDisk what this proxy's executable file is now ("" when it would not
// say).
func buildDecision(mine, daemon, onDisk string) buildAction {
	switch {
	case mine == daemon || daemon == "":
		return buildsAgree
	case onDisk == daemon:
		return proxyIsStale
	case onDisk == mine || onDisk == "":
		return upgradeDaemon
	}
	return buildsDiffer
}
