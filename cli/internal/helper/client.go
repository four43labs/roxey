package helper

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/four43labs/roxey/cli/internal/config"
)

// Available reports whether a helper daemon is listening and authorized.
func Available() bool {
	c, err := dial(500 * time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	resp, err := call(c, Request{Op: OpPing})
	return err == nil && resp.OK
}

// SyncHosts asks the daemon to apply the managed /etc/hosts block.
func SyncHosts(hosts []string) error { return do(Request{Op: OpSyncHosts, Hosts: hosts}) }

// EnsureRelay asks the daemon to start the relay for the primary local TLD.
func EnsureRelay() error { return do(Request{Op: OpEnsureRelay}) }

// TrustCA asks the daemon to trust the local CA in the system store.
func TrustCA() error { return do(Request{Op: OpTrustCA}) }

// RelayStatus returns the daemon's relay TLD and health.
func RelayStatus() (tld string, up bool, err error) {
	resp, err := callOp(Request{Op: OpRelayStatus})
	if err != nil {
		return "", false, err
	}
	return resp.TLD, resp.RelayUp, nil
}

func do(req Request) error {
	resp, err := callOp(req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

func callOp(req Request) (Response, error) {
	c, err := dial(2 * time.Second)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	return call(c, req)
}

func dial(timeout time.Duration) (net.Conn, error) {
	path := config.SocketPath()
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("helper daemon not installed (run `roxey setup`)")
	}
	return net.DialTimeout("unix", path, timeout)
}

func call(c net.Conn, req Request) (Response, error) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeMessage(c, req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := readMessage(c, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
