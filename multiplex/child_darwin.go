//go:build darwin

package multiplex

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// readProcIdent reads pid's start time (microseconds since the epoch) and
// command name (p_comm, at most 16 bytes) from sysctl kern.proc.pid.
func readProcIdent(pid int) (procIdent, error) {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return procIdent{}, errNoProcess
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return procIdent{}, err
	}
	if int(k.Proc.P_pid) != pid {
		return procIdent{}, errNoProcess
	}
	st := k.Proc.P_starttime
	return procIdent{
		PID:   pid,
		Start: uint64(st.Sec)*1_000_000 + uint64(st.Usec),
		Exe:   unix.ByteSliceToString(k.Proc.P_comm[:]),
	}, nil
}

// groupMembers returns the pids in process group pgid other than pgid.
func groupMembers(pgid int) []int {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil
	}
	var out []int
	for _, k := range ks {
		if pid := int(k.Proc.P_pid); pid != pgid {
			out = append(out, pid)
		}
	}
	return out
}
