// Package relayapi provides small authenticated REST helpers for talking
// to a remote roxey relay using an API key.
package relayapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// RelayHost is the admin hostname for a TLD: roxey.<tld>.
func RelayHost(tld string) string { return "roxey." + tld }

// LookupHosts maps service names to their public tunnel hosts on hosted
// relays, where hosts are namespaced per account+service and cannot be
// derived client-side.
func LookupHosts(tld, apiKey string, services []string) (map[string]string, error) {
	if len(services) == 0 || apiKey == "" {
		return map[string]string{}, nil
	}
	// Honor the same overrides as the tunnel worker, so `up` works against a
	// self-hosted or test relay: ROXEY_INSECURE=1 (plain HTTP),
	// ROXEY_RELAY_HOST (admin hostname), ROXEY_RELAY_ADDR (dial address).
	scheme := "https"
	if os.Getenv("ROXEY_INSECURE") == "1" {
		scheme = "http"
	}
	host := RelayHost(tld)
	if h := os.Getenv("ROXEY_RELAY_HOST"); h != "" {
		host = h
	}
	u := url.URL{Scheme: scheme, Host: host, Path: "/api/lookup"}
	u.RawQuery = "services=" + url.QueryEscape(strings.Join(services, ","))
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	if addr := os.Getenv("ROXEY_RELAY_ADDR"); addr != "" {
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lookup hosts: %s", res.Status)
	}
	out := map[string]string{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
