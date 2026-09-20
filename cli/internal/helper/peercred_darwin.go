//go:build darwin

package helper

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the uid of the process on the other end of a Unix socket.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Xucred
		serr error
	)
	if cerr := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); cerr != nil {
		return 0, cerr
	}
	if serr != nil {
		return 0, serr
	}
	if cred == nil {
		return 0, fmt.Errorf("no peer credentials")
	}
	return cred.Uid, nil
}
