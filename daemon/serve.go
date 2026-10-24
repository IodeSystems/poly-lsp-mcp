package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config parameterizes the daemon server.
type Config struct {
	Socket string     // listen path; "" = SocketPath(). A takeover daemon's side socket.
	Allow  *AllowList // declared root prefixes a client may address
	Reg    *Registry  // the per-root server host
	Args   []string   // the flags it was started with, recorded in daemon.json

	// TakeoverFrom is the pid of the daemon this one is to replace (blue/
	// green; see takeover.go), 0 for a daemon started on its own. Warm are
	// the roots to bring up before taking over.
	TakeoverFrom int
	Warm         []string
	// WarmRecent: the roots requested within this long are the ones a
	// takeover warms. 0 = the default.
	WarmRecent time.Duration
}

// host is one daemon process's serving state, shared by its handlers.
type host struct {
	allow *AllowList
	reg   *Registry
	up    *upgrader
	args  []string

	srv *http.Server
	ln  *credListener

	// mu guards socket, main, lock, removeState and handedOver.
	mu sync.Mutex
	// socket is the main socket path once this daemon serves it; a
	// takeover daemon learns it in /takeover.
	socket string
	// main: this daemon owns the main socket and daemon.json.
	main bool
	// lock is the daemon lock. A takeover daemon inherits it (fd 3) from
	// the daemon it replaces; see takeover.go.
	lock        *Lock
	removeState func()
	handedOver  bool

	// takeoverFrom / side: set on a takeover daemon until it takes over.
	takeoverFrom int
	side         string
	warm         atomic.Bool
	tookOver     chan struct{}

	// warmRecent is how recently a root must have been used for a
	// takeover to warm it.
	warmRecent time.Duration

	// draining is closed when this daemon has handed over: event streams
	// end, so their proxies reconnect to the new daemon.
	draining  chan struct{}
	inflight  atomic.Int64
	stopWatch context.CancelFunc
}

// Serve runs the daemon: it listens on the unix socket (peer-cred gated),
// serves the gat/huma handler, records the state file, and blocks until
// SIGINT/SIGTERM or a takeover by a new build, then tears down hosted
// servers gracefully. It is the process `poly-lsp-mcp daemon` becomes.
func Serve(cfg Config) error {
	h := &host{
		allow: cfg.Allow, reg: cfg.Reg, args: cfg.Args,
		takeoverFrom: cfg.TakeoverFrom,
		warmRecent:   cfg.WarmRecent,
		draining:     make(chan struct{}),
		tookOver:     make(chan struct{}),
	}
	if h.warmRecent <= 0 {
		h.warmRecent = defaultWarmRecent
	}
	socket := cfg.Socket
	if socket == "" {
		socket = SocketPath()
	}

	if h.takeoverFrom != 0 {
		// A takeover daemon serves a side socket until it takes the main
		// one, and holds the lock its parent passed it.
		lock, err := inheritedLock()
		if err != nil {
			return fmt.Errorf("daemon: takeover: %w", err)
		}
		h.lock, h.side = lock, socket
	} else {
		// The lock first: of two daemons started at once, the loser stops
		// here, before it touches the socket or daemon.json, and its
		// starter's poll finds the winner.
		lock, err := TryLock()
		if err != nil {
			if errors.Is(err, ErrLocked) {
				if st, ok := ReadState(); ok {
					return fmt.Errorf("daemon: already running (pid %d, %s)", st.PID, st.Socket)
				}
				return fmt.Errorf("daemon: already running (%s is held)", LockPath())
			}
			return fmt.Errorf("daemon: lock: %w", err)
		}
		h.lock = lock
	}
	defer h.releaseLock()

	ln, err := listenUnix(socket)
	if err != nil {
		return err
	}
	h.ln = ln

	// Seed the shared cache before serving so the first root opens warm.
	cfg.Reg.LoadCache()

	h.up = newUpgrader(h.blueGreen)
	handler, err := h.handler()
	if err != nil {
		_ = ln.Close()
		return err
	}
	h.srv = &http.Server{
		Handler: h.track(handler),
		// Stash the accepted conn's peer creds for per-connection policy.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, pid, err := peerCred(c)
			if err != nil {
				return ctx
			}
			return context.WithValue(ctx, peerKey{}, peerInfo{UID: uid, PID: pid})
		},
	}
	ctx, stopWatch := context.WithCancel(context.Background())
	h.stopWatch = stopWatch
	defer stopWatch()

	if h.takeoverFrom != 0 {
		log.Printf("daemon: takeover of pid %d: warming %d root(s) on %s (pid %d, build %s)", h.takeoverFrom, len(cfg.Warm), socket, os.Getpid(), BuildID())
		go h.warmRoots(cfg.Warm)
		go h.awaitTakeover()
	} else {
		h.becomeMain(ctx, socket)
		log.Printf("daemon: listening on %s (pid %d, build %s, allow %v)", socket, os.Getpid(), BuildID(), cfg.Allow.Prefixes())
	}

	// Graceful shutdown: SIGTERM is what --stop/--restart send.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		log.Print("daemon: shutting down")
		h.shutdown(5*time.Second, true)
	}()

	if err := h.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	// Serve returns as soon as the listener closes; the hosted servers'
	// teardown runs in shutdown, which closed it. Wait for that to end.
	h.mu.Lock()
	h.mu.Unlock()
	h.cleanup()
	return nil
}

// becomeMain makes this daemon the one daemon.json names and starts its
// self-watch. Called holding the lock, with socket the main path.
func (h *host) becomeMain(ctx context.Context, socket string) {
	remove, err := WriteState(socket, h.args)
	if err != nil {
		log.Printf("daemon: write state: %v (continuing)", err)
	}
	h.mu.Lock()
	h.socket, h.main, h.removeState = socket, true, remove
	h.mu.Unlock()
	if exe, err := selfExe(); err == nil {
		go watchSelf(ctx, h.up, exe)
	}
}

// shutdown stops serving and tears down the hosted roots. save persists
// the shared parse cache — not after a hand-over, when the new daemon owns
// it.
func (h *host) shutdown(grace time.Duration, save bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reg.Close()
	if save {
		h.reg.SaveCache()
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = h.srv.Shutdown(sctx)
}

// cleanup removes what this daemon owns on its way out: daemon.json and
// the main socket, under the lock — so never a path another daemon now
// serves. A daemon that handed over owns neither.
func (h *host) cleanup() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handedOver || !h.main {
		if h.side != "" && !h.main {
			_ = os.Remove(h.side)
		}
		return
	}
	if st, ok := ReadState(); !ok || st.PID == os.Getpid() {
		_ = os.Remove(h.socket)
	}
	if h.removeState != nil {
		h.removeState()
	}
}

// releaseLock lets go of the daemon lock at exit. After a hand-over this
// daemon's descriptor is only closed: the lock is the new daemon's now,
// and an unlock would release it for both.
func (h *host) releaseLock() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lock == nil {
		return
	}
	// Unlock only as the main daemon. One that handed over, or a takeover
	// daemon that never took over, shares the description with the daemon
	// that holds the lock now.
	if h.main && !h.handedOver {
		h.lock.Release()
	} else {
		h.lock.closeOnly()
	}
	h.lock = nil
}

// track counts in-flight requests, for the drain after a hand-over. The
// long-lived session streams are not requests in flight: they end when
// draining closes.
func (h *host) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/session/") {
			next.ServeHTTP(w, r)
			return
		}
		h.inflight.Add(1)
		defer h.inflight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// listenUnix creates the socket with tight permissions: dir 0700, socket
// 0600. A stale socket file whose owner is gone is unlinked and replaced
// (the raglit "drop a stale daemon.json" move, applied to the socket).
// The listener never unlinks its path on close: after a takeover the path
// names the new daemon's socket (cleanup removes it, under the lock, when
// it is still ours).
func listenUnix(socket string) (*credListener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: socket dir: %w", err)
	}
	// If something is already listening, refuse; if it's a stale file,
	// remove it and take over.
	if _, err := os.Stat(socket); err == nil {
		if c, derr := net.Dial("unix", socket); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("daemon: socket %s is already served (another daemon is running)", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("daemon: remove stale socket: %w", err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("daemon: listen %s: %w", socket, err)
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(socket)
		return nil, fmt.Errorf("daemon: chmod socket: %w", err)
	}
	fi, err := os.Stat(socket)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: stat socket: %w", err)
	}
	// Reject any connecting uid but our own at accept — redundant with
	// 0600 in the normal case, which is the point: it still holds if the
	// mode bits are ever wrong.
	return &credListener{Listener: ln, selfUID: os.Getuid(), bound: fi}, nil
}
