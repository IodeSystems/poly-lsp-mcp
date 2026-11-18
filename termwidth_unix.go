//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// termSize is f's terminal width, ok false when f is not a terminal.
func termSize(f *os.File) (width int, ok bool) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, false
	}
	return int(ws.Col), true
}
