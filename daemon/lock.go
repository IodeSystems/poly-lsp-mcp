package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock is the daemon lock (~/.poly-lsp/daemon.lock): an flock, not a
// pidfile. The serving daemon holds it for its whole life, so two daemons
// started at once cannot both serve — the second one's TryLock fails and it
// exits — and a daemon killed with -9 releases it with its last descriptor,
// where a pidfile would be left naming a dead (or reused) pid. A takeover
// moves it: the old daemon releases it, the new one takes it, and only the
// holder rewrites daemon.json or the main socket path.
type Lock struct{ f *os.File }

// LockPath is the daemon lock file.
func LockPath() string { return filepath.Join(ConfigHome(), "daemon.lock") }

// ErrLocked is TryLock's answer when another process holds the lock.
var ErrLocked = errors.New("daemon lock is held by another process")

// TryLock takes the daemon lock without waiting.
func TryLock() (*Lock, error) { return lockPath(LockPath(), 0) }

// WaitLock takes the daemon lock, waiting up to d for its holder.
func WaitLock(d time.Duration) (*Lock, error) { return lockPath(LockPath(), d) }

func lockPath(path string, wait time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// O_CLOEXEC is Go's default: a child this daemon starts (a takeover)
	// does not inherit the descriptor, and so does not hold the lock by
	// accident after its parent let go.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrLocked
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Release lets go of the lock. Safe on nil and more than once.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
