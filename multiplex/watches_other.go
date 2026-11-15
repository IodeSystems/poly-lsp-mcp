//go:build !linux

package multiplex

import "errors"

// No /proc inotify accounting here: the watch count is unknown.
func readWatchLimit() (int, error) {
	return 0, errors.New("inotify limit: Linux only")
}

func readTreeWatches(int) (WatchCount, error) {
	return WatchCount{}, errors.New("inotify watch count: Linux only")
}
