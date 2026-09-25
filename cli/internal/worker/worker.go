// Package worker is roxey's background tunnel process (`roxey _run`): it
// resolves the relay and API key from roxey's saved state and holds one
// route open with the public tunnel package until it is signaled.
package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/four43labs/roxey/cli/internal/config"
	"github.com/four43labs/roxey/cli/internal/localrelay"
	"github.com/four43labs/roxey/cli/tunnel"
)

// Run connects to the relay for tld (at roxey.<tld>) and blocks, bridging
// tunneled streams to the local target and reconnecting after drops, until
// the process is signaled. gateGroup lets manifest tunnels share one unlock
// gate.
func Run(tld, service, pathPrefix, target, protect, gateGroup string) error {
	sc, ok := config.ServerFor(tld)
	if !ok || sc.APIKey == "" {
		return fmt.Errorf("no API key saved for %s, run `roxey auth --tld %s <key>` first", tld, tld)
	}

	scheme := "wss"
	if os.Getenv("ROXEY_INSECURE") == "1" { // for testing against a relay without TLS
		scheme = "ws"
	}
	relayHost := os.Getenv("ROXEY_RELAY_HOST") // overrides the roxey.<tld> hostname (self-hosted/testing)
	if relayHost == "" {
		relayHost = localrelay.AdminSubdomain + "." + tld
	}
	u := tunnel.WebsocketURL(scheme, relayHost, service, pathPrefix, protect, gateGroup)
	opt := tunnel.Options{
		RelayURL: u.String(), APIKey: sc.APIKey, Service: service, Path: pathPrefix,
		Protect: protect, GateGroup: gateGroup, Target: target,
	}
	if addr := os.Getenv("ROXEY_RELAY_ADDR"); addr != "" { // force-dial a different address than the URL host
		opt.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logf := func(format string, a ...any) { fmt.Printf("[roxey] "+format+"\n", a...) }
	host, done, err := tunnel.Hold(ctx, opt, logf)
	if err != nil {
		return err
	}
	suffix := ""
	if protect != "" {
		suffix = "  (protected)"
	}
	fmt.Printf("[roxey] connected: https://%s.%s%s -> %s%s\n", host, tld, pathPrefix, target, suffix)
	<-done
	return nil
}
