//go:build darwin

package multiplex

import "golang.org/x/sys/unix"

// TotalRAM is the machine's physical RAM in bytes (hw.memsize), 0 if unknown.
func TotalRAM() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}
