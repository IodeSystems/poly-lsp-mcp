package multiplex

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

const fdinfoFixture = `pos:	0
flags:	02000000
mnt_id:	15
ino:	1234
inotify wd:1 ino:5f sdev:800003 mask:fc6 ignored_mask:0 fhandle-bytes:8 fhandle-type:1 f_handle:5f00000000000000
inotify wd:2 ino:60 sdev:800003 mask:fc6 ignored_mask:0 fhandle-bytes:8 fhandle-type:1 f_handle:6000000000000000
inotify wd:3 ino:61 sdev:800003 mask:fc6 ignored_mask:0 fhandle-bytes:8 fhandle-type:1 f_handle:6100000000000000
`

func TestCountInotifyWds(t *testing.T) {
	if n := countInotifyWds(fdinfoFixture); n != 3 {
		t.Fatalf("got %d want 3", n)
	}
	// An epoll fdinfo has "tfd:" lines, not "inotify wd:".
	if n := countInotifyWds("pos:\t0\ntfd: 5 events: 19 data: 5\n"); n != 0 {
		t.Fatalf("epoll: got %d", n)
	}
	if n := countInotifyWds(""); n != 0 {
		t.Fatalf("empty: got %d", n)
	}
}

func TestFleetWarnsOncePerCrossing(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	f := newTestFleet(t, Lifecycle{})
	a := startInFleet(t, f)
	pid := a.Running(stubURI).Pid()
	total := 10
	f.readWatchMax = func() (int, error) { return 1000, nil } // threshold 50
	f.readWatches = func(p int) (WatchCount, error) {
		if p != pid {
			return WatchCount{}, nil
		}
		return WatchCount{Total: total, TopPid: 4242, TopCount: total - 1}, nil
	}
	warns := func() int { return strings.Count(buf.String(), "inotify watches") }

	f.check()
	if warns() != 0 || f.Status().Watches != 10 {
		t.Fatalf("under: warns=%d status=%+v", warns(), f.Status())
	}
	total = 60
	f.check()
	f.check()
	if warns() != 1 {
		t.Fatalf("over twice: want 1 warning, got %d\n%s", warns(), buf.String())
	}
	if out := buf.String(); !strings.Contains(out, "pid 4242 (59)") || !strings.Contains(out, "holds 60 inotify") {
		t.Fatalf("warning lacks count or top pid: %s", out)
	}
	total = 10
	f.check()
	total = 70
	f.check()
	if warns() != 2 {
		t.Fatalf("second crossing: want 2 warnings, got %d", warns())
	}
}

func TestFleetWatchLimitUnknown(t *testing.T) {
	f := newTestFleet(t, Lifecycle{})
	startInFleet(t, f)
	f.readWatchMax = func() (int, error) { return 0, errors.New("unknown") }
	f.check()
	if w := f.Status().Watches; w != 0 {
		t.Fatalf("unknown limit: watches %d", w)
	}
}
