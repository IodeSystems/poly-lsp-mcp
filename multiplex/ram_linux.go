//go:build linux

package multiplex

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// TotalRAM is the machine's physical RAM in bytes (MemTotal), 0 if unknown.
func TotalRAM() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fs := strings.Fields(rest) // 16384000 kB
			if len(fs) == 0 {
				return 0
			}
			kb, err := strconv.ParseInt(fs[0], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}
