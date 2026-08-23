// Command roxey-relay is the public relay: it terminates HTTP for *.{domain}
// tunnel subdomains and pipes each connection as a raw TCP stream (multiplexed
// with yamux over the CLI's websocket) to the connected CLI's local target,
// and serves a Basic-Auth-protected REST API + dashboard on the admin host
// for managing API keys and viewing live tunnels.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
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
	adminHost := env("ROXEY_ADMIN_HOST", "relay."+domain)
	adminUser := os.Getenv("ROXEY_ADMIN_USER")
	adminPass := os.Getenv("ROXEY_ADMIN_PASS")
	dbPath := env("ROXEY_DB_PATH", "roxey.db")
	port := env("PORT", "8080")

	if adminUser == "" || adminPass == "" {
		log.Fatal("ROXEY_ADMIN_USER and ROXEY_ADMIN_PASS must be set")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	reg := relay.NewRegistry()
	adminHandler := auth.BasicAuth(adminUser, adminPass, buildMux(reg, st))

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
				handleWSUpgrade(upgrader, reg, st, w, r)
				return
			}
			adminHandler.ServeHTTP(w, r)
			return
		}

		if !strings.HasSuffix(host, "."+domain) {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		service := strings.TrimSuffix(host, "."+domain)
		handleTunnelRequest(reg, service, w, r)
	})

	tlsCert, tlsKey := os.Getenv("ROXEY_TLS_CERT"), os.Getenv("ROXEY_TLS_KEY")
	addr := ":" + port
	if tlsCert != "" && tlsKey != "" {
		log.Printf("roxey relay listening on https://%s (admin host: %s, domain: *.%s)", addr, adminHost, domain)
		log.Fatal(http.ListenAndServeTLS(addr, tlsCert, tlsKey, handler))
	}
	log.Printf("roxey relay listening on http://%s (admin host: %s, domain: *.%s)", addr, adminHost, domain)
	log.Fatal(http.ListenAndServe(addr, handler))
}

func buildMux(reg *relay.Registry, st *store.Store) http.Handler {
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	mux.HandleFunc("/api/tunnels", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, reg.ListActive())
	})

	mux.HandleFunc("/api/tunnel-events", func(w http.ResponseWriter, r *http.Request) {
		events, err := st.ListTunnelEvents(100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, events)
	})

	mux.HandleFunc("/api/keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			keys, err := st.ListAPIKeys()
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
			plain, key, err := st.CreateAPIKey(body.Label)
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
	})

	mux.HandleFunc("/api/keys/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
		ok, err := st.RevokeAPIKey(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func handleWSUpgrade(upgrader websocket.Upgrader, reg *relay.Registry, st *store.Store, w http.ResponseWriter, r *http.Request) {
	apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !st.ValidateAPIKey(apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	service := r.URL.Query().Get("service")
	pathPrefix := r.URL.Query().Get("path")
	if service == "" {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}
	if err := reg.Check(service, pathPrefix); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Wrap the websocket as a stream multiplexer: every public connection is
	// a raw byte stream inside this one session.
	sess, err := yamux.Server(relay.NewWSNetConn(conn), nil)
	if err != nil {
		conn.Close()
		return
	}

	entry, err := reg.Register(service, pathPrefix, sess, r.RemoteAddr)
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, err.Error()), time.Now().Add(time.Second))
		conn.Close()
		return
	}
	eventID, err := st.RecordConnect(service, pathPrefix, r.RemoteAddr)
	if err != nil {
		log.Printf("record connect: %v", err)
	}
	defer func() {
		reg.Unregister(service, pathPrefix, entry)
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

func handleTunnelRequest(reg *relay.Registry, service string, w http.ResponseWriter, r *http.Request) {
	entry := reg.Resolve(service, r.URL.Path)
	if entry == nil {
		http.Error(w, "no active tunnel for "+service, http.StatusBadGateway)
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
			http.Error(w, "tunnel error: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
