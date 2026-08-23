// Package tunnel implements the tunnel worker: it holds the multiplexed
// session (yamux over a websocket) to the relay, and for each incoming
// stream dials the local target's TCP port and pipes bytes both ways.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"roxey/internal/config"
)

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); _ = a.Close(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); _ = b.Close(); done <- struct{}{} }()
	<-done
}

func handleStream(stream net.Conn, host, port string) {
	defer stream.Close()

	local, err := net.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Printf("[roxey] local dial %s:%s failed: %v\n", host, port, err)
		return
	}
	defer local.Close()
	pipe(stream, local)
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

// Run connects to the relay for tld (at relay.<tld>) and blocks, bridging
// tunneled streams to the local target, until the connection drops or the
// process is signaled.
func Run(tld, service, pathPrefix, target string) error {
	sc, ok := config.ServerFor(tld)
	if !ok || sc.APIKey == "" {
		return fmt.Errorf("no API key saved for %s, run `roxey auth --tld %s <key>` first", tld, tld)
	}

	host, port := parseTarget(target)

	scheme := "wss"
	if os.Getenv("ROXEY_INSECURE") == "1" { // for testing against a relay without TLS
		scheme = "ws"
	}
	relayHost := os.Getenv("ROXEY_RELAY_HOST") // overrides the relay.<tld> hostname (self-hosted/testing)
	if relayHost == "" {
		relayHost = "relay." + tld
	}
	u := url.URL{Scheme: scheme, Host: relayHost, Path: "/_ws"}
	q := u.Query()
	q.Set("service", service)
	q.Set("path", pathPrefix)
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+sc.APIKey)

	dialer := *websocket.DefaultDialer
	if addr := os.Getenv("ROXEY_RELAY_ADDR"); addr != "" { // force-dial a different address than the URL host
		dialer.NetDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	conn, resp, err := dialer.Dial(u.String(), hdr)
	if err != nil {
		return fmt.Errorf("connect: %w%s", err, handshakeReason(resp))
	}
	defer conn.Close()

	sess, err := yamux.Client(NewWSNetConn(conn), nil)
	if err != nil {
		return fmt.Errorf("mux: %w", err)
	}
	defer sess.Close()

	publicURL := fmt.Sprintf("https://%s.%s%s", service, tld, pathPrefix)
	fmt.Printf("[roxey] connected: %s -> %s\n", publicURL, target)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		conn.Close()
		os.Exit(0)
	}()

	for {
		stream, err := sess.Accept()
		if err != nil {
			fmt.Printf("[roxey] disconnected: %v\n", err)
			return nil
		}
		go handleStream(stream, host, port)
	}
}
