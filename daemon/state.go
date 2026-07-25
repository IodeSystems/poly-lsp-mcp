package daemon

import (
	"encoding/json"
	"errors"
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
	Version   string `json:"version"`
}

// WriteState records the running daemon (this process) and returns a
// remover the daemon defers so the file disappears on clean shutdown.
func WriteState(socket string) (remove func(), err error) {
	st := State{
		PID:       os.Getpid(),
		Socket:    socket,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Version:   Version,
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return func() {}, err
	}
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Remove(path) }, nil
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
	return err == nil || errors.Is(err, syscall.EPERM)
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
