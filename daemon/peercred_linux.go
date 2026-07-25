//go:build linux

package daemon

import (
	"fmt"
	"net"
	"syscall"
)

// peerCred reads the connecting process's uid and pid via SO_PEERCRED.
// The kernel supplies these — a client cannot forge them — which is why
// this holds even if the socket's 0600 mode bits are ever wrong.
func peerCred(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("peer cred: not a unix conn (%T)", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var ucred *syscall.Ucred
	var innerErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		ucred, innerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctrlErr != nil {
		return 0, 0, ctrlErr
	}
	if innerErr != nil {
		return 0, 0, innerErr
	}
	return int(ucred.Uid), int(ucred.Pid), nil
}
