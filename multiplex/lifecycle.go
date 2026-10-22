package multiplex

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lifecycle bounds what child LSPs cost while nobody uses them. Every
// field's zero value turns that rule off.
//
// Stopping a child means LSP shutdown+exit, then kill and reap: the
// memory goes back to the OS. (SIGSTOP would not do that.) The next
// RouteByURI for the language starts it again.
type Lifecycle struct {
	// IdleStop stops a child that got no RouteByURI for this long.
	IdleStop time.Duration
	// MaxRSS stops a child whose VmRSS is above this many bytes.
	MaxRSS int64
	// PSIThreshold: when /proc/pressure/memory "some avg10" is at or
	// above this, stop one child per Interval — the one idle longest,
	// then the largest. systemd-oomd acts at 50 by default; act first.
	// Linux only: elsewhere there is no PSI and this rule does nothing.
	PSIThreshold float64
	// Interval between checks. Zero = 10s.
	Interval time.Duration
}

func (l Lifecycle) enabled() bool {
	return l.IdleStop > 0 || l.MaxRSS > 0 || l.PSIThreshold > 0
}

func (l Lifecycle) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return 10 * time.Second
}

// Stop reasons, as logged and reported in ChildStats.LastStop.
const (
	StopIdle     = "idle"
	StopMemory   = "memory"
	StopPressure = "pressure"
)

// ChildStats counts lifecycle events for one language. Crash restarts
// and policy stops are separate: only crashes use up RestartMaxAttempts.
type ChildStats struct {
	Starts        int    `json:"starts"` // lazy wakes after a policy stop
	CrashRestarts int    `json:"crashRestarts"`
	IdleStops     int    `json:"idleStops"`
	MemoryStops   int    `json:"memoryStops"`
	PressureStops int    `json:"pressureStops"`
	LastStop      string `json:"lastStop,omitempty"`
	LastStopAt    string `json:"lastStopAt,omitempty"`  // RFC 3339
	LastStopRSS   int64  `json:"lastStopRSS,omitempty"` // bytes, at stop time
	LastWakeMS    int64  `json:"lastWakeMs,omitempty"`  // spawn+initialize+OnStart
}

// ChildStatus is one language's row in Status.
type ChildStatus struct {
	Language string `json:"language"`
	State    string `json:"state"` // running | stopped | down (crashed, restart pending)
	PID      int    `json:"pid,omitempty"`
	RSS      int64  `json:"rss,omitempty"` // bytes; 0 when not running or unreadable
	IdleSec  int64  `json:"idleSec"`
	ChildStats
}

// slot is per-language lifecycle state. stopped and stats are guarded by
// Manager.mu.
type slot struct {
	wake     sync.Mutex   // held across a start or stop; serializes them
	lastUsed atomic.Int64 // unix nanos of the last RouteByURI
	stopped  bool         // stopped by policy; RouteByURI starts it again
	stats    ChildStats
}

func newSlot() *slot {
	s := &slot{}
	s.touch()
	return s
}

func (s *slot) touch() { s.lastUsed.Store(time.Now().UnixNano()) }

func (s *slot) idle(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, s.lastUsed.Load()))
}

// wake starts a policy-stopped child for name. Returns nil on failure;
// the slot stays stopped so the next RouteByURI tries again.
func (m *Manager) wake(name string, sl *slot) *Child {
	sl.wake.Lock()
	defer sl.wake.Unlock()

	m.mu.RLock()
	child := m.children[name]
	stopped := sl.stopped
	m.mu.RUnlock()
	if child != nil && alive(child) {
		sl.touch()
		return child // another caller woke it while we waited
	}
	if !stopped || m.stopping() {
		return nil
	}
	lang := m.registry.LookupByName(name)
	if lang == nil || lang.LSP == nil {
		return nil
	}

	start := time.Now()
	child, err := Spawn(name, lang.LSP, m.startCwd)
	if err != nil {
		log.Printf("multiplex: wake %s: spawn: %v", name, err)
		return nil
	}
	m.diagnostics.Attach(child)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	caps, err := child.Initialize(ctx, m.startRootURI)
	cancel()
	if err != nil {
		log.Printf("multiplex: wake %s: initialize: %v", name, err)
		_ = child.Kill()
		_ = child.Wait()
		return nil
	}

	m.mu.Lock()
	if m.stopping() {
		m.mu.Unlock()
		_ = child.Kill()
		_ = child.Wait()
		return nil
	}
	m.children[name] = child
	m.caps[name] = caps
	sl.stopped = false
	sl.stats.Starts++
	m.mu.Unlock()
	go m.watch(name, child)

	if m.OnStart != nil {
		m.OnStart(name, child)
	}
	took := time.Since(start)
	m.mu.Lock()
	sl.stats.LastWakeMS = took.Milliseconds()
	m.mu.Unlock()
	sl.touch()
	log.Printf("multiplex: %s started on demand (pid %d) in %s", name, child.Pid(), took.Round(time.Millisecond))
	m.writeStatus()
	return child
}

// stopChild stops the running child for name for a policy reason. It
// returns false if the slot is busy (a wake in progress) or nothing runs.
func (m *Manager) stopChild(name, reason string, rss int64) bool {
	m.mu.RLock()
	sl := m.slots[name]
	m.mu.RUnlock()
	if sl == nil || !sl.wake.TryLock() {
		return false
	}
	defer sl.wake.Unlock()

	idle := sl.idle(time.Now())
	m.mu.Lock()
	child := m.children[name]
	if child == nil {
		m.mu.Unlock()
		return false
	}
	// Remove first: the watchdog sees it is no longer current and does
	// not treat the exit as a crash. Capabilities stay; the server
	// already advertised them.
	delete(m.children, name)
	sl.stopped = true
	switch reason {
	case StopIdle:
		sl.stats.IdleStops++
	case StopMemory:
		sl.stats.MemoryStops++
	case StopPressure:
		sl.stats.PressureStops++
	}
	sl.stats.LastStop = reason
	sl.stats.LastStopAt = time.Now().Format(time.RFC3339)
	sl.stats.LastStopRSS = rss
	m.mu.Unlock()

	log.Printf("multiplex: stopping %s (pid %d): reason=%s rss=%s idle=%s",
		name, child.Pid(), reason, fmtBytes(rss), idle.Round(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := child.Shutdown(ctx); err != nil {
		log.Printf("multiplex: stop %s: shutdown: %v; killing", name, err)
	}
	cancel()
	_ = child.Reap(2 * time.Second) // the watchdog reaps too; Wait is idempotent
	m.writeStatus()
	return true
}

// monitor applies Lifecycle every Interval until Shutdown.
func (m *Manager) monitor() {
	t := time.NewTicker(m.Lifecycle.interval())
	defer t.Stop()
	for {
		select {
		case <-m.monitorStop:
			return
		case <-t.C:
			m.checkLifecycle()
			m.writeStatus()
		}
	}
}

type candidate struct {
	name string
	idle time.Duration
	rss  int64
}

// checkLifecycle runs one pass of the policy. Exposed to tests through
// the package; the monitor calls it on a ticker.
func (m *Manager) checkLifecycle() {
	lc := m.Lifecycle
	now := time.Now()

	m.mu.RLock()
	var cands []candidate
	pids := map[string]int{}
	for name, child := range m.children {
		sl := m.slots[name]
		if sl == nil || !alive(child) {
			continue
		}
		cands = append(cands, candidate{name: name, idle: sl.idle(now)})
		pids[name] = child.Pid()
	}
	m.mu.RUnlock()

	var left []candidate
	for _, c := range cands {
		c.rss, _ = m.rssOf(pids[c.name])
		switch {
		case lc.IdleStop > 0 && c.idle >= lc.IdleStop:
			m.stopChild(c.name, StopIdle, c.rss)
		case lc.MaxRSS > 0 && c.rss > lc.MaxRSS:
			m.stopChild(c.name, StopMemory, c.rss)
		default:
			left = append(left, c)
		}
	}

	if lc.PSIThreshold <= 0 || len(left) == 0 {
		return
	}
	read := m.readPSI
	if read == nil {
		read = readMemoryPSI
	}
	avg10, err := read()
	if err != nil || avg10 < lc.PSIThreshold {
		return
	}
	// Idle longest first (minute resolution, so near-ties fall to
	// size), then largest.
	sort.Slice(left, func(i, j int) bool {
		mi, mj := left[i].idle/time.Minute, left[j].idle/time.Minute
		if mi != mj {
			return mi > mj
		}
		return left[i].rss > left[j].rss
	})
	v := left[0]
	log.Printf("multiplex: memory pressure some avg10=%.2f >= %.2f", avg10, lc.PSIThreshold)
	m.stopChild(v.name, StopPressure, v.rss)
}

func (m *Manager) rssOf(pid int) (int64, error) {
	if pid == 0 {
		return 0, nil
	}
	if m.readRSS != nil {
		return m.readRSS(pid)
	}
	return readVmRSS(pid)
}

// Status reports every language the manager has started: pid, RSS,
// idle time and lifecycle counters.
func (m *Manager) Status() []ChildStatus {
	now := time.Now()
	m.mu.RLock()
	out := make([]ChildStatus, 0, len(m.slots))
	pids := make([]int, 0, len(m.slots))
	for name, sl := range m.slots {
		st := ChildStatus{Language: name, IdleSec: int64(sl.idle(now) / time.Second), ChildStats: sl.stats}
		pid := 0
		switch child := m.children[name]; {
		case child != nil && alive(child):
			st.State = "running"
			pid = child.Pid()
			st.PID = pid
		case sl.stopped:
			st.State = "stopped"
		default:
			st.State = "down"
		}
		out = append(out, st)
		pids = append(pids, pid)
	}
	m.mu.RUnlock()
	for i := range out {
		out[i].RSS, _ = m.rssOf(pids[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out
}

func parsePSISomeAvg10(s string) (float64, error) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	return 0, fmt.Errorf("no some avg10 in %q", s)
}

func fmtBytes(n int64) string {
	switch {
	case n <= 0:
		return "?"
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%dM", n>>20)
	}
}
