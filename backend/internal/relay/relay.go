// Package relay holds the live routing table (service/path -> connected CLI)
// and the request/response bridge sent over each tunnel's websocket.
package relay

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Entry struct {
	Conn        *websocket.Conn
	ConnectedAt time.Time
	Remote      string

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]func(Response)
}

func newEntry(conn *websocket.Conn, remote string) *Entry {
	return &Entry{Conn: conn, ConnectedAt: time.Now(), Remote: remote, pending: make(map[string]func(Response))}
}

type prefixEntry struct {
	prefix string
	entry  *Entry
}

type serviceTable struct {
	root     *Entry
	prefixed []prefixEntry
}

type Registry struct {
	mu    sync.Mutex
	table map[string]*serviceTable
}

func NewRegistry() *Registry { return &Registry{table: make(map[string]*serviceTable)} }

func (r *Registry) Register(service, pathPrefix string, conn *websocket.Conn, remote string) (*Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[service]
	if !ok {
		t = &serviceTable{}
		r.table[service] = t
	}

	entry := newEntry(conn, remote)
	if pathPrefix == "" {
		if t.root != nil {
			return nil, errors.New("service already in use")
		}
		t.root = entry
	} else {
		for _, p := range t.prefixed {
			if p.prefix == pathPrefix {
				return nil, errors.New("path already in use")
			}
		}
		t.prefixed = append(t.prefixed, prefixEntry{prefix: pathPrefix, entry: entry})
		sort.Slice(t.prefixed, func(i, j int) bool { return len(t.prefixed[i].prefix) > len(t.prefixed[j].prefix) })
	}
	return entry, nil
}

func (r *Registry) Unregister(service, pathPrefix string, entry *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[service]
	if !ok {
		return
	}
	if pathPrefix == "" {
		if t.root == entry {
			t.root = nil
		}
	} else {
		kept := t.prefixed[:0]
		for _, p := range t.prefixed {
			if p.entry != entry {
				kept = append(kept, p)
			}
		}
		t.prefixed = kept
	}
	if t.root == nil && len(t.prefixed) == 0 {
		delete(r.table, service)
	}
}

// Resolve finds the tunnel for an incoming request path, preferring the longest matching path prefix.
func (r *Registry) Resolve(service, reqPath string) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[service]
	if !ok {
		return nil
	}
	for _, p := range t.prefixed {
		if reqPath == p.prefix || strings.HasPrefix(reqPath, p.prefix+"/") {
			return p.entry
		}
	}
	return t.root
}

type ActiveTunnel struct {
	Service     string    `json:"service"`
	Path        string    `json:"path"`
	ConnectedAt time.Time `json:"connectedAt"`
	Remote      string    `json:"remote"`
}

func (r *Registry) ListActive() []ActiveTunnel {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []ActiveTunnel{}
	for service, t := range r.table {
		if t.root != nil {
			out = append(out, ActiveTunnel{Service: service, Path: "/", ConnectedAt: t.root.ConnectedAt, Remote: t.root.Remote})
		}
		for _, p := range t.prefixed {
			out = append(out, ActiveTunnel{Service: service, Path: p.prefix, ConnectedAt: p.entry.ConnectedAt, Remote: p.entry.Remote})
		}
	}
	return out
}

// Wire protocol exchanged with the CLI over the tunnel websocket.
// Bodies are base64'd JSON frames — fine for typical dev traffic; large
// file/streaming payloads would need a binary framing upgrade.
type Request struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	BodyB64 string              `json:"bodyB64,omitempty"`
}

type Response struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	BodyB64 string              `json:"bodyB64,omitempty"`
}

// SendRequest forwards an HTTP request over the tunnel and blocks for the matching response.
func (e *Entry) SendRequest(id, method, path string, headers map[string][]string, body []byte, timeout time.Duration) (Response, error) {
	ch := make(chan Response, 1)
	e.mu.Lock()
	e.pending[id] = func(resp Response) { ch <- resp }
	e.mu.Unlock()

	req := Request{Type: "request", ID: id, Method: method, Path: path, Headers: headers}
	if len(body) > 0 {
		req.BodyB64 = base64.StdEncoding.EncodeToString(body)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}

	e.writeMu.Lock()
	err = e.Conn.WriteMessage(websocket.TextMessage, data)
	e.writeMu.Unlock()
	if err != nil {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return Response{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return Response{}, errors.New("tunnel timeout")
	}
}

// HandleMessage dispatches a response frame read from the tunnel to its waiting SendRequest call.
func (e *Entry) HandleMessage(raw []byte) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Type != "response" {
		return
	}
	e.mu.Lock()
	cb, ok := e.pending[resp.ID]
	if ok {
		delete(e.pending, resp.ID)
	}
	e.mu.Unlock()
	if ok {
		cb(resp)
	}
}
