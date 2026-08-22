// Package relay holds the live routing table (service/path -> connected CLI).
// Each tunnel is a multiplexed session (yamux) over the CLI's websocket;
// every public connection gets its own raw byte stream to the local target.
package relay

import (
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

type Entry struct {
	ConnectedAt time.Time
	Remote      string

	session *yamux.Session
}

func newEntry(sess *yamux.Session, remote string) *Entry {
	return &Entry{ConnectedAt: time.Now(), Remote: remote, session: sess}
}

// OpenStream dials a fresh raw TCP stream through the tunnel to the CLI's
// local target. The caller speaks whatever protocol it wants on the bytes.
func (e *Entry) OpenStream() (net.Conn, error) {
	if e.session.IsClosed() {
		return nil, errors.New("tunnel closed")
	}
	return e.session.Open()
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

func (r *Registry) Register(service, pathPrefix string, sess *yamux.Session, remote string) (*Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[service]
	if !ok {
		t = &serviceTable{}
		r.table[service] = t
	}

	entry := newEntry(sess, remote)
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
