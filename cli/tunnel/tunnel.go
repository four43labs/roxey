// Package tunnel is the local side of a Roxey tunnel: it holds a multiplexed
// session (yamux over a websocket) to a relay, and for each incoming stream
// dials the local target's TCP port and pipes bytes both ways.
//
// The wire protocol: dial wss://<relay>/_ws?service=<name>&path=<prefix>
// [&protect=<secret>][&gate_group=<id>] with "Authorization: Bearer <key>",
// read a "host:<label>" text message (the public host the relay assigned),
// then serve yamux streams over binary websocket frames.
//
// It is public so other tools can open Roxey tunnels without the roxey CLI
// (Teyliv's CLI does); roxey's own background worker is internal/worker.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Options describes one tunnel route.
type Options struct {
	// RelayURL is the relay's websocket endpoint, e.g. wss://roxey.f43.run/_ws.
	// Query parameters already on it are kept.
	RelayURL string
	APIKey   string
	Service  string
	// Path is the path prefix this route answers under; "" is the host root.
	Path string
	// Protect gates the route behind a shared secret; GateGroup lets several
	// routes share one unlock (see `roxey up --protect`).
	Protect   string
	GateGroup string
	// Target is the local address streams are piped to: "localhost:3000",
	// "http://127.0.0.1:8080", or a bare port "3000".
	Target string
	// Dial, when set, dials the relay instead of resolving the relay URL's host
	// (ROXEY_RELAY_ADDR, tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Tunnel is one live connection to the relay.
type Tunnel struct {
	// Host is the public host label the relay assigned; the public URL is
	// https://<Host>.<tld><Path>.
	Host string

	conn *websocket.Conn
	sess *yamux.Session
	once sync.Once
	done chan struct{}
}

// Done is closed when the tunnel is gone (closed locally or dropped).
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Close ends the tunnel.
func (t *Tunnel) Close() error {
	t.once.Do(func() {
		_ = t.sess.Close()
		_ = t.conn.Close()
	})
	return nil
}

// WebsocketURL builds the relay endpoint for a route.
func WebsocketURL(scheme, relayHost, service, pathPrefix, protect, gateGroup string) url.URL {
	u := url.URL{Scheme: scheme, Host: relayHost, Path: "/_ws"}
	q := u.Query()
	setRouteQuery(q, service, pathPrefix, protect, gateGroup)
	u.RawQuery = q.Encode()
	return u
}

func setRouteQuery(q url.Values, service, pathPrefix, protect, gateGroup string) {
	q.Set("service", service)
	q.Set("path", pathPrefix)
	if protect != "" {
		q.Set("protect", protect)
	}
	if gateGroup != "" {
		q.Set("gate_group", gateGroup)
	}
}

// muxConfig is yamux's default without its stderr logger: drops are reported
// through Done and Hold's log function instead.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	return c
}

// Dial connects one route to the relay and serves it in the background until
// the connection drops or Close is called. It returns once the relay has
// assigned the public host.
func Dial(ctx context.Context, opt Options) (*Tunnel, error) {
	u, err := url.Parse(opt.RelayURL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, fmt.Errorf("invalid relay URL %q", opt.RelayURL)
	}
	q := u.Query()
	setRouteQuery(q, opt.Service, opt.Path, opt.Protect, opt.GateGroup)
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+opt.APIKey)
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 30 * time.Second
	if opt.Dial != nil {
		dialer.NetDialContext = opt.Dial
	}
	conn, resp, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		return nil, fmt.Errorf("connect: %w%s", err, handshakeReason(resp))
	}

	// The relay's first message assigns our public host (it may differ from
	// <service>.<tld> on hosted relays, which namespace per account+service).
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, greeting, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read assigned host: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	host, ok := strings.CutPrefix(string(greeting), "host:")
	if !ok || host == "" {
		conn.Close()
		return nil, fmt.Errorf("unexpected relay greeting %q", greeting)
	}

	sess, err := yamux.Client(NewWSNetConn(conn), muxConfig())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mux: %w", err)
	}
	t := &Tunnel{Host: host, conn: conn, sess: sess, done: make(chan struct{})}
	target := ParseTarget(opt.Target)
	go func() {
		defer close(t.done)
		defer t.Close()
		for {
			stream, err := sess.Accept()
			if err != nil {
				return
			}
			go handleStream(stream, target)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			t.Close()
		case <-t.done:
		}
	}()
	return t, nil
}

// Hold keeps a route connected until ctx ends, reconnecting with backoff when
// the relay drops it (a relay restart, a network blip). The first connection
// is synchronous so its error is the caller's. done closes once ctx ends and
// the tunnel is down. Hosts are stable per account and service, so a
// reconnect comes back on the same URL.
func Hold(ctx context.Context, opt Options, logf func(format string, args ...any)) (host string, done <-chan struct{}, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t, err := Dial(ctx, opt)
	if err != nil {
		return "", nil, err
	}
	host = t.Host // read before the loop below may replace t
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		backoff := time.Second
		for {
			select {
			case <-ctx.Done():
				t.Close()
				return
			case <-t.Done():
			}
			if ctx.Err() != nil {
				return
			}
			logf("tunnel %s%s dropped; reconnecting", t.Host, opt.Path)
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				next, err := Dial(ctx, opt)
				if err == nil {
					t, backoff = next, time.Second
					logf("tunnel %s%s reconnected", t.Host, opt.Path)
					break
				}
				logf("reconnect %s%s: %v", opt.Service, opt.Path, err)
				backoff = min(backoff*2, 30*time.Second)
			}
		}
	}()
	return host, finished, nil
}

// ParseTarget turns "http://localhost:3000/x", "localhost:3000", or "3000"
// into a dialable host:port.
func ParseTarget(target string) string {
	target = strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	target, _, _ = strings.Cut(target, "/")
	if !strings.Contains(target, ":") {
		return net.JoinHostPort("localhost", target)
	}
	return target
}

func handleStream(stream net.Conn, target string) {
	defer stream.Close()
	local, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		fmt.Printf("[roxey] local dial %s failed: %v\n", target, err)
		return
	}
	defer local.Close()
	pipe(stream, local)
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); _ = a.Close(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); _ = b.Close(); done <- struct{}{} }()
	<-done
}

// handshakeReason extracts the relay's rejection message (e.g. "service
// already in use", sent as an HTTP error body or a close frame) so failures
// aren't just a generic "bad handshake".
func handshakeReason(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil || len(body) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s)", strings.TrimSpace(string(body)))
}
