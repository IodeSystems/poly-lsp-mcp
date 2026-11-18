//go:build !(darwin || freebsd || openbsd || netbsd || dragonfly)

package mcp

// hostWatchBudget is the inotify budget: a share of max_user_watches,
// one unit per watched dir.
func hostWatchBudget() *watchBudget {
	return budgetFromHost(inotifyMaxPath)
}
