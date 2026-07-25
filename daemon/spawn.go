package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// StartDetached re-execs this binary as a background daemon, detached
// from the current session (Setsid), stdout+stderr to daemon.log. args
// are the daemon subcommand's flags to replay (e.g. --allow, --socket).
func StartDetached(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigHome(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(exe, append([]string{"daemon"}, args...)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // new session — survives our exit
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// EnsureRunning returns a client to a healthy daemon, auto-starting one
// detached if none is up. args replay into the spawned daemon. The poll
// loop is flat 150ms/8s, matching raglit; concurrent starters lose the
// socket-bind race and the loser's poll finds the winner.
func EnsureRunning(args []string) (*Client, error) {
	c := NewClient(daemonSocket())
	if c.Healthy() {
		return c, nil
	}
	if err := StartDetached(args); err != nil {
		return nil, fmt.Errorf("auto-start daemon: %w", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if c.Healthy() {
			return c, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, fmt.Errorf("auto-started daemon did not come up on %s (see %s)", c.socket, LogPath())
}

// daemonSocket returns the socket to dial: the state file's recorded
// socket if a daemon is recorded, else the default path (so a first-ever
// client still has somewhere to auto-start).
func daemonSocket() string {
	if st, ok := ReadState(); ok {
		return st.Socket
	}
	return SocketPath()
}

// Stop signals the recorded daemon to shut down (SIGTERM) and reports it.
// Fire-and-report — waiting for exit is Restart's job.
func Stop() error {
	st, ok := ReadState()
	if !ok {
		return fmt.Errorf("no daemon state at %s", StatePath())
	}
	if !PidAlive(st.PID) {
		RemoveStaleState(st)
		return fmt.Errorf("daemon pid %d not running — removed stale %s", st.PID, StatePath())
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon pid %d: %w", st.PID, err)
	}
	fmt.Printf("stopped daemon pid %d (%s)\n", st.PID, st.Socket)
	return nil
}

// Restart stops a running daemon (SIGTERM, wait for actual exit) then
// relaunches detached replaying args. If no daemon is running it just
// starts one.
func Restart(args []string) error {
	if st, ok := ReadState(); ok && PidAlive(st.PID) {
		if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
			return fmt.Errorf("signal daemon pid %d: %w", st.PID, err)
		}
		if !WaitPidGone(st.PID, 15*time.Second) {
			return fmt.Errorf("daemon pid %d did not exit within 15s", st.PID)
		}
	} else if ok {
		RemoveStaleState(st)
	}
	if err := StartDetached(args); err != nil {
		return fmt.Errorf("relaunch daemon: %w", err)
	}
	c := NewClient(SocketPath())
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c.Healthy() {
			if st, ok := ReadState(); ok {
				fmt.Printf("restarted daemon pid %d (%s)\n", st.PID, st.Socket)
			}
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("restarted daemon did not come up (see %s)", LogPath())
}

// probeHealth is the shared 1s GET /health used by Client.Healthy and the
// poll loops.
func probeHealth(hc *http.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://poly-lsp/health", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
