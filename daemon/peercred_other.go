//go:build !linux

package daemon

import (
	"net"
	"os"
)

// peerCred has no portable non-Linux implementation (macOS uses
// getpeereid, which the standard syscall package does not expose). On
// these platforms the peer-credential gate degenerates to the socket's
// 0600 mode bits — only the owning user can connect anyway — so we
// report the daemon's own uid, which the accept filter treats as
// allowed. pid is unavailable (0), so the optional /proc-based binding
// strengthening (Linux-only) is simply absent here.
func peerCred(_ net.Conn) (uid, pid int, err error) {
	return os.Getuid(), 0, nil
}
