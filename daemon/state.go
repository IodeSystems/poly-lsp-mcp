package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// State is the daemon discovery record persisted at StatePath. Ported
// from raglit's daemonState; "socket" replaces raglit's TCP "addr".
type State struct {
	PID       int    `json:"pid"`
	Socket    string `json:"socket"`
	StartedAt string `json:"started_at"`
	// Build is the daemon binary's identity (BuildID); Protocol its wire
	// revision. A client compares Build with its own to see a stale
	// daemon without dialing it.
	Build    string `json:"build"`
	Protocol int    `json:"protocol"`
	// Exe is the binary the daemon runs, for doctor and for the recover
	// hint after a failed upgrade.
	Exe string `json:"exe,omitempty"`
}

// WriteState records the running daemon (this process) and returns a
// remover the daemon defers so the file disappears on clean shutdown.
// Call it holding the daemon lock: only the lock holder owns the file.
// Written to a temp file and renamed, so a reader never sees half of it.
func WriteState(socket string) (remove func(), err error) {
	exe, _ := selfExe()
	st := State{
		PID:       os.Getpid(),
		Socket:    socket,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Build:     BuildID(),
		Protocol:  Protocol,
		Exe:       exe,
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return func() {}, err
	}
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}, err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return func() {}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return func() {}, err
	}
	return func() { removeOwnState() }, nil
}

// removeOwnState deletes daemon.json only while it still names this
// process. After a takeover the file names the new daemon, and the old
// one's exit must not erase it.
func removeOwnState() {
	if st, ok := ReadState(); ok && st.PID != os.Getpid() {
		return
	}
	_ = os.Remove(StatePath())
}

// ReadState loads the discovery record. ok is false on any error or an
// empty socket field (a half-written or truncated file reads as absent).
func ReadState() (st State, ok bool) {
	b, err := os.ReadFile(StatePath())
	if err != nil {
		return State{}, false
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Socket == "" {
		return State{}, false
	}
	return st, true
}

// PidAlive reports whether pid names a live process. Signal 0 probes
// without delivering: nil = alive, EPERM = alive-but-not-ours, ESRCH =
// gone. Used only to decide whether to TRUST the state file — a live
// health probe over the socket is the authority on "is it up".
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	// kill(pid, 0) succeeds on a zombie: the process has exited and only
	// waits for its parent to reap it. Count that as gone, or Restart
	// waits its full timeout for a daemon that already exited.
	return !pidZombie(pid)
}

// RemoveStaleState deletes the discovery file when its pid is gone.
// Returns true if it removed one.
func RemoveStaleState(st State) bool {
	if PidAlive(st.PID) {
		return false
	}
	_ = os.Remove(StatePath())
	return true
}

// WaitPidGone polls until pid exits or the timeout elapses. Reports
// whether it is gone.
func WaitPidGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !PidAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !PidAlive(pid)
}
