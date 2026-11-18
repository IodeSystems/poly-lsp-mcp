package mcp

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
)

// Per-root activity: what the watcher and the index spend on one root,
// and the runaway guard that pauses a root whose watcher never goes
// quiet. A daemon holds many roots in one process, so process CPU says
// nothing about which root costs it; each piece of work is timed
// instead (wall time). Git subprocesses also report their own CPU
// (os.ProcessState), which costs nothing extra. docs/design.md
// "Per-root activity" has the status fields.

// opKind is one kind of timed work.
type opKind int

const (
	opIndexWalk     opKind = iota // a whole index build
	opParse                       // a watched file re-read and re-extracted
	opIgnoreResolve               // the ignore set re-resolved (git ls-files)
	opGitSpawn                    // one git subprocess, any caller
	opWatchEvent                  // one fsnotify event routed
	opKinds
)

var opNames = [opKinds]string{"indexWalk", "parse", "ignoreResolve", "gitSpawn", "watchEvent"}

// eventDriven: work a watch event causes. Only this counts as busy time
// for the guard; a build a query or the resync starts does not.
func (k opKind) eventDriven() bool {
	return k == opParse || k == opIgnoreResolve || k == opWatchEvent
}

// Runaway guard. A root is hot for a minute when it receives
// guardEventsPerMin events or spends guardBusyPerMin on event-driven
// work in that minute. It backs off after guardWindowMins hot minutes
// in a row.
//
//   - 5 minutes: an editing burst, a checkout, a rebase or an install
//     ends in seconds to a minute or two. Five minutes without a break
//     is a loop: a tool rewriting files, or the server's own work
//     causing events.
//   - 300 events/min (5/s, sustained): a save is 1-4 events; even an
//     autosaving editor saves a few times per minute. A 500-file
//     checkout is about 1,500 events once, so it makes one or two hot
//     minutes, not five.
//   - 15s busy/min (a quarter of one core, sustained): catches a slow
//     loop of few events that each cost much, for example a re-resolve
//     of a large repo's ignores on every .gitignore write.
//
// After a backoff the root needs only guardRetripMins hot minute to back
// off again, with the backoff doubled, until it has stayed calm for
// backoffCalm. Otherwise a permanent storm would run 5 of every 6
// minutes; with this, 1 of every 31 at the cap.
const (
	guardWindowMins   = 5
	guardRetripMins   = 1
	guardEventsPerMin = 300
	guardBusyPerMin   = 15 * time.Second
	backoffBase       = time.Minute
	backoffMax        = 30 * time.Minute
	backoffCalm       = 30 * time.Minute

	// handlingWindow: a root that saw an event this recently shows
	// handling-events; the debounce (200ms) plus a parse fit well inside.
	handlingWindow = 5 * time.Second
	// topPathsCap bounds the per-path sample kept for the backoff log.
	topPathsCap = 256
	// pendingCap bounds the paths coalesced during a backoff and replayed
	// to the child LSPs at the resync. The index needs none of them: the
	// resync rebuilds it.
	pendingCap = 4096
)

// Rate ring: counts in 10-second buckets, about 10 minutes of them. A
// "minute" is the six buckets ending at some bucket, so the current
// minute includes the current partial bucket.
const (
	rateBucket  = 10 * time.Second
	rateBuckets = 64
	bucketsMin  = int64(time.Minute / rateBucket)
)

type rateRing struct {
	idx [rateBuckets]int64 // bucket number held in each slot
	n   [rateBuckets]int64
}

func bucketOf(t time.Time) int64 { return t.UnixNano() / int64(rateBucket) }

func (r *rateRing) add(t time.Time, v int64) {
	b := bucketOf(t)
	i := b % rateBuckets
	if r.idx[i] != b {
		r.idx[i], r.n[i] = b, 0
	}
	r.n[i] += v
}

// minute sums the six buckets ending at bucket end.
func (r *rateRing) minute(end int64) int64 {
	var sum int64
	for b := end - bucketsMin + 1; b <= end; b++ {
		if i := b % rateBuckets; r.idx[i] == b {
			sum += r.n[i]
		}
	}
	return sum
}

// opTotal is the cumulative cost of one kind of work.
type opTotal struct {
	count int64
	wall  time.Duration
	cpu   time.Duration
}

// rootActivity is one root's counters and guard. Methods are safe for
// concurrent use and on a nil receiver.
type rootActivity struct {
	root string
	now  func() time.Time
	// afterFunc schedules the end of a backoff; a test replaces it.
	afterFunc func(time.Duration, func())
	// resync runs after a backoff ends, outside mu, with the paths that
	// changed during it (op = the events seen, ORed) and whether some
	// were not kept.
	resync func(pending map[string]fsnotify.Op, overflow bool)
	logf   func(string, ...any)

	mu        sync.Mutex
	ops       [opKinds]opTotal
	events    rateRing
	gits      rateRing
	busy      rateRing // nanoseconds of event-driven work
	building  int      // index walks running
	built     int64    // index walks finished
	resolving int      // ignore re-resolves running
	lastEvent time.Time
	lastPath  string
	paths     map[string]int // events per path in the current hot streak

	backedOff bool
	until     time.Time
	backoff   time.Duration // the current or last backoff
	trips     int
	resumedAt time.Time
	pending   map[string]fsnotify.Op
	overflow  bool
}

func newRootActivity(root string) *rootActivity {
	return &rootActivity{root: root, now: time.Now, afterFunc: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		logf: log.Printf, paths: map[string]int{}}
}

// start begins one timed piece of work; call the result when it ends.
func (a *rootActivity) start(k opKind) func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	switch k {
	case opIndexWalk:
		a.building++
	case opIgnoreResolve:
		a.resolving++
	}
	t0 := a.now()
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		now := a.now()
		d := now.Sub(t0)
		a.ops[k].count++
		a.ops[k].wall += d
		switch k {
		case opIndexWalk:
			a.building--
			a.built++
		case opIgnoreResolve:
			a.resolving--
		}
		if k.eventDriven() {
			a.busy.add(now, int64(d))
		}
	}
}

// gitSpawn records one finished git subprocess.
func (a *rootActivity) gitSpawn(wall, cpu time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ops[opGitSpawn].count++
	a.ops[opGitSpawn].wall += wall
	a.ops[opGitSpawn].cpu += cpu
	a.gits.add(a.now(), 1)
}

// watchEvent records one fsnotify event and runs the guard. It reports
// true when the root is backed off: the caller drops the event, which
// is kept (coalesced by path) for the resync.
func (a *rootActivity) watchEvent(ev fsnotify.Event) (drop bool) {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.events.add(now, 1)
	a.lastEvent, a.lastPath = now, ev.Name
	if !a.backedOff && a.hot(now) {
		a.trip(now)
	}
	if a.backedOff {
		if _, ok := a.pending[ev.Name]; ok || len(a.pending) < pendingCap {
			a.pending[ev.Name] |= ev.Op
		} else {
			a.overflow = true
		}
		return true
	}
	if a.minuteHot(bucketOf(now)) {
		if _, ok := a.paths[ev.Name]; ok || len(a.paths) < topPathsCap {
			a.paths[ev.Name]++
		}
	} else if len(a.paths) > 0 {
		clear(a.paths) // the streak, if any, is over
	}
	return false
}

func (a *rootActivity) minuteHot(end int64) bool {
	return a.events.minute(end) >= guardEventsPerMin || time.Duration(a.busy.minute(end)) >= guardBusyPerMin
}

// hot reports whether the last needed minutes were all hot, counting
// only buckets after the last resume. Caller holds mu.
func (a *rootActivity) hot(now time.Time) bool {
	need := a.needMins(now)
	cur := bucketOf(now)
	if !a.resumedAt.IsZero() && cur-need*bucketsMin+1 <= bucketOf(a.resumedAt) {
		return false
	}
	for j := int64(0); j < need; j++ {
		if !a.minuteHot(cur - j*bucketsMin) {
			return false
		}
	}
	return true
}

// needMins is how many hot minutes in a row trip the guard: fewer
// while a root that backed off has not yet stayed calm. Caller holds mu.
func (a *rootActivity) needMins(now time.Time) int64 {
	if a.trips > 0 && now.Sub(a.resumedAt) < backoffCalm {
		return guardRetripMins
	}
	return guardWindowMins
}

// trip starts a backoff. Caller holds mu.
func (a *rootActivity) trip(now time.Time) {
	need := a.needMins(now)
	if need == guardWindowMins {
		a.backoff = backoffBase
	} else {
		a.backoff = min(2*a.backoff, backoffMax)
	}
	a.trips++
	a.backedOff = true
	a.until = now.Add(a.backoff)
	a.pending = map[string]fsnotify.Op{}
	a.overflow = false
	var ev, busy int64
	cur := bucketOf(now)
	for j := int64(0); j < need; j++ {
		ev += a.events.minute(cur - j*bucketsMin)
		busy += a.busy.minute(cur - j*bucketsMin)
	}
	a.logf("mcp activity: %s: backing off %s until %s: %d events/min, busy %s/min over %d min; top paths: %s",
		a.root, a.backoff, a.until.Format("15:04:05"), ev/need,
		(time.Duration(busy / need)).Round(100*time.Millisecond), need, a.topPaths(5))
	clear(a.paths)
	a.afterFunc(a.backoff, a.endBackoff)
}

// topPaths renders the n paths with the most events in the streak,
// relative to the root.
func (a *rootActivity) topPaths(n int) string {
	type pc struct {
		p string
		c int
	}
	var all []pc
	for p, c := range a.paths {
		all = append(all, pc{p, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].c != all[j].c {
			return all[i].c > all[j].c
		}
		return all[i].p < all[j].p
	})
	var out []string
	for i, e := range all {
		if i == n {
			break
		}
		rel := e.p
		if r, err := filepath.Rel(a.root, e.p); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		out = append(out, fmt.Sprintf("%s (%d)", rel, e.c))
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// endBackoff ends the backoff and runs the resync.
func (a *rootActivity) endBackoff() {
	a.mu.Lock()
	if !a.backedOff {
		a.mu.Unlock()
		return
	}
	now := a.now()
	pending, overflow := a.pending, a.overflow
	a.backedOff, a.until, a.pending, a.overflow = false, time.Time{}, nil, false
	a.resumedAt = now
	a.logf("mcp activity: %s: resuming after %s backoff; resync: index rebuild, %d changed path(s) to the child LSPs (overflow %v)",
		a.root, a.backoff, len(pending), overflow)
	resync := a.resync
	a.mu.Unlock()
	if resync != nil {
		resync(pending, overflow)
	}
}

// Activity states, as status shows them.
const (
	stateIdle      = "idle"
	stateIndexing  = "indexing"
	stateRebuild   = "rebuilding"
	stateHandling  = "handling-events"
	stateBackedOff = "backed-off"
)

// OpStatus is the cumulative cost of one kind of work.
type OpStatus struct {
	Count  int64 `json:"count"`
	WallMs int64 `json:"wallMs"`
	CPUMs  int64 `json:"cpuMs,omitempty"` // git spawns only
}

// ActivityStatus is a root's activity in its status file.
type ActivityStatus struct {
	State            string              `json:"state"`
	BackedOffUntil   string              `json:"backedOffUntil,omitempty"` // RFC 3339
	Backoffs         int                 `json:"backoffs,omitempty"`
	EventsLastMin    int64               `json:"eventsLastMin"`
	GitSpawnsLastMin int64               `json:"gitSpawnsLastMin"`
	LastEventAt      string              `json:"lastEventAt,omitempty"` // RFC 3339
	LastEventPath    string              `json:"lastEventPath,omitempty"`
	Ops              map[string]OpStatus `json:"ops"`
}

// status snapshots the counters.
func (a *rootActivity) status() ActivityStatus {
	if a == nil {
		return ActivityStatus{State: stateIdle, Ops: map[string]OpStatus{}}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	cur := bucketOf(now)
	st := ActivityStatus{
		State:            a.state(now),
		Backoffs:         a.trips,
		EventsLastMin:    a.events.minute(cur),
		GitSpawnsLastMin: a.gits.minute(cur),
		LastEventPath:    a.lastPath,
		Ops:              make(map[string]OpStatus, opKinds),
	}
	if a.backedOff {
		st.BackedOffUntil = a.until.Format(time.RFC3339)
	}
	if !a.lastEvent.IsZero() {
		st.LastEventAt = a.lastEvent.Format(time.RFC3339)
	}
	for k, o := range a.ops {
		st.Ops[opNames[k]] = OpStatus{Count: o.count, WallMs: o.wall.Milliseconds(), CPUMs: o.cpu.Milliseconds()}
	}
	return st
}

// state is the root's current state. Caller holds mu.
func (a *rootActivity) state(now time.Time) string {
	switch {
	case a.backedOff:
		return stateBackedOff
	case a.building > 0 && a.built == 0:
		return stateIndexing
	case a.building > 0 || a.resolving > 0:
		return stateRebuild
	case !a.lastEvent.IsZero() && now.Sub(a.lastEvent) < handlingWindow:
		return stateHandling
	}
	return stateIdle
}

// Every root's activity, by root, so a git spawn (which knows only its
// directory) finds its root.
var activityRoots = struct {
	sync.Mutex
	m map[string]*rootActivity
}{m: map[string]*rootActivity{}}

func init() { git.SpawnHook = noteGitSpawn }

// noteGitSpawn credits a git run to the root holding dir: the longest
// registered root that is dir or contains it.
func noteGitSpawn(dir string, wall, cpu time.Duration) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	activityRoots.Lock()
	var best *rootActivity
	for root, a := range activityRoots.m {
		if (dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))) &&
			(best == nil || len(root) > len(best.root)) {
			best = a
		}
	}
	activityRoots.Unlock()
	best.gitSpawn(wall, cpu)
}

// act is this server's activity, made on first use and registered
// under the root. nil-safe callers: every rootActivity method is.
func (s *Server) act() *rootActivity {
	s.activityOnce.Do(func() {
		root := s.getRoot()
		a := newRootActivity(root)
		a.logf = s.logf
		a.resync = s.resyncAfterBackoff
		s.activity = a
		if root != "" {
			activityRoots.Lock()
			activityRoots.m[root] = a
			activityRoots.Unlock()
		}
	})
	return s.activity
}

// ActivityStatus is this root's activity for the status file.
func (s *Server) ActivityStatus() ActivityStatus { return s.act().status() }

// resyncAfterBackoff brings a root back in step after a backoff dropped
// its events: rebuild the index, watch the dirs created meanwhile, and
// tell the child LSPs about each path that changed. No-op once the
// watcher has stopped.
func (s *Server) resyncAfterBackoff(pending map[string]fsnotify.Op, overflow bool) {
	s.watcherMu.Lock()
	w := s.watcher
	s.watcherMu.Unlock()
	if w == nil {
		return
	}
	if err := s.BuildIndex(); err != nil {
		s.logf("mcp activity: %s: resync rebuild: %v", s.getRoot(), err)
	}
	if wa, ok := w.(watchAdder); ok {
		s.addWatchDirs(wa, s.getRoot())
	}
	for path, op := range pending {
		ev := fsnotify.Event{Name: path, Op: fsnotify.Remove}
		if _, err := os.Stat(path); err == nil {
			ev.Op = fsnotify.Write
			if op&fsnotify.Create != 0 {
				ev.Op = fsnotify.Create
			}
		}
		s.queueChildFileEvent(ev)
	}
	if overflow {
		s.logf("mcp activity: %s: over %d paths changed during the backoff; the child LSPs heard of the first %d only",
			s.getRoot(), pendingCap, pendingCap)
	}
}
