package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebsocketURLIncludesGateGroup(t *testing.T) {
	u := WebsocketURL("wss", "roxey.f43.run", "app", "/api", "secret", "up_group")
	q := u.Query()
	if q.Get("service") != "app" || q.Get("path") != "/api" || q.Get("protect") != "secret" {
		t.Fatalf("missing tunnel query values: %s", u.String())
	}
	if got := q.Get("gate_group"); got != "up_group" {
		t.Fatalf("gate_group = %q", got)
	}
}

// fakeRelay greets every connection with a host and drops the first one, so
// Hold has to reconnect.
func fakeRelay(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" || r.URL.Query().Get("service") != "app" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		n := conns.Add(1)
		_ = c.WriteMessage(websocket.TextMessage, []byte("host:app-abc123"))
		if n == 1 {
			c.Close() // the relay restarts
			return
		}
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &conns
}

func TestHoldReconnectsAfterADrop(t *testing.T) {
	srv, conns := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	opt := Options{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/_ws", APIKey: "key", Service: "app", Target: "3000"}
	host, done, err := Hold(ctx, opt, t.Logf)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if host != "app-abc123" {
		t.Fatalf("host = %q", host)
	}
	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if conns.Load() < 2 {
		t.Fatal("Hold did not reconnect after the relay dropped the tunnel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hold did not stop when its context ended")
	}
}

func TestDialReportsTheRelaysReason(t *testing.T) {
	srv, _ := fakeRelay(t)
	_, err := Dial(context.Background(), Options{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/_ws", APIKey: "wrong", Service: "app", Target: "3000"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want the relay's reason", err)
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]string{"3000": "localhost:3000", "localhost:8000": "localhost:8000", "http://127.0.0.1:9/x": "127.0.0.1:9"} {
		if got := ParseTarget(in); got != want {
			t.Errorf("ParseTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
