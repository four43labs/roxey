// Package relay holds the live routing table (public host/path -> connected
// CLI). Each tunnel is a multiplexed session (yamux) over the CLI's
// websocket; every public connection gets its own raw byte stream to the
// local target.
package relay

import (
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

type Entry struct {
	Service     string // requested service name
	Host        string // full public host this tunnel answers on
	UserID      string // owning account
	GateGroup   string // shared hosted-preview gate; "" = host-scoped
	ProtectHash string // sha256 of the shared secret when gated; "" = public
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

var serviceRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidateService enforces the hostname-safe charset for service names.
func ValidateService(service string) error {
	if !serviceRe.MatchString(service) {
		return errors.New("service must be 1-40 chars of lowercase letters, digits and dashes")
	}
	return nil
}

type prefixEntry struct {
	prefix string
	entry  *Entry
}

type hostTable struct {
	root     *Entry
	prefixed []prefixEntry
}

type Registry struct {
	mu    sync.Mutex
	table map[string]*hostTable
}

func NewRegistry() *Registry { return &Registry{table: make(map[string]*hostTable)} }

// Check reports whether a registration for host/pathPrefix would be
// accepted, so callers can reject duplicates with a proper HTTP error
// before upgrading the websocket.
func (r *Registry) Check(host, pathPrefix string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[host]
	if !ok {
		return nil
	}
	if pathPrefix == "" {
		if t.root != nil {
			return errors.New("service already in use")
		}
		return nil
	}
	for _, p := range t.prefixed {
		if p.prefix == pathPrefix {
			return errors.New("path already in use")
		}
	}
	return nil
}

// Register installs a live tunnel entry under its public host.
func (r *Registry) Register(entry *Entry, pathPrefix string, sess *yamux.Session, remote string) (*Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[entry.Host]
	if !ok {
		t = &hostTable{}
		r.table[entry.Host] = t
	}

	e := newEntry(sess, remote)
	e.Service = entry.Service
	e.Host = entry.Host
	e.UserID = entry.UserID
	e.GateGroup = entry.GateGroup
	e.ProtectHash = entry.ProtectHash

	if pathPrefix == "" {
		if t.root != nil {
			return nil, errors.New("service already in use")
		}
		t.root = e
	} else {
		for _, p := range t.prefixed {
			if p.prefix == pathPrefix {
				return nil, errors.New("path already in use")
			}
		}
		t.prefixed = append(t.prefixed, prefixEntry{prefix: pathPrefix, entry: e})
		sort.Slice(t.prefixed, func(i, j int) bool { return len(t.prefixed[i].prefix) > len(t.prefixed[j].prefix) })
	}
	return e, nil
}

func (r *Registry) Unregister(host, pathPrefix string, entry *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[host]
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
		delete(r.table, host)
	}
}

// Resolve finds the tunnel for an incoming request path, preferring the longest matching path prefix.
func (r *Registry) Resolve(host, reqPath string) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.table[host]
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
	Host        string    `json:"host"`
	Path        string    `json:"path"`
	Protected   bool      `json:"protected"`
	ConnectedAt time.Time `json:"connectedAt"`
	Remote      string    `json:"remote"`
}

// ListActive returns tunnels owned by userID.
func (r *Registry) ListActive(userID string) []ActiveTunnel {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []ActiveTunnel{}
	for _, t := range r.table {
		if t.root != nil && t.root.UserID == userID {
			out = append(out, ActiveTunnel{Service: t.root.Service, Host: t.root.Host, Path: "/",
				Protected: t.root.ProtectHash != "", ConnectedAt: t.root.ConnectedAt, Remote: t.root.Remote})
		}
		for _, p := range t.prefixed {
			if p.entry.UserID == userID {
				out = append(out, ActiveTunnel{Service: p.entry.Service, Host: p.entry.Host, Path: p.prefix,
					Protected: p.entry.ProtectHash != "", ConnectedAt: p.entry.ConnectedAt, Remote: p.entry.Remote})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host+out[i].Path < out[j].Host+out[j].Path })
	return out
}
