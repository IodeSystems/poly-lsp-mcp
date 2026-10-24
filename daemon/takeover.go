package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Blue/green upgrade.
//
// dun's dund restarts in place: it re-execs into the new build at a quiet
// point. poly-lsp cannot afford that: its warm state — the index, the parse
// cache, the child LSPs — IS the product, and takes tens of seconds to
// build again. So the old daemon (blue) starts the new build (green)
// beside it and keeps serving while green warms:
//
//  1. blue saves the shared parse cache and starts green as
//     `<new exe> daemon <blue's flags> --takeover-from <blue pid>
//     --socket <side> --warm <root>...`, passing the daemon lock on fd 3;
//  2. green listens on the side socket and brings up the roots blue used
//     recently — parse cache and index, not the child LSPs, which start on
//     a root's first request as they always do — and says warm in /health;
//  3. blue waits for a quiet point (no open edit batch, bounded), then asks
//     green to take over (POST /takeover). Green renames its side socket
//     over the main path and rewrites daemon.json, holding the lock;
//  4. blue stops its keep-alives and ends its event streams, so its
//     proxies reconnect — to green — drains the requests in flight
//     (bounded), and exits.
//
// Why the socket swap is race-free:
//   - The main path always names a listening socket. rename(2) replaces the
//     directory entry atomically: before it, blue's socket; after it,
//     green's; never none. A connect() resolves the path once and lands in
//     exactly one listener's queue.
//   - Nothing a client sent is dropped. A connect that reached blue did so
//     before the rename, and blue keeps accepting and serving until its
//     drain ends (at least drainMin after the rename, ample for its accept
//     loop to take what is queued). Blue's listener never unlinks the path
//     on close, which would delete green's socket.
//   - Only the lock holder writes daemon.json or binds the main path. The
//     lock is one open file description shared by blue and green from
//     green's start, so it is never free during the swap; blue closes its
//     descriptor without unlocking.
//   - A request on a connection blue closes after the swap fails at the
//     proxy, which retries a read-only call once on the new daemon and
//     never retries an edit blindly (see proxy.go).
//
// Green failing at any step — it exits, is not warm within warmWait, or
// refuses the takeover — leaves blue serving as before: blue kills green,
// takes the main path back if green had renamed over it, and counts the
// failure against that build (see rollback.go).

// Timings, variables so tests can shorten them.
var (
	warmWait  = 3 * time.Minute        // green has this long to warm
	quietWait = 60 * time.Second       // the swap waits this long for open edit batches
	drainMin  = 500 * time.Millisecond // blue serves at least this long after the swap
	drainMax  = 10 * time.Second       // and at most this long for requests in flight
	linger    = warmWait + time.Minute // green exits if no takeover comes by then
)

// defaultWarmRecent: a takeover warms the roots requested within this long.
const defaultWarmRecent = 30 * time.Minute

// sideSocket is where a takeover daemon listens while it warms: beside the
// main socket, because the takeover renames it over the main path and a
// rename is atomic only within one directory. The name is never longer
// than the main socket's, so a main path that fits sun_path gives a side
// path that fits too.
func sideSocket(main string, pid int) string {
	name := ".n" + strconv.FormatInt(int64(pid), 36) + ".s"
	if base := filepath.Base(main); len(name) > len(base) {
		name = name[:len(base)]
	}
	return filepath.Join(filepath.Dir(main), name)
}

// blueGreen is the upgrader's move on the serving daemon: start req.Exe
// as green, wait for it to warm, hand over. It returns nil once green
// serves the main socket; blue then drains and exits.
func (h *host) blueGreen(req UpgradeRequest) error {
	h.mu.Lock()
	main, socket, lock := h.main, h.socket, h.lock
	h.mu.Unlock()
	if !main || lock == nil {
		return errors.New("this daemon does not serve the main socket")
	}

	// Green loads the cache at its start: give it every parse made so far.
	h.reg.SaveCache()
	roots := h.reg.RecentRoots(h.warmRecent)
	side := sideSocket(socket, os.Getpid())
	_ = os.Remove(side)

	args := append([]string{"daemon"}, h.args...)
	args = append(args, "--takeover-from="+strconv.Itoa(os.Getpid()), "--socket="+side)
	for _, r := range roots {
		args = append(args, "--warm="+r)
	}
	logf, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(req.Exe, args...)
	cmd.Env = childEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{lock.file()} // fd 3: the lock (lockFD)
	err = cmd.Start()
	_ = logf.Close()
	if err != nil {
		return fmt.Errorf("start %s: %w", req.Exe, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	log.Printf("daemon: started build %s as pid %d on %s, warming %d root(s)", req.Build, cmd.Process.Pid, side, len(roots))

	fail := func(err error) error {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		_ = os.Remove(side)
		h.reclaimMain()
		return err
	}

	green := NewClient(side)
	deadline := time.Now().Add(warmWait)
	for {
		select {
		case err := <-exited:
			exited <- err // fail waits on it
			return fail(fmt.Errorf("build %s exited before it was warm (%v); see %s", req.Build, err, LogPath()))
		default:
		}
		if hl, err := green.health(); err == nil && hl.Warm {
			if hl.Build != req.Build {
				return fail(fmt.Errorf("the new daemon says build %s, not %s", hl.Build, req.Build))
			}
			break
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("build %s was not warm within %s", req.Build, warmWait))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A quiet point: an open batch lives in this process and the hand-over
	// would roll it back, so give it time to commit or discard.
	quiet := time.Now().Add(quietWait)
	for n := h.reg.OpenBatches(); n > 0; n = h.reg.OpenBatches() {
		if time.Now().After(quiet) {
			log.Printf("daemon: %d edit batch(es) still open after %s; the hand-over rolls them back", n, quietWait)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := green.takeover(socket); err != nil {
		return fail(fmt.Errorf("build %s refused the takeover: %w", req.Build, err))
	}
	h.handOver(req.Build)
	return nil
}

// reclaimMain puts this daemon back on the main path after a failed
// takeover in which green had already renamed its socket over it (the
// reply was lost, or green died after). This daemon still holds the lock.
func (h *host) reclaimMain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.main {
		return
	}
	if fi, err := os.Stat(h.socket); err == nil && os.SameFile(fi, h.ln.bound) {
		return // still ours
	}
	ln, err := listenUnix(h.socket)
	if err != nil {
		log.Printf("daemon: take the main socket back: %v", err)
		return
	}
	go func() { _ = h.srv.Serve(ln) }()
	if _, err := WriteState(h.socket, h.args); err != nil {
		log.Printf("daemon: rewrite state: %v", err)
	}
	log.Printf("daemon: took %s back after the failed takeover", h.socket)
}

// handOver ends this daemon after green took the main socket: its event
// streams end and its keep-alives stop, so every proxy's next request
// dials the main path — green; requests in flight finish (drainMin to
// drainMax); then it exits without touching the socket, daemon.json or
// the parse cache, all green's now.
func (h *host) handOver(to string) {
	h.mu.Lock()
	h.handedOver, h.main = true, false
	socket := h.socket
	h.mu.Unlock()
	h.stopWatch()
	log.Printf("daemon: build %s serves %s now; draining", to, socket)
	close(h.draining)
	h.srv.SetKeepAlivesEnabled(false)
	start := time.Now()
	for time.Since(start) < drainMax && (time.Since(start) < drainMin || h.inflight.Load() > 0) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := h.inflight.Load(); n > 0 {
		log.Printf("daemon: %d request(s) still in flight after %s; exiting", n, drainMax)
	}
	h.shutdown(time.Second, false)
}

// warmRoots brings up the roots the old daemon used recently, in
// parallel, without their child LSPs, and then says warm. A root that
// fails is not warm, and is left for its first request.
func (h *host) warmRoots(roots []string) {
	start := time.Now()
	var wg sync.WaitGroup
	for _, r := range roots {
		canonical, err := h.allow.Resolve(r)
		if err != nil {
			log.Printf("daemon: warm %s: %v", r, err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.reg.get(canonical, "", nil, false)
		}()
	}
	wg.Wait()
	h.warm.Store(true)
	log.Printf("daemon: warm: %d root(s) in %s", len(roots), time.Since(start).Round(time.Millisecond))
}

// awaitTakeover ends a takeover daemon that is never asked to take over —
// its parent died or gave up — instead of leaving it on the side socket.
func (h *host) awaitTakeover() {
	t := time.NewTimer(linger)
	defer t.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-h.tookOver:
			return
		case <-t.C:
			log.Printf("daemon: no takeover from pid %d within %s; exiting", h.takeoverFrom, linger)
		case <-tick.C:
			if PidAlive(h.takeoverFrom) {
				continue
			}
			log.Printf("daemon: pid %d is gone before the takeover; exiting", h.takeoverFrom)
		}
		h.shutdown(time.Second, false)
		return
	}
}

// takeOver is green's side of the swap (POST /takeover, from blue only):
// rename the side socket over the main path, then record itself in
// daemon.json. Both under the daemon lock it inherited.
func (h *host) takeOver(ctx context.Context, socket string) error {
	peer, _ := ctx.Value(peerKey{}).(peerInfo)
	if h.takeoverFrom == 0 || peer.PID != h.takeoverFrom {
		return errors.New("only the daemon being replaced may hand over")
	}
	if !h.warm.Load() {
		return errors.New("not warm yet")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.main {
		return nil
	}
	if filepath.Dir(socket) != filepath.Dir(h.side) {
		return fmt.Errorf("%s is not beside %s", socket, h.side)
	}
	if err := os.Rename(h.side, socket); err != nil {
		return err
	}
	// The flags were recorded without the side --socket; a main socket
	// other than the default is a choice a restart replays.
	if socket != SocketPath() {
		h.args = append(h.args, "--socket="+socket)
	}
	remove, err := WriteState(socket, h.args)
	if err != nil {
		log.Printf("daemon: write state: %v (continuing)", err)
	}
	h.socket, h.main, h.removeState, h.side = socket, true, remove, ""
	close(h.tookOver)
	ctx2, cancel := context.WithCancel(context.Background())
	old := h.stopWatch
	h.stopWatch = func() { cancel(); old() }
	if exe, err := selfExe(); err == nil {
		go watchSelf(ctx2, h.up, exe)
	}
	log.Printf("daemon: took over %s from pid %d (build %s)", socket, h.takeoverFrom, BuildID())
	return nil
}
