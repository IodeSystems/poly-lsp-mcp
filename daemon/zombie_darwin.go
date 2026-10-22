//go:build darwin

package daemon

import (
	"os/exec"
	"strconv"
	"strings"
)

// pidZombie reports whether ps says pid is a zombie (state Z). macOS has
// no /proc. False when ps fails.
func pidZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
