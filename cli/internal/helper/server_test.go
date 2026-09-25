package helper

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/four43labs/roxey/cli/internal/config"
)

// testStateDir returns a short-lived state dir under /tmp: the Unix socket
// path limit (104 bytes on macOS) makes t.TempDir() paths too long.
func testStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rxy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	config.SetDir(dir)
	t.Cleanup(func() { config.SetDir("") })
	return dir
}

func startServer(t *testing.T, uid uint32, relay *RelaySupervisor) {
	t.Helper()
	srv := NewServer(config.Dir(), uid, relay)
	l, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { l.Close(); os.Remove(config.SocketPath()) })
	go srv.Serve(l)
}

func TestProtocolRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		_ = writeMessage(a, Request{Op: OpSyncHosts, Hosts: []string{"app.dev"}})
	}()

	var req Request
	if err := readMessage(b, &req); err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if req.Op != OpSyncHosts || len(req.Hosts) != 1 || req.Hosts[0] != "app.dev" {
		t.Fatalf("unexpected request: %+v", req)
	}
}

func TestServerPing(t *testing.T) {
	testStateDir(t)
	startServer(t, uint32(os.Getuid()), nil)

	c, err := net.Dial("unix", config.SocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := writeMessage(c, Request{Op: OpPing}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp Response
	if err := readMessage(c, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !resp.OK || resp.Version != ProtocolVersion {
		t.Fatalf("ping response = %+v, want ok version %d", resp, ProtocolVersion)
	}
}

func TestServerRejectsWrongUID(t *testing.T) {
	testStateDir(t)
	// Server expects a different uid than the connecting process.
	startServer(t, uint32(os.Getuid())+1, nil)

	c, err := net.Dial("unix", config.SocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := writeMessage(c, Request{Op: OpPing}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp Response
	if err := readMessage(c, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.OK {
		t.Fatal("expected unauthorized peer to be rejected")
	}
}

func TestSocketPathInStateDir(t *testing.T) {
	dir := testStateDir(t)
	if got := config.SocketPath(); got != filepath.Join(dir, "helper.sock") {
		t.Fatalf("SocketPath = %q", got)
	}
}

func TestEnsureRelayWithoutSupervisor(t *testing.T) {
	testStateDir(t)
	startServer(t, uint32(os.Getuid()), nil)

	if err := EnsureRelay(); err != nil {
		t.Fatalf("EnsureRelay: %v", err)
	}
}

func TestSyncHostsRejectsUnregisteredTLD(t *testing.T) {
	testStateDir(t)
	startServer(t, uint32(os.Getuid()), nil)

	// No projects registered, so any host is rejected before any write.
	if err := SyncHosts([]string{"evil.com"}); err == nil {
		t.Fatal("expected SyncHosts to reject unregistered TLD")
	}
}
