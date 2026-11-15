package multiplex

import "strings"

// WatchCount is the inotify watch use of one child process tree.
type WatchCount struct {
	Total    int // watches held by the root pid and every descendant
	TopPid   int // the pid in the tree that holds the most
	TopCount int // watches held by TopPid
}

// countInotifyWds counts the "inotify wd:" lines of one /proc/<pid>/fdinfo/<fd>
// file. Each line is one watch on that inotify instance.
func countInotifyWds(fdinfo string) int {
	n := 0
	for _, line := range strings.Split(fdinfo, "\n") {
		if strings.HasPrefix(line, "inotify wd:") {
			n++
		}
	}
	return n
}

// TreeWatches counts the inotify watches of pid and all its descendants.
// Linux only; an error elsewhere.
func TreeWatches(pid int) (WatchCount, error) { return readTreeWatches(pid) }
