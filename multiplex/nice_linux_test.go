//go:build linux

package multiplex

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestLowerPriority(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := lowerPriority(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// getpriority via the raw syscall returns 20-nice.
	r, _, errno := syscall.Syscall(syscall.SYS_GETPRIORITY, syscall.PRIO_PROCESS, uintptr(cmd.Process.Pid), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if got := 20 - int(r); got < childNice {
		t.Errorf("nice = %d, want >= %d", got, childNice)
	}
}
