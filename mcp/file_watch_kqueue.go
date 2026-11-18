//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package mcp

import (
	"log"
	"syscall"
)

// hostWatchBudget is the kqueue budget: a share of the process's
// RLIMIT_NOFILE soft limit. Go raises that limit at start-up to the hard
// limit, capped on darwin at kern.maxfilesperproc (syscall/rlimit.go),
// so it already reflects both.
func hostWatchBudget() *watchBudget {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil || rl.Cur == 0 || rl.Cur > 1<<30 {
		log.Printf("mcp file-watch: cannot read RLIMIT_NOFILE (%v, cur=%d); assuming %d", err, rl.Cur, kqueueMaxFallback)
		return newKqueueBudget(kqueueMaxFallback)
	}
	return newKqueueBudget(int(rl.Cur))
}
