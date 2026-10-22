//go:build darwin

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCred reads the connecting process's uid (LOCAL_PEERCRED) and pid
// (LOCAL_PEERPID). Kernel-supplied, like SO_PEERCRED on Linux.
func peerCred(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("peer cred: not a unix conn (%T)", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var xu *unix.Xucred
	var innerErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		xu, innerErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if innerErr == nil {
			pid, innerErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		}
	}); ctrlErr != nil {
		return 0, 0, ctrlErr
	}
	if innerErr != nil {
		return 0, 0, innerErr
	}
	return int(xu.Uid), pid, nil
}
