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
	"strconv"
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
	Path string
	// Name is an optional handle for a spawned route. Any route can address
	// it as {{port:<name>}} in its environment values or target address, so a
	// service whose port roxey assigns can be referenced without hardcoding a
	// number. It must be a unique DNS label and must not shadow a host name.
	Name        string
	Target      string
	Command     string
	Cwd         string
	Port        int
	Environment map[string]string
}

func (r *Route) IsRun() bool { return r.Command != "" }

type Environment struct {
	Host string `yaml:"host"`
	// Protect, when set, requires visitors to enter this shared secret
	// before any traffic reaches the environment's routes.
	Protect string  `yaml:"protect"`
	Routes  []Route `yaml:"routes"`
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

	// AssignedHostMap contains hosted-relay labels returned by /api/lookup,
	// keyed by the original declared host. Local manifests leave it empty.
	AssignedHostMap map[string]string
}

var (
	hostTemplateRe = regexp.MustCompile(`\{\{\s*host:([a-z0-9.-]+)\s*\}\}`)
	portTemplateRe = regexp.MustCompile(`\{\{\s*port:([a-z0-9.-]+)\s*\}\}`)
)

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// DefaultLocalTLD is the TLD every project shares when using the local
// relay: one relay at roxey.<DefaultLocalTLD> serves all projects.
const DefaultLocalTLD = "dev"

// Load reads and validates the manifest at path.
func Load(path string) (*Manifest, error) { return LoadWith(path, LoadOptions{}) }

// LoadOptions tunes Load for tools that run roxey.yaml topologies themselves.
type LoadOptions struct {
	// RelayOptional accepts a manifest without a relay_server block: the
	// caller brings its own relay and sets RelayServer.TLD before resolving
	// templates. A relay_server block that is present is still validated.
	RelayOptional bool
}

// LoadWith reads and validates the manifest at path with opts.
func LoadWith(path string, opts LoadOptions) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m.Dir = filepath.Dir(abs(path))
	rs := m.RelayServer
	if opts.RelayOptional && rs.TLD == "" && !rs.Local && rs.APIKey == "" {
		err = m.validateEnvironments()
	} else {
		err = m.Validate()
	}
	if err != nil {
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

// ResolveTemplates replaces {{host:…}}, {{port:…}} and {{online}} placeholders
// across the manifest: every route's environment values and, for proxy routes,
// the target address. Call it after ports are assigned (preview.AssignPorts),
// so {{port:…}} can resolve to a spawned service's assigned port.
func (m *Manifest) ResolveTemplates(online bool) error {
	if err := m.ResolveEnvTemplates(online); err != nil {
		return err
	}
	return m.ResolveRouteTargets()
}

// ResolveRouteTargets replaces {{port:…}} placeholders in proxy-route targets,
// letting a route point at a spawned service whose port roxey assigned. Run
// routes are skipped: their target is the localhost:<port> roxey dials.
func (m *Manifest) ResolveRouteTargets() error {
	ix := m.portIndex()
	for i := range m.Environments {
		env := &m.Environments[i]
		for j := range env.Routes {
			r := &env.Routes[j]
			if r.IsRun() {
				continue
			}
			resolved, err := resolvePortTemplates(r.Target, ix)
			if err != nil {
				return fmt.Errorf("resolve target for %s%s: %w", env.Host, r.Path, err)
			}
			r.Target = resolved
		}
	}
	return nil
}

// ResolveEnvTemplates replaces public host, spawned root-route port, and
// online-mode placeholders in route environment values.
func (m *Manifest) ResolveEnvTemplates(online bool) error {
	ix := m.portIndex()
	onlineValue := ""
	if online {
		onlineValue = "1"
	}
	for i := range m.Environments {
		env := &m.Environments[i]
		for j := range env.Routes {
			for k, v := range env.Routes[j].Environment {
				resolved := hostTemplateRe.ReplaceAllStringFunc(v, func(match string) string {
					name := string(hostTemplateRe.FindStringSubmatch(match)[1])
					return ix.host(name) + "." + m.RelayServer.TLD
				})
				resolved, err := resolvePortTemplates(resolved, ix)
				if err != nil {
					return fmt.Errorf("resolve environment %s for %s%s: %w", k, env.Host, env.Routes[j].Path, err)
				}
				resolved = strings.ReplaceAll(resolved, "{{online}}", onlineValue)
				env.Routes[j].Environment[k] = resolved
			}
		}
	}
	return nil
}

// portIndex resolves {{port:…}} names: a spawned route's Name first, then the
// historical host form (a host's "/" run route).
type portIndex struct {
	named    map[string]int
	manifest *Manifest
}

func (m *Manifest) portIndex() *portIndex {
	named := map[string]int{}
	for i := range m.Environments {
		for j := range m.Environments[i].Routes {
			r := &m.Environments[i].Routes[j]
			if r.IsRun() && r.Name != "" {
				named[r.Name] = r.Port
			}
		}
	}
	return &portIndex{named: named, manifest: m}
}

// host returns a placeholder host's effective name (a preview slug or hosted
// label may have rewritten it), defaulting to the declared name.
func (ix *portIndex) host(name string) string {
	if assigned, ok := ix.manifest.AssignedHostMap[name]; ok {
		return assigned
	}
	if final, ok := ix.manifest.HostMap[name]; ok {
		return final
	}
	return name
}

func (ix *portIndex) port(name string) (int, error) {
	if port, ok := ix.named[name]; ok {
		return assignedPort("service", name, port)
	}
	effective, ok := ix.manifest.HostMap[name]
	if !ok {
		return 0, fmt.Errorf("unknown host or service %q in port template", name)
	}
	count, port := 0, 0
	for i := range ix.manifest.Environments {
		if ix.manifest.Environments[i].Host != effective {
			continue
		}
		for j := range ix.manifest.Environments[i].Routes {
			r := &ix.manifest.Environments[i].Routes[j]
			if r.Path == "/" && r.IsRun() {
				count++
				port = r.Port
			}
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("host %q has no spawned root route for port template", name)
	}
	if count > 1 {
		return 0, fmt.Errorf("host %q has multiple spawned root routes for port template", name)
	}
	return assignedPort("host", name, port)
}

func assignedPort(kind, name string, port int) (int, error) {
	if port <= 0 {
		return 0, fmt.Errorf("%s %q has no assigned port for port template", kind, name)
	}
	return port, nil
}

// resolvePortTemplates substitutes every {{port:…}} in s, returning the first
// resolution error.
func resolvePortTemplates(s string, ix *portIndex) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var resolveErr error
	out := portTemplateRe.ReplaceAllStringFunc(s, func(match string) string {
		if resolveErr != nil {
			return match
		}
		name := string(portTemplateRe.FindStringSubmatch(match)[1])
		port, err := ix.port(name)
		if err != nil {
			resolveErr = err
			return match
		}
		return strconv.Itoa(port)
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return out, nil
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
	return m.validateEnvironments()
}

// validateEnvironments checks the hosts and routes (everything but the relay).
func (m *Manifest) validateEnvironments() error {
	if len(m.Environments) == 0 {
		return fmt.Errorf("at least one environment is required")
	}
	hosts := make(map[string]bool, len(m.Environments))
	for i := range m.Environments {
		env := &m.Environments[i]
		label := "environments[" + itoa(i) + "]"
		if env.Host == "" || !validHost(env.Host) {
			return fmt.Errorf("%s.host %q must be a DNS name (labels like %q or dotted like %q)",
				label, env.Host, "app", "app.verifycate")
		}
		if hosts[env.Host] {
			return fmt.Errorf("%s.host %q is duplicated", label, env.Host)
		}
		hosts[env.Host] = true
	}

	names := map[string]string{} // spawned-route name -> definition site
	for i := range m.Environments {
		env := &m.Environments[i]
		label := "environments[" + itoa(i) + "]"
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

			switch {
			case r.Command != "":
				if r.Target != "" {
					return fmt.Errorf("%s sets both an address and command", rl)
				}
				if r.Port < 0 || r.Port > 65535 {
					return fmt.Errorf("%s.command %q has invalid port (%d)", rl, r.Command, r.Port)
				}
				if err := checkRouteName(rl, r.Name, hosts, names); err != nil {
					return err
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
			case r.Name != "":
				return fmt.Errorf("%s.name %q requires a command (only spawned routes take a name)", rl, r.Name)
			case r.Target == "":
				return fmt.Errorf("%s must map to either an address or a command", rl)
			}
		}
	}
	return nil
}

// checkRouteName validates an optional spawned-route handle: a DNS label that
// is unique and never shadows an environment host (so {{port:name}} stays
// unambiguous).
func checkRouteName(loc, name string, hosts map[string]bool, seen map[string]string) error {
	if name == "" {
		return nil
	}
	if !hostRe.MatchString(name) {
		return fmt.Errorf("%s.name %q must be a DNS label (letters, digits, dashes)", loc, name)
	}
	if hosts[name] {
		return fmt.Errorf("%s.name %q collides with an environment host", loc, name)
	}
	if prev, ok := seen[name]; ok {
		return fmt.Errorf("%s.name %q is already used by %s", loc, name, prev)
	}
	seen[name] = loc
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
