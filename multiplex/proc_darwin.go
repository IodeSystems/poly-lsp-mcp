//go:build darwin

package multiplex

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// readVmRSS returns the resident set size of pid, in bytes, from ps.
// macOS has no /proc.
func readVmRSS(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

// readMemoryPSI: macOS has no PSI, so the pressure rule never fires.
func readMemoryPSI() (float64, error) {
	return 0, errors.New("memory PSI: not available on macOS")
}
