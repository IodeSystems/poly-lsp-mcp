//go:build linux

package multiplex

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readVmRSS returns VmRSS from /proc/<pid>/status, in bytes.
func readVmRSS(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line) // VmRSS: 12345 kB
		if len(f) < 2 {
			break
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("no VmRSS for pid %d", pid)
}

// readMemoryPSI returns "some avg10" from /proc/pressure/memory.
func readMemoryPSI() (float64, error) {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, err
	}
	return parsePSISomeAvg10(string(b))
}
