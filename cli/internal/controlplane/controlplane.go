// Package controlplane publishes a manifest's topology to an Xhetra control
// plane instead of tunnelling to a Roxey relay.
//
// In this mode Roxey still parses the manifest, assigns ports, resolves
// `{{host:...}}`/`{{port:...}}` templates, and spawns services — but the public
// hosts are assigned by the control plane (via LookupHosts) and the route table
// is handed off (via Publish). The control plane's edge proxy (Caddy) then
// fronts every host behind its own authentication, so previews sit on the
// deployment's domain and are tracked per sandbox/chat.
//
// Configuration (env):
//
//	ROXEY_CONTROL_PLANE          control-plane origin, e.g. https://app.xhetra.com
//	ROXEY_CONTROL_PLANE_SECRET   sandbox secret (x-xhetra-sandbox-secret)
//	ROXEY_CONTROL_PLANE_DOMAIN   preview domain, e.g. xhetra.com
package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is a resolved control-plane endpoint.
type Config struct {
	URL    string
	Secret string
	Domain string
}

// FromEnv returns the control-plane config, or nil when not configured.
func FromEnv() *Config {
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv("ROXEY_CONTROL_PLANE")), "/")
	secret := strings.TrimSpace(os.Getenv("ROXEY_CONTROL_PLANE_SECRET"))
	if raw == "" || secret == "" {
		return nil
	}
	domain := strings.TrimSpace(os.Getenv("ROXEY_CONTROL_PLANE_DOMAIN"))
	if domain == "" {
		domain = defaultDomain(raw)
	}
	return &Config{URL: raw, Secret: secret, Domain: domain}
}

// defaultDomain derives the preview domain from the control-plane origin:
// `https://app.xhetra.com` -> `xhetra.com`. A leading `app.` label is dropped.
func defaultDomain(raw string) string {
	u, err := url.Parse(raw)
	host := raw
	if err == nil && u.Host != "" {
		host = u.Hostname()
	} else {
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		host = strings.SplitN(host, "/", 2)[0]
		host = strings.SplitN(host, ":", 2)[0]
	}
	if rest, ok := strings.CutPrefix(host, "app."); ok {
		return rest
	}
	return host
}

// Route is one path within an environment, dialling a port inside the sandbox.
type Route struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

// Environment is one public host and its path routes.
type Environment struct {
	Host   string  `json:"host"`
	Routes []Route `json:"routes"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (c *Config) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.URL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("x-xhetra-sandbox-secret", c.Secret)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d %s", method, path, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// LookupHosts asks the control plane to assign a public label for each declared
// environment host, keyed by the declared host. Labels are single DNS labels
// (no domain); callers append the domain returned by the manifest's TLD.
func (c *Config) LookupHosts(declared []string) (map[string]string, error) {
	var res struct {
		TLD   string            `json:"tld"`
		Hosts map[string]string `json:"hosts"`
	}
	if err := c.do(http.MethodPost, "/api/sandbox/previews/lookup", map[string]any{"hosts": declared}, &res); err != nil {
		return nil, err
	}
	if len(res.Hosts) == 0 {
		return nil, fmt.Errorf("control plane returned no hosts")
	}
	return res.Hosts, nil
}

// Publish hands the control plane a topology to front. It is idempotent: the
// same name replaces the previous topology's hosts and routes.
func (c *Config) Publish(name string, environments []Environment) error {
	return c.do(http.MethodPost, "/api/sandbox/previews", map[string]any{
		"name":         name,
		"environments": environments,
	}, nil)
}

// Delete removes a published topology by name.
func (c *Config) Delete(name string) error {
	q := url.Values{}
	q.Set("name", name)
	return c.do(http.MethodDelete, "/api/sandbox/previews?"+q.Encode(), nil, nil)
}
