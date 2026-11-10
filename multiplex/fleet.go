package multiplex

import (
	"log"
	"sort"
	"sync"
	"time"
)

// Fleet applies the process-wide child-LSP rules across every Manager
// that shares it: the daemon makes one and gives it to each root's
// Manager; a Manager with no Fleet makes its own at Start.
//
//   - MaxChildren: before a child starts (Manager.Start, wake), the least
//     recently used running child of any Manager is stopped (reason "cap")
//     until the new one fits.
//   - RSSBudgetPct: every Interval, while the summed child RSS is above
//     the budget, the largest child is stopped (reason "budget").
//   - PSIThreshold, MacPressureLevel: every Interval, at most one child is
//     stopped for memory pressure for the whole fleet (reason "pressure").
//
// A child with a request in flight is never chosen. If every running
// child is busy, a new child is admitted over the cap and that is logged.
type Fleet struct {
	lc       Lifecycle
	totalRAM int64 // bytes, 0 = unknown (the budget rule is then off)

	// Test seams; nil = the platform reads.
	readRSS      func(pid int) (int64, error)
	readPSI      func() (float64, error)
	readPressure func() (int, error)

	admitMu sync.Mutex // serializes admission: count, evict, reserve

	mu       sync.Mutex
	managers map[*Manager]struct{}
	reserved int   // admitted children still in spawn+initialize
	lastRSS  int64 // summed child RSS at the last check

	startOnce sync.Once
	closeOnce sync.Once
	stop      chan struct{}
}

// NewFleet returns a Fleet that applies lc's process-wide rules.
func NewFleet(lc Lifecycle) *Fleet {
	return &Fleet{
		lc:       lc,
		totalRAM: TotalRAM(),
		managers: map[*Manager]struct{}{},
		stop:     make(chan struct{}),
	}
}

// FleetStatus is the fleet's row in a StatusFile.
type FleetStatus struct {
	MaxChildren int   `json:"maxChildren"` // 0 = no cap
	Running     int   `json:"running"`     // running children across every Manager
	RSS         int64 `json:"rss"`         // summed child RSS in bytes, at the last check
	RSSBudget   int64 `json:"rssBudget,omitempty"`
}

// Status reports the cap, the running count and the summed RSS.
func (f *Fleet) Status() FleetStatus {
	f.mu.Lock()
	rss := f.lastRSS
	f.mu.Unlock()
	return FleetStatus{
		MaxChildren: f.lc.MaxChildren,
		Running:     len(f.members()),
		RSS:         rss,
		RSSBudget:   f.budget(),
	}
}

// Close stops the fleet's monitor. Safe on nil and to call twice.
func (f *Fleet) Close() {
	if f == nil {
		return
	}
	f.closeOnce.Do(func() { close(f.stop) })
}

func (f *Fleet) add(m *Manager) {
	f.mu.Lock()
	f.managers[m] = struct{}{}
	f.mu.Unlock()
	f.startOnce.Do(func() { go f.monitor() })
}

func (f *Fleet) remove(m *Manager) {
	f.mu.Lock()
	delete(f.managers, m)
	f.mu.Unlock()
}

func (f *Fleet) budget() int64 {
	if f.lc.RSSBudgetPct <= 0 || f.totalRAM <= 0 {
		return 0
	}
	return f.totalRAM / 100 * int64(f.lc.RSSBudgetPct)
}

// member is one running child of one Manager.
type member struct {
	m     *Manager
	k     childKey
	child *Child
	used  int64 // slot.lastUsed
	idle  time.Duration
	rss   int64
}

type memberKey struct {
	m *Manager
	k childKey
}

// members lists the running children of every Manager. f.mu is not held
// while a Manager's lock is taken.
func (f *Fleet) members() []member {
	f.mu.Lock()
	ms := make([]*Manager, 0, len(f.managers))
	for m := range f.managers {
		ms = append(ms, m)
	}
	f.mu.Unlock()
	now := time.Now()
	var out []member
	for _, m := range ms {
		m.mu.RLock()
		for k, c := range m.children {
			sl := m.slots[k]
			if sl == nil || !alive(c) {
				continue
			}
			out = append(out, member{m: m, k: k, child: c, used: sl.lastUsed.Load(), idle: sl.idle(now)})
		}
		m.mu.RUnlock()
	}
	return out
}

func (f *Fleet) rssOf(pid int) int64 {
	if pid == 0 {
		return 0
	}
	read := f.readRSS
	if read == nil {
		read = readVmRSS
	}
	n, _ := read(pid)
	return n
}

// admit makes room under MaxChildren for the child k of self, which is
// about to start, and reserves its place. Call release once the child is
// in self's table or failed to start.
func (f *Fleet) admit(self *Manager, k childKey) (release func()) {
	if f == nil || f.lc.MaxChildren <= 0 {
		return func() {}
	}
	f.admitMu.Lock()
	defer f.admitMu.Unlock()
	skip := map[memberKey]bool{}
	for {
		var ms []member
		for _, mb := range f.members() {
			if mb.m != self || mb.k != k {
				ms = append(ms, mb)
			}
		}
		f.mu.Lock()
		n := len(ms) + f.reserved
		f.mu.Unlock()
		if n < f.lc.MaxChildren {
			break
		}
		var v *member
		for i := range ms {
			mb := &ms[i]
			if skip[memberKey{mb.m, mb.k}] || mb.child.inFlight() {
				continue
			}
			if v == nil || mb.used < v.used {
				v = mb
			}
		}
		if v == nil {
			log.Printf("multiplex: %s at %s: %d children running, cap %d: all busy; starting over the cap", k.lang, k.dir, n, f.lc.MaxChildren)
			break
		}
		if !v.m.stopChild(v.k, StopCap, f.rssOf(v.child.Pid())) {
			skip[memberKey{v.m, v.k}] = true
		}
	}
	f.mu.Lock()
	f.reserved++
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.reserved--
			f.mu.Unlock()
		})
	}
}

// monitor runs check every Interval until Close.
func (f *Fleet) monitor() {
	t := time.NewTicker(f.lc.interval())
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			f.check()
		}
	}
}

// check runs one pass of the budget and pressure rules, and records the
// summed RSS for Status.
func (f *Fleet) check() {
	ms := f.members()
	var sum int64
	for i := range ms {
		ms[i].rss = f.rssOf(ms[i].child.Pid())
		sum += ms[i].rss
	}
	f.mu.Lock()
	f.lastRSS = sum
	f.mu.Unlock()

	// Budget: the largest first, until under it.
	if budget := f.budget(); budget > 0 && sum > budget {
		log.Printf("multiplex: children RSS %s > budget %s (%d%% of RAM)", fmtBytes(sum), fmtBytes(budget), f.lc.RSSBudgetPct)
		sort.Slice(ms, func(i, j int) bool { return ms[i].rss > ms[j].rss })
		var left []member
		for _, mb := range ms {
			if sum > budget && !mb.child.inFlight() && mb.m.stopChild(mb.k, StopBudget, mb.rss) {
				sum -= mb.rss
				continue
			}
			left = append(left, mb)
		}
		ms = left
		f.mu.Lock()
		f.lastRSS = sum
		f.mu.Unlock()
	}

	var idle []member
	for _, mb := range ms {
		if !mb.child.inFlight() {
			idle = append(idle, mb)
		}
	}
	if len(idle) == 0 {
		return
	}
	// Pressure: one stop per check for the whole fleet.
	if f.lc.PSIThreshold > 0 {
		read := f.readPSI
		if read == nil {
			read = readMemoryPSI
		}
		if avg10, err := read(); err == nil && avg10 >= f.lc.PSIThreshold {
			// Idle longest first (minute resolution, so near-ties fall
			// to size), then largest.
			sort.Slice(idle, func(i, j int) bool {
				mi, mj := idle[i].idle/time.Minute, idle[j].idle/time.Minute
				if mi != mj {
					return mi > mj
				}
				return idle[i].rss > idle[j].rss
			})
			log.Printf("multiplex: memory pressure some avg10=%.2f >= %.2f", avg10, f.lc.PSIThreshold)
			if f.stopFirst(idle) {
				return
			}
		}
	}
	if f.lc.MacPressureLevel > 0 {
		read := f.readPressure
		if read == nil {
			read = readMacPressure
		}
		if level, err := read(); err == nil && level >= f.lc.MacPressureLevel {
			sort.Slice(idle, func(i, j int) bool { return idle[i].rss > idle[j].rss })
			log.Printf("multiplex: memory pressure level %d >= %d", level, f.lc.MacPressureLevel)
			f.stopFirst(idle)
		}
	}
}

// stopFirst stops the first of ms that can be stopped, for pressure.
func (f *Fleet) stopFirst(ms []member) bool {
	for _, mb := range ms {
		if mb.m.stopChild(mb.k, StopPressure, mb.rss) {
			return true
		}
	}
	return false
}

// inFlight reports whether c has a request waiting for its response.
func (c *Child) inFlight() bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending) > 0
}
