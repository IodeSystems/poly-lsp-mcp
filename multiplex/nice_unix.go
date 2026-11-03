//go:build linux || darwin

package multiplex

import "syscall"

// lowerPriority sets nice +childNice on pid. On Linux this sets the main
// thread only; threads created later inherit it. Start returns right after
// exec, before the child spawns its workers, so they inherit it in practice.
func lowerPriority(pid int) error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, childNice)
}
