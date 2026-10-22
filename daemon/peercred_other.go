//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// peerCred has no implementation here. It refuses rather than reporting
// the daemon's own uid: the accept filter rejects the connection.
func peerCred(_ net.Conn) (uid, pid int, err error) {
	return 0, 0, errors.New("peer cred: unsupported on this platform")
}
