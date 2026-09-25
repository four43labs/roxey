package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func muxPair(t *testing.T) (server, client *yamux.Session) {
	t.Helper()
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	server, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err = yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); client.Close() })
	return server, client
}

// A pending entry is routable at once but opens no stream until Ready: the
// CLI must receive its "host:" greeting before any stream frame.
func TestPendingEntryHoldsStreamsUntilReady(t *testing.T) {
	server, client := muxPair(t)
	go func() {
		for {
			s, err := client.Accept()
			if err != nil {
				return
			}
			s.Close()
		}
	}()
	reg := NewRegistry()
	e, err := reg.Register(&Entry{Service: "app", Host: "app-1", UserID: "u", Pending: true}, "", server, "test")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Resolve("app-1", "/") != e {
		t.Fatal("a pending entry is already routable")
	}
	opened := make(chan error, 1)
	go func() {
		s, err := e.OpenStream()
		if err == nil {
			s.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("OpenStream returned before Ready (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	e.Ready()
	e.Ready() // idempotent
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("OpenStream after Ready: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenStream did not proceed after Ready")
	}
}

func TestNonPendingEntryOpensImmediately(t *testing.T) {
	server, _ := muxPair(t)
	reg := NewRegistry()
	e, err := reg.Register(&Entry{Service: "app", Host: "app-2", UserID: "u"}, "", server, "test")
	if err != nil {
		t.Fatal(err)
	}
	old := ReadyTimeout
	ReadyTimeout = 50 * time.Millisecond
	defer func() { ReadyTimeout = old }()
	if _, err := e.OpenStream(); err != nil && err.Error() == "tunnel not ready" {
		t.Fatal("an entry registered without Pending is ready at once")
	}
}
