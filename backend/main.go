// Command roxey-relay is the public relay: it terminates HTTP for *.{domain}
// tunnel subdomains and pipes each connection as a raw TCP stream (multiplexed
// with yamux over the CLI's websocket) to the connected CLI's local target,
// and serves a dashboard on the admin host where accounts sign up, manage
// their API keys, watch live tunnels, and lock previews behind a shared
// secret.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"roxey-relay/internal/auth"
	"roxey-relay/internal/relay"
	"roxey-relay/internal/store"
)

//go:embed web
var webFS embed.FS

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	domain := env("ROXEY_DOMAIN", "f43.run")
	adminHost := env("ROXEY_ADMIN_HOST", "roxey."+domain)
	dbPath := env("ROXEY_DB_PATH", "roxey.db")
	port := env("PORT", "8080")

	singleUser := env("ROXEY_SINGLE_USER", "") == "1"
	adminEmail := strings.ToLower(env("ROXEY_ADMIN_EMAIL", ""))
	adminPassword := env("ROXEY_ADMIN_PASSWORD", "")
	if singleUser && (adminEmail == "" || adminPassword == "") {
		log.Fatal("ROXEY_SINGLE_USER=1 requires ROXEY_ADMIN_EMAIL and ROXEY_ADMIN_PASSWORD")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if singleUser {
		hash, err := auth.HashPassword(adminPassword)
		if err != nil {
			log.Fatalf("hash admin password: %v", err)
		}
		if err := st.SeedSingleUser(adminEmail, hash); err != nil {
			log.Fatalf("seed single-user account: %v", err)
		}
	}

	sessions := auth.NewSessions(loadSessionSecret(dbPath))

	reg := relay.NewRegistry()
	signupLimiter := auth.NewLimiter(time.Minute, 10)
	gateLimiter := auth.NewLimiter(time.Minute, 30)

	mux := buildMux(buildMuxOpts{
		reg:           reg,
		st:            st,
		sessions:      sessions,
		domain:        domain,
		singleUser:    singleUser,
		signupLimiter: signupLimiter,
	})

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i != -1 {
			host = host[:i]
		}

		if r.URL.Path == "/healthz" { // unauthenticated readiness probe
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}

		if host == adminHost {
			if r.URL.Path == "/_ws" {
				handleWSUpgrade(upgrader, reg, st, sessions, domain, singleUser, w, r)
				return
			}
			mux.ServeHTTP(w, r)
			return
		}

		if !strings.HasSuffix(host, "."+domain) {
			renderRelayError(w, http.StatusNotFound, "unknown host", "This address is not a roxey tunnel.")
			return
		}
		label := strings.TrimSuffix(host, "."+domain)
		handleTunnelRequest(reg, sessions, gateLimiter, label, w, r)
	})

	tlsCert, tlsKey := os.Getenv("ROXEY_TLS_CERT"), os.Getenv("ROXEY_TLS_KEY")
	addr := ":" + port
	if tlsCert != "" && tlsKey != "" {
		// Serve certs via a reloading getter: the local CA re-issues the
		// leaf whenever a project adds a host, and the running relay should
		// pick that up without a restart.
		loader := &certLoader{certFile: tlsCert, keyFile: tlsKey}
		log.Printf("roxey relay listening on https://%s (admin host: %s, domain: *.%s)", addr, adminHost, domain)
		srv := &http.Server{
			Addr:    addr,
			Handler: handler,
			TLSConfig: &tls.Config{
				GetCertificate: loader.GetCertificate,
			},
		}
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}
	log.Printf("roxey relay listening on http://%s (admin host: %s, domain: *.%s)", addr, adminHost, domain)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// certLoader serves the TLS keypair from disk, re-reading it when the
// files' modification time changes.
type certLoader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cached  *tls.Certificate
	modTime time.Time
}

func (c *certLoader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fi, err := os.Stat(c.certFile)
	if err == nil && c.cached != nil && fi.ModTime().Equal(c.modTime) {
		return c.cached, nil
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		if c.cached != nil {
			return c.cached, nil // keep serving the previous cert on errors
		}
		return nil, err
	}
	c.cached, c.modTime = &cert, fi.ModTime()
	return c.cached, nil
}

// loadSessionSecret returns ROXEY_SESSION_SECRET or a random secret
// persisted next to the database so sessions survive restarts.
func loadSessionSecret(dbPath string) string {
	if s := os.Getenv("ROXEY_SESSION_SECRET"); s != "" {
		return s
	}
	path := dbPath + ".secret"
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b)
	}
	secret := auth.RandomSecret(32)
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		log.Fatalf("persist session secret: %v", err)
	}
	return secret
}

type buildMuxOpts struct {
	reg           *relay.Registry
	st            *store.Store
	sessions      *auth.Sessions
	domain        string
	singleUser    bool
	signupLimiter *auth.Limiter
}

func buildMux(o buildMuxOpts) http.Handler {
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web: %v", err)
	}
	fileServer := http.FileServer(http.FS(webRoot))

	mux := http.NewServeMux()

	// The dashboard shell requires a session; unauthenticated browsers are
	// bounced to /login. Static assets and the login/signup pages stay open.
	// Auth pages are templates so single-user relays can drop signup UI.
	loginTmpl := template.Must(template.ParseFS(webRoot, "login.html"))
	signupTmpl := template.Must(template.ParseFS(webRoot, "signup.html"))
	pageData := map[string]any{"SingleUser": o.singleUser}

	servePage := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(webRoot, name)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
		}
	}
	renderAuthPage := func(tmpl *template.Template) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.Execute(w, pageData)
		}
	}
	authPage := func(tmpl *template.Template) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(auth.SessionCookie); err == nil {
				if _, err := o.sessions.Verify(c.Value); err == nil {
					http.Redirect(w, r, "/", http.StatusSeeOther)
					return
				}
			}
			renderAuthPage(tmpl)(w, r)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			fileServer.ServeHTTP(w, r) // assets (common.css) + 404s
			return
		}
		c, err := r.Cookie(auth.SessionCookie)
		if err != nil {
			unauthorized(w, r)
			return
		}
		if _, err := o.sessions.Verify(c.Value); err != nil {
			auth.ClearSessionCookie(w)
			unauthorized(w, r)
			return
		}
		servePage("index.html")(w, r)
	})
	mux.Handle("/login", authPage(loginTmpl))
	if o.singleUser {
		// No accounts to create here; send the curious to the login page.
		mux.HandleFunc("/signup", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		})
	} else {
		mux.Handle("/signup", authPage(signupTmpl))
	}

	mux.HandleFunc("/api/auth/signup", func(w http.ResponseWriter, r *http.Request) {
		if o.singleUser {
			http.Error(w, "signup is disabled on this relay; ask the operator for an account", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ip := auth.ClientIP(r)
		if !o.signupLimiter.Allow(ip) {
			http.Error(w, "too many attempts, slow down", http.StatusTooManyRequests)
			return
		}
		var body struct{ Email, Password string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))
		if !validEmail(body.Email) {
			http.Error(w, "enter a valid email address", http.StatusBadRequest)
			return
		}
		if len(body.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		if _, _, exists, err := o.st.GetUserByEmail(body.Email); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if exists {
			http.Error(w, "an account with this email already exists", http.StatusConflict)
			return
		}
		hash, err := auth.HashPassword(body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		user, err := o.st.CreateUser(body.Email, hash)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		auth.SetSessionCookie(w, o.sessions.Issue(user.ID))
		writeJSON(w, http.StatusOK, userResponse(user.ID, body.Email))
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Email, Password string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		user, hash, ok, err := o.st.GetUserByEmail(strings.ToLower(strings.TrimSpace(body.Email)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok || !auth.CheckPassword(hash, body.Password) {
			http.Error(w, "wrong email or password", http.StatusUnauthorized)
			return
		}
		auth.SetSessionCookie(w, o.sessions.Issue(user.ID))
		writeJSON(w, http.StatusOK, userResponse(user.ID, user.Email))
	})

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		auth.ClearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	})

	// Everything below requires a session.
	sessioned := func(next func(w http.ResponseWriter, r *http.Request, userID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(auth.SessionCookie)
			if err != nil {
				unauthorized(w, r)
				return
			}
			userID, err := o.sessions.Verify(c.Value)
			if err != nil {
				auth.ClearSessionCookie(w)
				unauthorized(w, r)
				return
			}
			next(w, r, userID)
		}
	}

	mux.HandleFunc("/api/me", sessioned(func(w http.ResponseWriter, r *http.Request, userID string) {
		u, ok, err := o.st.GetUserByID(userID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			auth.ClearSessionCookie(w)
			unauthorized(w, r)
			return
		}
		writeJSON(w, http.StatusOK, userResponse(u.ID, u.Email))
	}))

	mux.HandleFunc("/api/keys", sessioned(func(w http.ResponseWriter, r *http.Request, userID string) {
		switch r.Method {
		case http.MethodGet:
			keys, err := o.st.ListAPIKeys(userID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, keys)
		case http.MethodPost:
			var body struct {
				Label string `json:"label"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			plain, key, err := o.st.CreateAPIKey(userID, body.Label)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": key.ID, "label": key.Label, "createdAt": key.CreatedAt, "apiKey": plain,
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/keys/", sessioned(func(w http.ResponseWriter, r *http.Request, userID string) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
		ok, err := o.st.RevokeAPIKey(userID, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// Hostname lookup for CLIs holding an API key: maps requested service
	// names to their public hosts so `roxey up` can print real URLs.
	mux.HandleFunc("/api/lookup", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := o.st.ValidateAPIKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		out := map[string]string{}
		for _, s := range strings.Split(r.URL.Query().Get("services"), ",") {
			s = strings.TrimSpace(s)
			if err := relay.ValidateService(s); err != nil {
				continue
			}
			out[s] = tunnelHost(o.singleUser, userID, s, o.domain)
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("/api/tunnels", sessioned(func(w http.ResponseWriter, r *http.Request, userID string) {
		writeJSON(w, http.StatusOK, o.reg.ListActive(userID))
	}))

	mux.HandleFunc("/api/tunnel-events", sessioned(func(w http.ResponseWriter, r *http.Request, userID string) {
		events, err := o.st.ListTunnelEvents(userID, 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, events)
	}))

	return mux
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// looseServiceRe accepts multi-label service names (app.proja) for
// SINGLE_USER relays whose hosts are bare <service>.<tld>.
var looseServiceRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,58}[a-z0-9])?$`)

func validEmail(s string) bool { return emailRe.MatchString(s) }

func unauthorized(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// wantsHTML guesses whether the caller is a browser navigating (vs an API
// client) so unauthenticated dashboard hits redirect to /login.
func wantsHTML(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func userResponse(id, email string) map[string]any {
	return map[string]any{"id": id, "email": email}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tunnelHost derives the public host for a tunnel. Hosted multi-user relays
// namespace every service with a short per-tunnel suffix derived from the
// account + service pair, making each URL independently unguessable.
// SINGLE_USER relays skip the suffix for clean dev URLs like app.dev.
func tunnelHost(singleUser bool, userID, service, domain string) string {
	if singleUser {
		return service
	}
	sum := sha256.Sum256([]byte(userID + "|" + service))
	suffix := hex.EncodeToString(sum[:])[:6]
	return service + "-" + suffix
}

func handleWSUpgrade(upgrader websocket.Upgrader, reg *relay.Registry, st *store.Store, sessions *auth.Sessions, domain string, singleUser bool, w http.ResponseWriter, r *http.Request) {
	apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	userID, ok := st.ValidateAPIKey(apiKey)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	service := r.URL.Query().Get("service")
	pathPrefix := r.URL.Query().Get("path")
	if singleUser {
		// Local relays serve bare <service>.<tld>, and existing projects
		// use multi-label names like app.proja — accept those.
		if !looseServiceRe.MatchString(service) {
			http.Error(w, "invalid service name", http.StatusBadRequest)
			return
		}
	} else if err := relay.ValidateService(service); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host := tunnelHost(singleUser, userID, service, domain)

	protectHash := ""
	if secret := r.URL.Query().Get("protect"); secret != "" {
		sum := sha256.Sum256([]byte(secret))
		protectHash = hex.EncodeToString(sum[:])
	}

	if err := reg.Check(host, pathPrefix); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	if err := reg.Check(host, pathPrefix); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Gorilla strips custom headers from 101 responses, so the assigned
	// public host travels as the first websocket message instead. The CLI
	// must consume it before starting its yamux client.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("host:"+host)); err != nil {
		conn.Close()
		return
	}

	// Wrap the websocket as a stream multiplexer: every public connection is
	// a raw byte stream inside this one session.
	sess, err := yamux.Server(relay.NewWSNetConn(conn), nil)
	if err != nil {
		conn.Close()
		return
	}

	entry, err := reg.Register(&relay.Entry{
		Service:     service,
		Host:        host,
		UserID:      userID,
		ProtectHash: protectHash,
	}, pathPrefix, sess, r.RemoteAddr)
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, err.Error()), time.Now().Add(time.Second))
		sess.Close()
		conn.Close()
		return
	}

	eventID, err := st.RecordConnect(userID, service, host, pathPrefix, r.RemoteAddr)
	if err != nil {
		log.Printf("record connect: %v", err)
	}
	defer func() {
		reg.Unregister(host, pathPrefix, entry)
		sess.Close()
		conn.Close()
		if eventID != "" {
			if err := st.RecordDisconnect(eventID); err != nil {
				log.Printf("record disconnect: %v", err)
			}
		}
	}()

	// Block until the CLI's session (or the websocket) drops. Streams are
	// opened on demand by handleTunnelRequest; anything the CLI opens back
	// is closed immediately.
	for {
		stream, err := sess.Accept()
		if err != nil {
			return
		}
		stream.Close()
	}
}

func handleTunnelRequest(reg *relay.Registry, sessions *auth.Sessions, gateLimiter *auth.Limiter, host string, w http.ResponseWriter, r *http.Request) {
	entry := reg.Resolve(host, r.URL.Path)
	if entry == nil {
		renderRelayError(w, http.StatusBadGateway, "no active tunnel",
			"No tunnel is currently connected for "+host+". Start one with `roxey up`.")
		return
	}

	// Gated previews check the visitor before any bytes reach the app.
	if entry.ProtectHash != "" && !visitorAllowed(sessions, entry, r) {
		ip := auth.ClientIP(r)
		if r.Method == http.MethodPost && !gateLimiter.Allow(ip) {
			renderRelayError(w, http.StatusTooManyRequests, "slow down", "Too many unlock attempts. Try again in a minute.")
			return
		}
		serveGate(sessions, entry, w, r)
		return
	}

	// Each public connection gets a fresh raw stream through the tunnel;
	// the CLI pipes it straight to the local target's TCP port, so any
	// protocol on top of HTTP (websockets, SSE, chunked uploads...) passes
	// through untouched.
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "tunneled"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return entry.OpenStream()
			},
			ResponseHeaderTimeout: 30 * time.Second,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			renderRelayError(w, http.StatusBadGateway, "tunnel error", err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// visitorAllowed checks the three ways through a gated preview: the unlock
// cookie, HTTP Basic credentials, or an access_token query param.
func visitorAllowed(sessions *auth.Sessions, entry *relay.Entry, r *http.Request) bool {
	if c, err := r.Cookie(auth.GateCookie); err == nil && sessions.VerifyGate(c.Value, entry.Host) {
		return true
	}
	if u, p, ok := r.BasicAuth(); ok && checkGateSecret(entry.ProtectHash, p, u) {
		return true
	}
	if token := r.URL.Query().Get("access_token"); token != "" && checkGateSecret(entry.ProtectHash, token, "token") {
		return true
	}
	return false
}

// checkGateSecret compares a presented secret against the stored hash in
// constant time. The username field is ignored (kept for curl convenience).
func checkGateSecret(hash, secret, _ string) bool {
	sum := sha256.Sum256([]byte(secret))
	presented := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(presented), []byte(hash)) == 1
}

// serveGate renders the password page (GET) or verifies a submitted password
// and unlocks the preview via a signed cookie (POST).
func serveGate(sessions *auth.Sessions, entry *relay.Entry, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			renderRelayError(w, http.StatusBadRequest, "bad request", "could not parse form")
			return
		}
		if checkGateSecret(entry.ProtectHash, r.PostFormValue("password"), "") {
			// Token is bound to the tunnel's internal label; the cookie
			// itself must carry the full FQDN from the request or browsers
			// will reject the domain.
			auth.SetGateCookie(w, sessions.IssueGate(entry.Host), stripPort(r.Host))
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
		renderGate(w, entry.Host, true)
		return
	}
	if c, err := r.Cookie(auth.GateCookie); err == nil && sessions.VerifyGate(c.Value, entry.Host) {
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	renderGate(w, entry.Host, false)
}

func stripPort(host string) string {
	if i := strings.IndexByte(host, ':'); i != -1 {
		return host[:i]
	}
	return host
}

func renderGate(w http.ResponseWriter, host string, denied bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = gateTemplate.Execute(w, map[string]any{"Host": host, "Denied": denied})
}

var gateTemplate = template.Must(template.New("gate").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Protected — roxey</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>` + commonCSS + `
.panel { max-width: 380px; margin: 18vh auto 0; text-align: center; }
.lock { color: var(--amber); font-size: 12px; letter-spacing: .2em; margin-bottom: 14px; }
.host { color: var(--dim); font-size: 12px; word-break: break-all; margin-bottom: 24px; }
input[type=password] { width: 100%; box-sizing: border-box; }
.denied { color: var(--red); font-size: 12px; margin-top: 12px; min-height: 16px; }
</style>
</head>
<body>
<main class="panel panel-box">
  <div class="lock">[ LOCKED ]</div>
  <h1 class="wordmark">roxey<span class="cursor">▊</span></h1>
  <p class="host">{{.Host}}</p>
  <form method="post">
    <label for="password">PASSWORD</label>
    <input id="password" name="password" type="password" autofocus autocomplete="current-password">
    <button type="submit">[ unlock ]</button>
  </form>
  {{if .Denied}}<p class="denied">! access denied — wrong password</p>{{end}}
</main>
</body>
</html>
`))

// renderRelayError answers public tunnel-host failures with the same visual
// language as everything else instead of Go's plaintext defaults.
func renderRelayError(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = errorTemplate.Execute(w, map[string]any{"Title": title, "Message": msg})
}

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}} — roxey</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>` + commonCSS + `
.panel { max-width: 440px; margin: 18vh auto 0; text-align: center; }
.code { color: var(--red); font-size: 12px; letter-spacing: .2em; margin-bottom: 14px; }
.msg { color: var(--dim); font-size: 13px; margin: 18px 0 0; word-break: break-word; }
</style>
</head>
<body>
<main class="panel panel-box">
  <div class="code">[ ERROR ]</div>
  <h1 class="wordmark">roxey<span class="cursor">▊</span></h1>
  <h2 style="margin-top:10px;">{{.Title}}</h2>
  <p class="msg">{{.Message}}</p>
</main>
</body>
</html>
`))

// commonCSS holds the design tokens shared by the embedded Go templates;
// web/common.css mirrors it for the static pages.
const commonCSS = `:root {
  --bg: #0d0f0c;
  --panel: #12150f;
  --line: #2a2e28;
  --text: #d8d8d0;
  --dim: #7a7f76;
  --green: #5af78e;
  --amber: #f2b632;
  --red: #e5534b;
}
* { box-sizing: border-box; }
html, body { background: var(--bg); color: var(--text); }
body {
  font: 13px/1.6 ui-monospace, "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace;
  margin: 0; padding: 24px 20px 48px;
}
a { color: var(--green); text-decoration: none; }
a:hover { text-decoration: underline; }
.wordmark { font-size: 20px; font-weight: 600; letter-spacing: .02em; margin: 0 0 4px; }
.cursor { animation: blink 1.1s steps(1) infinite; color: var(--green); }
@keyframes blink { 50% { opacity: 0; } }
.panel-box {
  border: 1px solid var(--line);
  background: var(--panel);
  padding: 28px 26px;
}
label { display: block; color: var(--dim); font-size: 11px; letter-spacing: .12em; margin: 16px 0 4px; text-transform: uppercase; }
input {
  background: var(--bg); border: 1px solid var(--line); color: var(--text);
  font: inherit; padding: 8px 10px; outline: none; width: 100%;
}
input:focus { border-color: var(--green); }
button {
  background: transparent; border: none; color: var(--green); cursor: pointer;
  font: inherit; padding: 0;
}
button:hover { color: var(--bg); background: var(--green); }
button.danger { color: var(--red); }
button.danger:hover { background: var(--red); color: var(--bg); }
.btn-solid {
  border: 1px solid var(--line); padding: 8px 14px; margin-top: 18px;
}
.btn-solid:hover { background: var(--green); border-color: var(--green); color: var(--bg); }
`
