// Package relayapi provides small authenticated REST helpers for talking
// to a remote roxey relay using an API key.
package relayapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	u := url.URL{Scheme: "https", Host: RelayHost(tld), Path: "/api/lookup"}
	u.RawQuery = "services=" + url.QueryEscape(strings.Join(services, ","))
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
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
