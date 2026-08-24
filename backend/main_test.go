package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"roxey-relay/internal/auth"
	"roxey-relay/internal/relay"
	"roxey-relay/internal/store"
)

const testDomain = "example.test"

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func makeUserWithKey(t *testing.T, st *store.Store, email string) (userID string, key string) {
	t.Helper()
	u, err := st.CreateUser(email, "$2a$10$placeholderhashplaceholderhashplaceholderhash")
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := st.CreateAPIKey(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, plain
}

// startFakeTarget returns an upstream HTTP server standing in for the CLI's
// local target, plus a yamux bridge wired to relaySrv like `_run` would be.
// readAssignedHost consumes the relay's first websocket message, which
// carries the public host assigned to this tunnel ("host:<host>").
func readAssignedHost(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read assigned host: %v", err)
	}
	host, ok := strings.CutPrefix(string(msg), "host:")
	if !ok || host == "" {
		t.Fatalf("unexpected first message %q", msg)
	}
	return host
}

// dialTunnel connects like the `_run` worker and returns the live session
// plus the host the relay assigned.
func dialTunnel(t *testing.T, relaySrv *httptest.Server, key, service string) (*websocket.Conn, string) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/_ws?service=" + service + "&path="
	hdr := http.Header{"Authorization": {"Bearer " + key}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial ws: %v (%s)", err, resp.Status)
	}
	return conn, readAssignedHost(t, conn)
}

// TestTunnelPipe covers the full relay path: a fake CLI connects over
// websocket (as the `_run` worker would), registers a service on its
// namespaced host, and a public HTTP request comes back from the target.
func TestTunnelPipe(t *testing.T) {
	st := testStore(t)
	reg := relay.NewRegistry()
	sessions := auth.NewSessions("test-secret")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello-from-target:" + r.URL.Path))
	}))
	defer backend.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWSUpgrade(upgrader, reg, st, sessions, testDomain, false, w, r)
	}))
	defer relaySrv.Close()

	userID, key := makeUserWithKey(t, st, "a@x.test")
	conn, host := dialTunnel(t, relaySrv, key, "shop")
	defer conn.Close()
	if !strings.HasPrefix(host, "shop-") || host == tunnelHost(false, userID, "other", "") {
		t.Fatalf("unexpected namespaced host %q", host)
	}

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

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = host + "." + testDomain
		handleTunnelRequest(reg, sessions, auth.NewLimiter(time.Minute, 100), host, w, r)
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

// TestSingleUserHostsAreBare verifies SINGLE_USER mode skips suffixes.
func TestSingleUserHostsAreBare(t *testing.T) {
	if got := tunnelHost(true, "u1", "app", "dev"); got != "app" {
		t.Fatalf("single-user host = %q, want app", got)
	}
	a := tunnelHost(false, "user-a", "shop", "")
	b := tunnelHost(false, "user-b", "shop", "")
	if a == b || !strings.HasPrefix(a, "shop-") {
		t.Fatalf("namespaced hosts must differ per user: %q vs %q", a, b)
	}
	c := tunnelHost(false, "user-a", "other", "")
	if c == a {
		t.Fatal("same user, different services must get different hosts")
	}
}

// TestDuplicateRegistrationRejected ensures two tunnels can't claim one host.
func TestDuplicateRegistrationRejected(t *testing.T) {
	st := testStore(t)
	reg := relay.NewRegistry()
	sessions := auth.NewSessions("test-secret")

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWSUpgrade(upgrader, reg, st, sessions, testDomain, true, w, r)
	}))
	defer srv.Close()

	_, key := makeUserWithKey(t, st, "k@x.test")
	dial := func() (*websocket.Conn, *http.Response, error) {
		u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/_ws?service=svc&path="
		return websocket.DefaultDialer.Dial(u,
			http.Header{"Authorization": {"Bearer " + key}})
	}

	conn1, resp1, err := dial()
	if err != nil {
		t.Fatalf("first dial: %v (%s)", err, resp1.Status)
	}
	t.Cleanup(func() { conn1.Close() })

	_, resp2, err := dial()
	if err == nil {
		t.Fatal("second registration should be rejected")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 conflict, got %v", resp2)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "already in use") {
		t.Fatalf("reason not surfaced: %q", body)
	}
}

// ── dashboard auth + scoping ─────────────────────────────────────────────

type apiClient struct {
	t       *testing.T
	base    string
	cookies []*http.Cookie
}

func (c *apiClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	for _, ck := range res.Cookies() {
		c.cookies = append(c.cookies, ck)
	}
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func newDashboard(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st := testStore(t)
	mux := buildMux(buildMuxOpts{
		reg:           relay.NewRegistry(),
		st:            st,
		sessions:      auth.NewSessions("test-secret"),
		domain:        testDomain,
		signupLimiter: auth.NewLimiter(time.Minute, 1000),
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return st, srv
}

func TestSignupLoginKeyFlow(t *testing.T) {
	_, srv := newDashboard(t)

	alina := &apiClient{t: t, base: srv.URL}
	code, body := alina.do(http.MethodPost, "/api/auth/signup", map[string]string{
		"email": "Alina@Example.com ", "password": "hunter22345"})
	if code != http.StatusOK {
		t.Fatalf("signup: %d %s", code, body)
	}
	if !strings.Contains(string(body), "alina@example.com") {
		t.Fatalf("email not normalized: %s", body)
	}

	// Signup response carries the session cookie; keys are created after.
	code, body = alina.do(http.MethodPost, "/api/keys", map[string]string{"label": "laptop"})
	if code != http.StatusOK {
		t.Fatalf("create key: %d %s", code, body)
	}
	var created struct{ ID, APIKey string }
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	code, body = alina.do(http.MethodGet, "/api/keys", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "laptop") {
		t.Fatalf("list keys: %d %s", code, body)
	}

	// Login works independently of the session from signup.
	bob := &apiClient{t: t, base: srv.URL}
	code, body = bob.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": "alina@example.com", "password": "wrong-password"})
	if code != http.StatusUnauthorized {
		t.Fatalf("bad login should fail: %d %s", code, body)
	}
	code, _ = bob.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": "alina@example.com", "password": "hunter22345"})
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	code, body = bob.do(http.MethodGet, "/api/me", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "alina") {
		t.Fatalf("me after login: %d %s", code, body)
	}
}

func TestSignupValidationAndDisabledMode(t *testing.T) {
	st, srv := newDashboard(t)

	client := &apiClient{t: t, base: srv.URL}
	code, _ := client.do(http.MethodPost, "/api/auth/signup", map[string]string{
		"email": "not-an-email", "password": "longenough1"})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid email: %d", code)
	}
	code, _ = client.do(http.MethodPost, "/api/auth/signup", map[string]string{
		"email": "ok@ok.test", "password": "short"})
	if code != http.StatusBadRequest {
		t.Fatalf("weak password: %d", code)
	}

	// SINGLE_USER relays disable open signup entirely.
	st.SeedSingleUser("boss@local.test", "$2a$10$anotherplaceholderhashxxxxxxxxxxxxxxxxxxxxxx")
	singleMux := buildMux(buildMuxOpts{
		reg: relay.NewRegistry(), st: st,
		sessions:      auth.NewSessions("test-secret"),
		domain:        testDomain,
		singleUser:    true,
		signupLimiter: auth.NewLimiter(time.Minute, 1000),
	})
	singleSrv := httptest.NewServer(singleMux)
	t.Cleanup(singleSrv.Close)

	code, body := (&apiClient{t: t, base: singleSrv.URL}).do(
		http.MethodPost, "/api/auth/signup", map[string]string{
			"email": "x@y.test", "password": "longenough1"})
	if code != http.StatusForbidden {
		t.Fatalf("signup on single-user relay: %d %s", code, body)
	}

	// The signup page itself is gone too — /signup bounces to /login.
	nofollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := nofollow.Get(singleSrv.URL + "/signup")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("single-user /signup => %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// And the login page renders without the signup hint.
	lres, err := nofollow.Get(singleSrv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	lb, _ := io.ReadAll(lres.Body)
	lres.Body.Close()
	if strings.Contains(string(lb), "/signup") {
		t.Fatal("login page still advertises signup on a single-user relay")
	}
}

func TestCrossUserDataIsolation(t *testing.T) {
	st, srv := newDashboard(t)

	a := &apiClient{t: t, base: srv.URL}
	b := &apiClient{t: t, base: srv.URL}
	a.do(http.MethodPost, "/api/auth/signup", map[string]string{"email": "a@x.test", "password": "password11"})
	b.do(http.MethodPost, "/api/auth/signup", map[string]string{"email": "b@x.test", "password": "password22"})

	code, body := a.do(http.MethodPost, "/api/keys", map[string]string{"label": "a-key"})
	if code != http.StatusOK {
		t.Fatalf("key create: %d %s", code, body)
	}
	var created struct{ ID string }
	json.Unmarshal(body, &created)

	// Bob must neither see nor revoke Alice's key...
	code, body = b.do(http.MethodGet, "/api/keys", nil)
	if code != http.StatusOK || strings.Contains(string(body), "a-key") {
		t.Fatalf("bob sees alice keys: %d %s", code, body)
	}
	code, _ = b.do(http.MethodDelete, "/api/keys/"+created.ID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("bob revoking alice key: %d", code)
	}

	// ...and events are scoped too.
	if _, err := st.RecordConnect("user-a-id", "svc", "svc-x.host", "", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	// Use alice's real id from her session.
	code, meBody := a.do(http.MethodGet, "/api/me", nil)
	if code != http.StatusOK {
		t.Fatalf("me: %d %s", code, meBody)
	}
	var me struct{ ID string }
	json.Unmarshal(meBody, &me)
	st.RecordConnect(me.ID, "alice-svc", "h", "", "1.2.3.4")

	_, body = b.do(http.MethodGet, "/api/tunnel-events", nil)
	if strings.Contains(string(body), "alice-svc") {
		t.Fatalf("bob sees alice tunnel events: %s", body)
	}
	_, body = b.do(http.MethodGet, "/api/tunnels", nil)
	if strings.Contains(string(body), "alice-svc") {
		t.Fatalf("bob sees alice active tunnels: %s", body)
	}
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	_, srv := newDashboard(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("unauthenticated / => %d %q", res.StatusCode, loc)
	}
}

// ── protected-tunnel gate ────────────────────────────────────────────────

func TestGateFlow(t *testing.T) {
	st := testStore(t)
	reg := relay.NewRegistry()
	sessions := auth.NewSessions("gate-test-secret")
	limiter := auth.NewLimiter(time.Minute, 100)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret-app"))
	}))
	defer backend.Close()

	userID, _ := makeUserWithKey(t, st, "g@x.test")
	host := tunnelHost(false, userID, "locked", "")

	// Wire a yamux server/client pair across an in-memory pipe; the client
	// side plays the CLI worker bridging streams to the fake target.
	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	defer cliConn.Close()
	yamuxSrv, err := yamux.Server(srvConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	yamuxCli, err := yamux.Client(cliConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := reg.Register(&relay.Entry{
		Service: "locked", Host: host, UserID: userID,
		ProtectHash: sha256Hex("letmein"),
	}, "", yamuxSrv, "9.9.9.9:1234")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			stream, err := yamuxCli.Accept()
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
	t.Cleanup(func() { yamuxSrv.Close(); yamuxCli.Close() })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = host + "." + testDomain
		handleTunnelRequest(reg, sessions, limiter, host, w, r)
	})

	// Unauthenticated visitor gets the gate page, not app bytes.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gate status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-app") {
		t.Fatal("gate leaked app content")
	}

	// Wrong password re-renders the form with denial.
	rec = httptest.NewRecorder()
	wrong := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("password=nope"))
	wrong.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, wrong)
	if !strings.Contains(rec.Body.String(), "access denied") {
		t.Fatalf("wrong password response: %s", rec.Body.String())
	}

	// Correct password unlocks via redirect + signed cookie.
	rec = httptest.NewRecorder()
	right := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("password=letmein"))
	right.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, right)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unlock status = %d", rec.Code)
	}
	var gate *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.GateCookie {
			gate = c
		}
	}
	if gate == nil {
		t.Fatal("no unlock cookie set")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(gate)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "secret-app" {
		t.Fatalf("cookie unlock failed: %d %q", rec.Code, rec.Body.String())
	}

	// Basic-auth header bypasses the browser flow for curl/scripts.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("ignored", "letmein")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("basic-auth unlock failed: %d", rec.Code)
	}

	// ?access_token= works too; a wrong token does not.
	req = httptest.NewRequest(http.MethodGet, "/?access_token=letmein", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token unlock failed: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/?access_token=nope", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token accepted: %d", rec.Code)
	}
	_ = entry
}

// sha256Hex is a test helper mirroring main.go's protect hashing.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
