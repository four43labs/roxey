// Package tunnel implements the tunnel worker: it holds the websocket
// connection to the relay, and for each incoming request frame makes the
// matching local HTTP call and ships the response back.
package tunnel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"

	"roxey/internal/config"
)

// Wire protocol mirrors the relay's — duplicated rather than shared since
// the CLI and relay are separate modules/deployables.
type wireRequest struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	BodyB64 string              `json:"bodyB64,omitempty"`
}

type wireResponse struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	BodyB64 string              `json:"bodyB64,omitempty"`
}

type worker struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	client    *http.Client
	localBase string
}

func (w *worker) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_ = w.conn.WriteMessage(websocket.TextMessage, data)
}

func (w *worker) sendError(id string, err error) {
	w.send(wireResponse{
		Type: "response", ID: id, Status: http.StatusBadGateway,
		Headers: map[string][]string{},
		BodyB64: base64.StdEncoding.EncodeToString([]byte("local target error: " + err.Error())),
	})
}

func (w *worker) handleRequest(req wireRequest) {
	var body io.Reader
	if req.BodyB64 != "" {
		data, err := base64.StdEncoding.DecodeString(req.BodyB64)
		if err != nil {
			w.sendError(req.ID, err)
			return
		}
		body = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequest(req.Method, w.localBase+req.Path, body)
	if err != nil {
		w.sendError(req.ID, err)
		return
	}
	for k, vals := range req.Headers {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := w.client.Do(httpReq)
	if err != nil {
		w.sendError(req.ID, err)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		w.sendError(req.ID, err)
		return
	}

	out := wireResponse{Type: "response", ID: req.ID, Status: resp.StatusCode, Headers: map[string][]string(resp.Header)}
	if len(respBody) > 0 {
		out.BodyB64 = base64.StdEncoding.EncodeToString(respBody)
	}
	w.send(out)
}

func parseTarget(target string) (host, port string) {
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimPrefix(target, "https://")
	if strings.Contains(target, ":") {
		parts := strings.SplitN(target, ":", 2)
		return parts[0], parts[1]
	}
	return "localhost", target
}

// Run connects to the relay and blocks, bridging tunneled requests to the
// local target, until the connection drops or the process is signaled.
func Run(service, pathPrefix, target string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("not authenticated, run `roxey auth` first")
	}

	host, port := parseTarget(target)
	localBase := fmt.Sprintf("http://%s:%s", host, port)

	scheme := "wss"
	if os.Getenv("ROXEY_INSECURE") == "1" { // for testing against a relay without TLS
		scheme = "ws"
	}
	u := url.URL{Scheme: scheme, Host: cfg.RelayHost, Path: "/_ws"}
	q := u.Query()
	q.Set("service", service)
	q.Set("path", pathPrefix)
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+cfg.APIKey)

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), hdr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	publicURL := fmt.Sprintf("https://%s.%s%s", service, cfg.Domain, pathPrefix)
	fmt.Printf("[roxey] connected: %s -> %s\n", publicURL, target)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		conn.Close()
		os.Exit(0)
	}()

	w := &worker{conn: conn, client: &http.Client{}, localBase: localBase}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("[roxey] disconnected: %v\n", err)
			return nil
		}
		var req wireRequest
		if err := json.Unmarshal(raw, &req); err != nil || req.Type != "request" {
			continue
		}
		go w.handleRequest(req)
	}
}
