//go:build linux

package multiplex

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const procRoot = "/proc"

// readWatchLimit returns fs.inotify.max_user_watches.
func readWatchLimit() (int, error) {
	b, err := os.ReadFile(procRoot + "/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// countPidWatches sums the inotify watches of one pid: for every fd whose
// link is anon_inode:inotify, the "inotify wd:" lines of its fdinfo.
func countPidWatches(root string, pid int) (int, error) {
	p := filepath.Join(root, strconv.Itoa(pid))
	ents, err := os.ReadDir(filepath.Join(p, "fd"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		link, err := os.Readlink(filepath.Join(p, "fd", e.Name()))
		if err != nil || link != "anon_inode:inotify" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p, "fdinfo", e.Name()))
		if err != nil {
			continue
		}
		n += countInotifyWds(string(b))
	}
	return n, nil
}

// childPids lists the direct children of pid from /proc/<pid>/task/*/children,
// or, when that file is missing (no CONFIG_PROC_CHILDREN), by scanning every
// process's ppid.
func childPids(root string, pid int) []int {
	var out []int
	tasks, _ := filepath.Glob(filepath.Join(root, strconv.Itoa(pid), "task", "*", "children"))
	for _, f := range tasks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, s := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(s); err == nil {
				out = append(out, c)
			}
		}
	}
	if len(tasks) > 0 {
		return out
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		c, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if parentOf(root, c) == pid {
			out = append(out, c)
		}
	}
	return out
}

// parentOf reads ppid from /proc/<pid>/stat; 0 when unreadable.
func parentOf(root string, pid int) int {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// "pid (comm) S ppid ...": comm may hold spaces and parens.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	pp, _ := strconv.Atoi(f[1])
	return pp
}

// countTreeWatches counts the watches of pid and all its descendants.
func countTreeWatches(root string, pid int) (WatchCount, error) {
	var wc WatchCount
	if pid <= 0 {
		return wc, errors.New("no pid")
	}
	if _, err := os.Stat(filepath.Join(root, strconv.Itoa(pid))); err != nil {
		return wc, err
	}
	seen := map[int]bool{}
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		if n, err := countPidWatches(root, p); err == nil {
			wc.Total += n
			if n > wc.TopCount {
				wc.TopCount, wc.TopPid = n, p
			}
		}
		queue = append(queue, childPids(root, p)...)
	}
	return wc, nil
}

// readTreeWatches is the platform read used by the Fleet.
func readTreeWatches(pid int) (WatchCount, error) { return countTreeWatches(procRoot, pid) }
