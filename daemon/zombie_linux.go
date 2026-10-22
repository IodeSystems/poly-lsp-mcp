//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pidZombie reports whether /proc says pid is a zombie (state Z). False
// when /proc is unavailable.
func pidZombie(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// "pid (comm) S ..." — comm may contain spaces and parens; the state
	// follows the LAST ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
