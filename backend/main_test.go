package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"roxey-relay/internal/relay"
	"roxey-relay/internal/store"
)

// TestTunnelPipe covers the full relay path: a fake CLI connects over
// websocket (as the `_run` worker would), registers service "shop", and a
// public HTTP request to shop.<domain> comes back from the local target.
func TestTunnelPipe(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := relay.NewRegistry()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello-from-target:" + r.URL.Path))
	}))
	defer backend.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWSUpgrade(upgrader, reg, st, w, r)
	}))
	defer relaySrv.Close()

	// Fake worker: connect like tunnel.Run does.
	wsURL := "ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/_ws?service=shop&path="
	key, _, err := st.CreateAPIKey("test")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{"Authorization": {"Bearer " + key}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial ws: %v (%s)", err, resp.Status)
	}
	defer conn.Close()

	sess, err := yamux.Client(relay.NewWSNetConn(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			stream, err := sess.Accept()
			if err != nil {
				return
			}
			local, err := net.Dial("tcp", strings.TrimPrefix(backend.URL, "http://"))
			if err != nil {
				stream.Close()
				continue
			}
			go io.Copy(local, stream)
			go io.Copy(stream, local)
		}
	}()

	// Public request routed through the relay handler.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "shop.example.test"
		handleTunnelRequest(reg, "shop", w, r)
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/path", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello-from-target:/some/path" {
		t.Fatalf("body = %q", got)
	}
}

func TestDuplicateRegistrationRejected(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := relay.NewRegistry()

	if err := reg.Check("svc", ""); err != nil {
		t.Fatalf("free service should check ok: %v", err)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWSUpgrade(upgrader, reg, st, w, r)
	}))
	defer srv.Close()

	key, _, err := st.CreateAPIKey("k")
	if err != nil {
		t.Fatal(err)
	}
	dial := func() (*websocket.Conn, *http.Response, error) {
		u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/_ws?service=svc&path="
		return websocket.DefaultDialer.Dial(u,
			http.Header{"Authorization": {"Bearer " + key}})
	}

	conn1, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	_, resp, err := dial()
	if err == nil {
		t.Fatal("second registration should be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 conflict, got %v", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "already in use") {
		t.Fatalf("reason not surfaced: %q", body)
	}
}
