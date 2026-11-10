//go:build linux

package multiplex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// spawnReq is one cmd.Start for the spawner thread.
type spawnReq struct {
	cmd *exec.Cmd
	err chan error
}

var spawnCh = make(chan spawnReq)

func init() { go spawnLoop() }

// spawnLoop starts every child from one OS thread that never exits.
// Pdeathsig fires when the thread that forked the child exits, not the
// process (golang/go#27505): a child started from an ordinary goroutine
// is killed whenever the runtime retires that thread.
func spawnLoop() {
	runtime.LockOSThread() // never unlocked: this thread lives as long as the process
	for r := range spawnCh {
		r.err <- r.cmd.Start()
	}
}

// startChild starts cmd in its own process group, killed by the kernel
// when this process dies (SIGKILL included). Grandchildren get no
// Pdeathsig; ReapOrphans covers them.
func startChild(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	r := spawnReq{cmd: cmd, err: make(chan error, 1)}
	spawnCh <- r
	return <-r.err
}

// awaitExit blocks until pid has exited and leaves it a zombie (WNOWAIT):
// its pid, and so its process group id, stay reserved until it is reaped.
func awaitExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// statFields returns the fields of /proc/<pid>/stat after the command
// name: [0] is state (field 3), [2] pgrp (5), [19] starttime (22).
func statFields(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, errNoProcess
	}
	if err != nil {
		return nil, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil, fmt.Errorf("/proc/%d/stat: no command name", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return nil, fmt.Errorf("/proc/%d/stat: %d fields", pid, len(f))
	}
	return f, nil
}

// readProcIdent reads pid's start time (clock ticks since boot) and
// executable from /proc.
func readProcIdent(pid int) (procIdent, error) {
	f, err := statFields(pid)
	if err != nil {
		return procIdent{}, err
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return procIdent{}, err
	}
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)) // "" for a zombie or another user's process
	return procIdent{PID: pid, Start: start, Boot: bootID(), Exe: strings.TrimSuffix(exe, " (deleted)")}, nil
}

var bootID = sync.OnceValue(func() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
})

// groupMembers returns the pids in process group pgid other than pgid.
func groupMembers(pgid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	want := strconv.Itoa(pgid)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == pgid {
			continue
		}
		if f, err := statFields(pid); err == nil && f[2] == want {
			out = append(out, pid)
		}
	}
	return out
}
