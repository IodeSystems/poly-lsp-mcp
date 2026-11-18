package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// fakeActivity is a rootActivity on a fake clock that records its log
// lines, scheduled backoffs and resyncs instead of acting on them.
type fakeActivity struct {
	*rootActivity
	t        time.Time
	logs     []string
	backoffs []time.Duration
	resyncs  []map[string]fsnotify.Op
}

func newFakeActivity() *fakeActivity {
	f := &fakeActivity{t: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	a := newRootActivity("/repo")
	a.now = func() time.Time { return f.t }
	a.logf = func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) }
	a.afterFunc = func(d time.Duration, _ func()) { f.backoffs = append(f.backoffs, d) }
	a.resync = func(p map[string]fsnotify.Op, _ bool) { f.resyncs = append(f.resyncs, p) }
	f.rootActivity = a
	return f
}

func (f *fakeActivity) advance(d time.Duration) { f.t = f.t.Add(d) }

// event sends one write event and, when not dropped, a parse costing
// parse of clock time.
func (f *fakeActivity) event(path string, parse time.Duration) (dropped bool) {
	if f.watchEvent(fsnotify.Event{Name: path, Op: fsnotify.Write}) {
		return true
	}
	if parse > 0 {
		done := f.start(opParse)
		f.advance(parse)
		done()
	}
	return false
}

// storm sends n events per second for d.
func (f *fakeActivity) storm(n int, d time.Duration) {
	step := time.Second / time.Duration(n)
	for end := f.t.Add(d); f.t.Before(end); f.advance(step) {
		f.event(fmt.Sprintf("/repo/gen/%d.go", f.t.Second()%3), 0)
	}
}

func TestRateRingRollsOver(t *testing.T) {
	var r rateRing
	t0 := time.Unix(1_000_000_000, 0)
	r.add(t0, 5)
	r.add(t0.Add(30*time.Second), 2)
	if got := r.minute(bucketOf(t0.Add(30 * time.Second))); got != 7 {
		t.Fatalf("minute = %d, want 7", got)
	}
	// 60s on, the first bucket has left the minute.
	if got := r.minute(bucketOf(t0.Add(60 * time.Second))); got != 2 {
		t.Fatalf("minute after 60s = %d, want 2", got)
	}
	// A full ring later the same slot holds a new bucket: the old count
	// is gone, not added to.
	later := t0.Add(rateBuckets * rateBucket)
	r.add(later, 1)
	if got := r.minute(bucketOf(later)); got != 1 {
		t.Fatalf("minute a ring later = %d, want 1", got)
	}
}

func TestStatusEventsLastMinRollsOver(t *testing.T) {
	f := newFakeActivity()
	for range 10 {
		f.event("/repo/a.go", 0)
	}
	if st := f.status(); st.EventsLastMin != 10 || st.State != stateHandling || st.LastEventPath != "/repo/a.go" {
		t.Fatalf("status = %+v", st)
	}
	f.advance(61 * time.Second)
	if st := f.status(); st.EventsLastMin != 0 || st.State != stateIdle {
		t.Fatalf("status a minute on = %+v", st)
	}
}

// A 500-file checkout (about 3 events and one parse per file, in a few
// seconds), once, then ten minutes of ordinary editing, must never back
// off.
func TestGuardCheckoutBurstDoesNotBackOff(t *testing.T) {
	f := newFakeActivity()
	for i := range 500 {
		p := fmt.Sprintf("/repo/pkg/f%03d.go", i)
		f.event(p, 0)
		f.event(p, 0)
		f.event(p, 5*time.Millisecond)
	}
	if st := f.status(); st.EventsLastMin != 1500 {
		t.Fatalf("eventsLastMin = %d, want 1500", st.EventsLastMin)
	}
	for range 120 { // a save every 5s for 10 minutes
		f.advance(5 * time.Second)
		f.event("/repo/main.go", 0)
		f.event("/repo/main.go", 30*time.Millisecond)
	}
	if len(f.backoffs) != 0 || f.status().State == stateBackedOff {
		t.Fatalf("backed off on a checkout and editing: %v", f.logs)
	}
	// The same burst every minute is still not five hot minutes if it
	// stops: four bursts, then quiet.
	for range 4 {
		for range 400 {
			f.event("/repo/x.go", 0)
		}
		f.advance(time.Minute)
	}
	f.advance(10 * time.Minute)
	f.event("/repo/x.go", 0)
	if len(f.backoffs) != 0 {
		t.Fatalf("backed off after four hot minutes: %v", f.logs)
	}
}

func TestGuardBacksOffWithDoublingCappedBackoff(t *testing.T) {
	f := newFakeActivity()
	f.storm(10, 4*time.Minute) // 600/min, not yet five minutes
	if len(f.backoffs) != 0 {
		t.Fatalf("backed off after 4 minutes: %v", f.logs)
	}
	f.storm(10, 90*time.Second)
	if len(f.backoffs) != 1 || f.backoffs[0] != time.Minute {
		t.Fatalf("backoffs = %v, want [1m]", f.backoffs)
	}
	st := f.status()
	if st.State != stateBackedOff || st.BackedOffUntil == "" || st.Backoffs != 1 {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(f.logs[0], "backing off 1m0s") || !strings.Contains(f.logs[0], "gen/") ||
		!strings.Contains(f.logs[0], "events/min") {
		t.Fatalf("log = %q", f.logs[0])
	}
	// Dropped while backed off, coalesced by path.
	if !f.event("/repo/gen/9.go", 0) || !f.event("/repo/gen/9.go", 0) {
		t.Fatal("event not dropped while backed off")
	}
	f.advance(time.Minute)
	f.endBackoff()
	if len(f.resyncs) != 1 || len(f.resyncs[0]) == 0 || f.status().State == stateBackedOff {
		t.Fatalf("resyncs = %v, state %s", f.resyncs, f.status().State)
	}
	// The storm goes on: each backoff doubles, capped at 30m, after one
	// hot minute each.
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for len(f.backoffs) < len(want) {
		n := len(f.backoffs)
		f.storm(10, 80*time.Second)
		if len(f.backoffs) != n+1 {
			t.Fatalf("no backoff after %d: %v", n, f.backoffs)
		}
		f.advance(f.backoffs[n])
		f.endBackoff()
	}
	for i := range want {
		if f.backoffs[i] != want[i] {
			t.Fatalf("backoffs = %v, want %v", f.backoffs, want)
		}
	}
	// Calm for backoffCalm: the next storm needs five minutes again and
	// starts over at 1m.
	f.advance(backoffCalm)
	n := len(f.backoffs)
	f.storm(10, 4*time.Minute)
	if len(f.backoffs) != n {
		t.Fatal("re-tripped within 4 minutes after a calm period")
	}
	f.storm(10, 2*time.Minute)
	if len(f.backoffs) != n+1 || f.backoffs[n] != time.Minute {
		t.Fatalf("after calm: backoffs = %v", f.backoffs[n:])
	}
}

// Few events that each cost much: busy time trips the guard.
func TestGuardBusyTimeTrips(t *testing.T) {
	f := newFakeActivity()
	for end := f.t.Add(6 * time.Minute); f.t.Before(end) && len(f.backoffs) == 0; f.advance(2 * time.Second) {
		f.event("/repo/.gitignore", 0)
		done := f.start(opIgnoreResolve)
		f.advance(time.Second) // 1s of every 3s: 20s/min
		done()
	}
	if len(f.backoffs) != 1 {
		t.Fatalf("busy root did not back off: %v", f.logs)
	}
}

func TestActivityStatusJSON(t *testing.T) {
	f := newFakeActivity()
	done := f.start(opIndexWalk)
	if st := f.status(); st.State != stateIndexing {
		t.Fatalf("state during first walk = %s", st.State)
	}
	f.advance(1500 * time.Millisecond)
	done()
	f.gitSpawn(20*time.Millisecond, 8*time.Millisecond)
	f.event("/repo/a.go", 0)
	raw, err := json.Marshal(multiplex.StatusFile{PID: 1, Root: "/repo", Activity: mustJSON(t, f.status())})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Activity map[string]json.RawMessage `json:"activity"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "eventsLastMin", "gitSpawnsLastMin", "lastEventAt", "lastEventPath", "ops"} {
		if _, ok := got.Activity[k]; !ok {
			t.Errorf("activity lacks %q: %s", k, raw)
		}
	}
	var ops map[string]OpStatus
	if err := json.Unmarshal(got.Activity["ops"], &ops); err != nil {
		t.Fatal(err)
	}
	if o := ops["indexWalk"]; o.Count != 1 || o.WallMs != 1500 {
		t.Errorf("indexWalk = %+v", o)
	}
	if o := ops["gitSpawn"]; o.Count != 1 || o.WallMs != 20 || o.CPUMs != 8 {
		t.Errorf("gitSpawn = %+v", o)
	}
	for _, k := range opNames {
		if _, ok := ops[k]; !ok {
			t.Errorf("ops lacks %q", k)
		}
	}
	// A status file from before activity still reads, with no activity.
	var old multiplex.StatusFile
	if err := json.Unmarshal([]byte(`{"pid":1,"root":"/r","children":[]}`), &old); err != nil || len(old.Activity) != 0 {
		t.Fatalf("old status file: %v %s", err, old.Activity)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A real git checkout of 500 files under a running watcher: counted,
// timed, and no backoff.
func TestActivityGitCheckout500(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	gitT := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(body string) {
		for i := range 500 {
			p := filepath.Join(dir, "pkg", fmt.Sprintf("f%03d.go", i))
			if err := os.WriteFile(p, []byte(fmt.Sprintf("package pkg\n\nfunc F%d() {}\n%s", i, body)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitT("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("")
	gitT("add", ".")
	gitT("commit", "-qm", "a")
	gitT("checkout", "-qb", "b")
	write("func G() {}\n")
	gitT("commit", "-qam", "b")
	gitT("checkout", "-q", "main")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetGitPrewarm(false)
	srv.SetProactiveOpen(false)
	srv.SetQuiet(true)
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	srv.kickFileWatch()
	defer func() {
		srv.stopFileWatch()
		_ = srv.WaitForFileWatch(mustCtx(t, time.Second))
	}()
	time.Sleep(100 * time.Millisecond)
	gitT("checkout", "-q", "b")

	deadline := time.Now().Add(60 * time.Second) // polls; a slow macOS runner took >10s for 500 check-ignore spawns
	var st ActivityStatus
	for time.Now().Before(deadline) {
		if st = srv.ActivityStatus(); st.Ops["parse"].Count >= 500 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Ops["parse"].Count < 500 || st.EventsLastMin < 500 || st.Ops["indexWalk"].Count != 1 {
		t.Fatalf("activity after checkout = %+v", st)
	}
	if st.State == stateBackedOff || st.Backoffs != 0 {
		t.Fatalf("checkout backed off: %+v", st)
	}
	if st.Ops["gitSpawn"].Count == 0 {
		t.Errorf("no git spawns counted: %+v", st.Ops)
	}
	if !strings.HasPrefix(st.LastEventPath, dir) {
		t.Errorf("lastEventPath = %q", st.LastEventPath)
	}
}

// The debounced .gitignore re-resolve (gitignore_reload.go) is counted:
// one ignoreResolve and its git spawn, credited to the root.
func TestActivityCountsGitignoreReload(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetGitPrewarm(false)
	srv.SetProactiveOpen(false)
	srv.SetQuiet(true)
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	before := srv.ActivityStatus().Ops
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What the debounce timer would flush, flushed now.
	srv.ignoreReload.mu.Lock()
	srv.ignoreReload.pending = map[string]bool{gi: true}
	srv.ignoreReload.mu.Unlock()
	srv.flushGitignoreReload()
	after := srv.ActivityStatus().Ops
	if got := after["ignoreResolve"].Count - before["ignoreResolve"].Count; got != 1 {
		t.Errorf("ignoreResolve count +%d, want +1", got)
	}
	if after["gitSpawn"].Count <= before["gitSpawn"].Count {
		t.Errorf("gitSpawn count %d -> %d: the reload's git ls-files was not counted",
			before["gitSpawn"].Count, after["gitSpawn"].Count)
	}
}
