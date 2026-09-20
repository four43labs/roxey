//go:build !darwin && !linux

package helper

import (
	"fmt"
	"net"
)

// peerUID is unsupported on this platform; roxey's local relay mode only
// ships for macOS and Linux.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("helper sockets are only supported on macOS and Linux")
}
