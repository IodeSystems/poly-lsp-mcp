package mcp

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// buildNice is the nice value of a thread running an index build.
const buildNice = 10

// niceHook, when set, observes the thread id niced (tests).
var niceHook func(tid int)

// lowerThreadPriority locks the calling goroutine to its thread and sets
// that thread's nice to buildNice. Linux nice is per thread (tid).
func lowerThreadPriority() {
	runtime.LockOSThread()
	tid := unix.Gettid()
	_ = unix.Setpriority(unix.PRIO_PROCESS, tid, buildNice)
	if niceHook != nil {
		niceHook(tid)
	}
}
