//go:build !linux

package multiplex

import (
	"errors"
	"os/exec"
	"syscall"
)

// startChild starts cmd in its own process group. There is no Pdeathsig
// here: the child of a process killed outright runs on until a later
// poly-lsp-mcp's ReapOrphans.
func startChild(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}

// awaitExit: no waitid(WNOWAIT) here, so the group of a child that exits
// on its own is not signalled after that exit.
func awaitExit(int) error { return errors.New("waitid WNOWAIT: unsupported") }
