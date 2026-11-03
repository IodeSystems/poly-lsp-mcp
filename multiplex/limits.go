package multiplex

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// childNice is the nice value every child LSP runs at, so a busy child
// cannot starve the editor or the rest of the machine.
const childNice = 10

// maxClangdJobs caps clangd's worker count. An assumption, not a measurement.
const maxClangdJobs = 8

var clangdName = regexp.MustCompile(`^clangd(-\d+)?(\.exe)?$`)

// clangdJobs is min(max(ceil(ncpu/2), 1), maxClangdJobs). clangd defaults
// to every hardware thread, and the built-in c and cpp languages each run
// one clangd per root.
func clangdJobs(ncpu int) int {
	j := (ncpu + 1) / 2
	if j < 1 {
		j = 1
	}
	if j > maxClangdJobs {
		j = maxClangdJobs
	}
	return j
}

// withLimits returns args for the command at path, with a worker cap added
// when it is clangd (clangd, clangd-17, clangd.exe) and no argument sets
// one. A user-supplied -j wins. The input slice is not modified.
func withLimits(path string, args []string, ncpu int) []string {
	if !clangdName.MatchString(strings.ToLower(filepath.Base(path))) {
		return args
	}
	for _, a := range args {
		// -j=4, -j4, -j, --j=4, --j (the value follows as the next arg).
		if strings.HasPrefix(a, "-j") || strings.HasPrefix(a, "--j") {
			return args
		}
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, args...)
	return append(out, "-j="+strconv.Itoa(clangdJobs(ncpu)))
}
