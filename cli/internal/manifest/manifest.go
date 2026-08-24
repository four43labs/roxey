// Package manifest loads and validates roxey.yaml files: a declarative
// description of one project's tunnel topology — which relay to use, the
// environments (subdomains) to expose, and the routes on each (either
// proxying to an existing address or spawning a service command).
package manifest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RelayServer selects the relay this manifest's tunnels attach to.
// The relay is always expected at roxey.<tld>; public URLs are
// https://<host>.<tld>.
type RelayServer struct {
	TLD    string `yaml:"tld"`
	APIKey string `yaml:"api_key"`
	Local  bool   `yaml:"local"`
}

// RunService describes a service roxey spawns itself (portless-style).
type RunService struct {
	Command     string            `yaml:"command"`
	Cwd         string            `yaml:"cwd,omitempty"`
	Port        int               `yaml:"port"`
	Environment map[string]string `yaml:"environment,omitempty"`
}

// Route is one path within an environment. Either Target (proxy to an
// already-running address) or Command (roxey spawns it) is set, never both.
type Route struct {
	Path        string
	Target      string
	Command     string
	Cwd         string
	Port        int
	Environment map[string]string
}

func (r *Route) IsRun() bool { return r.Command != "" }

type Environment struct {
	Host   string  `yaml:"host"`
	Routes []Route `yaml:"routes"`
}

type Manifest struct {
	RelayServer  RelayServer   `yaml:"relay_server"`
	Environments []Environment `yaml:"environments"`

	// Dir is the directory containing the manifest file; relative cwds
	// resolve against it.
	Dir string

	// HostMap maps each environment's declared host to its effective host
	// (identical unless a fork-preview rewrote them). Used to resolve
	// {{host:X}} placeholders in route environment values.
	HostMap map[string]string
}

var hostTemplateRe = regexp.MustCompile(`\{\{\s*host:([a-z0-9.-]+)\s*\}\}`)

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// DefaultLocalTLD is the TLD every project shares when using the local
// relay: one relay at roxey.<DefaultLocalTLD> serves all projects.
const DefaultLocalTLD = "dev"

// Load reads and validates the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m.Dir = filepath.Dir(abs(path))
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.initHostMap()
	return &m, nil
}

func (m *Manifest) initHostMap() {
	if m.HostMap == nil {
		m.HostMap = map[string]string{}
	}
	for _, e := range m.Environments {
		if _, ok := m.HostMap[e.Host]; !ok {
			m.HostMap[e.Host] = e.Host
		}
	}
}

// ResolveEnvTemplates replaces {{host:X}} placeholders in every route's
// environment values with the effective public host of environment X
// (FQDN, without scheme). Unknown hosts resolve to themselves.
func (m *Manifest) ResolveEnvTemplates() {
	sub := func(match []byte) []byte {
		name := string(hostTemplateRe.FindSubmatch(match)[1])
		host := name
		if final, ok := m.HostMap[name]; ok {
			host = final
		}
		return []byte(host + "." + m.RelayServer.TLD)
	}
	for i := range m.Environments {
		env := &m.Environments[i]
		for j := range env.Routes {
			for k, v := range env.Routes[j].Environment {
				if hostTemplateRe.Match([]byte(v)) {
					env.Routes[j].Environment[k] = string(hostTemplateRe.ReplaceAllFunc([]byte(v), sub))
				}
			}
		}
	}
}

func (m *Manifest) Validate() error {
	rs := &m.RelayServer
	if rs.TLD == "" && !rs.Local && rs.APIKey == "" {
		return fmt.Errorf("relay_server is required (e.g. `relay_server:\\n  tld: f43.run` or `local: true`)")
	}
	if rs.TLD == "" && rs.Local {
		rs.TLD = DefaultLocalTLD
	}
	if rs.TLD == "" {
		return fmt.Errorf("relay_server.tld is required")
	}
	if !validTLD(rs.TLD) {
		return fmt.Errorf("relay_server.tld %q is not a valid DNS name", rs.TLD)
	}
	if rs.Local && rs.APIKey != "" {
		return fmt.Errorf("relay_server.api_key cannot be combined with local: true")
	}

	if len(m.Environments) == 0 {
		return fmt.Errorf("at least one environment is required")
	}
	seenHosts := map[string]bool{}
	for i := range m.Environments {
		env := &m.Environments[i]
		label := "environments[" + itoa(i) + "]"
		if env.Host == "" || !validHost(env.Host) {
			return fmt.Errorf("%s.host %q must be a DNS name (labels like %q or dotted like %q)",
				label, env.Host, "app", "app.verifycate")
		}
		if seenHosts[env.Host] {
			return fmt.Errorf("%s.host %q is duplicated", label, env.Host)
		}
		seenHosts[env.Host] = true

		if len(env.Routes) == 0 {
			return fmt.Errorf("%s needs at least one route", label)
		}
		seenPaths := map[string]bool{}
		for j := range env.Routes {
			r := &env.Routes[j]
			rl := label + ".routes[" + itoa(j) + "]"
			if !strings.HasPrefix(r.Path, "/") {
				return fmt.Errorf("%s.path %q must start with /", rl, r.Path)
			}
			if seenPaths[r.Path] {
				return fmt.Errorf("%s.path %q is duplicated", rl, r.Path)
			}
			seenPaths[r.Path] = true

			if r.Command != "" {
				if r.Target != "" {
					return fmt.Errorf("%s sets both an address and command", rl)
				}
				if r.Port < 0 || r.Port > 65535 {
					return fmt.Errorf("%s.command %q has invalid port (%d)", rl, r.Command, r.Port)
				}
				// port 0 = roxey assigns a free one and exports PORT to the child
				if r.Cwd != "" {
					if !filepath.IsAbs(r.Cwd) {
						r.Cwd = filepath.Join(m.Dir, r.Cwd)
					}
					if st, err := os.Stat(r.Cwd); err != nil || !st.IsDir() {
						return fmt.Errorf("%s.cwd %q does not exist", rl, r.Cwd)
					}
				} else {
					r.Cwd = m.Dir
				}
			} else if r.Target == "" {
				return fmt.Errorf("%s must map to either an address or a command", rl)
			}
		}
	}
	return nil
}

func validTLD(tld string) bool {
	labels := strings.Split(strings.TrimSuffix(tld, "."), ".")
	if len(labels) < 1 || tld != strings.ToLower(tld) {
		return false
	}
	for _, l := range labels {
		if !hostRe.MatchString(l) {
			return false
		}
	}
	return net.ParseIP(tld) == nil // bare IPs are not TLDs
}

// validHost accepts single labels ("app") and dotted multi-label names
// ("app.verifycate"); every label must be a valid DNS word.
func validHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	for _, l := range strings.Split(host, ".") {
		if !hostRe.MatchString(l) {
			return false
		}
	}
	return true
}

// PublicURL returns the URL root for an environment host.
func (m *Manifest) PublicURL(host string) string {
	return "https://" + host + "." + m.RelayServer.TLD
}

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
