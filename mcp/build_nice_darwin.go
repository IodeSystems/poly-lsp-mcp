package mcp

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// x/sys exposes Setpriority on darwin but not the Darwin-only constants.
const (
	prioDarwinThread = 3      // PRIO_DARWIN_THREAD
	prioDarwinBG     = 0x1000 // PRIO_DARWIN_BG
)

// lowerThreadPriority locks the calling goroutine to its thread and puts
// that thread in the background class.
func lowerThreadPriority() {
	runtime.LockOSThread()
	_ = unix.Setpriority(prioDarwinThread, 0, prioDarwinBG)
}
