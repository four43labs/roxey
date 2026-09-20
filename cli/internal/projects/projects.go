// Package projects manages ~/.roxey/projects.json: the central registry of
// every project roxey has brought up on this machine. It stores identity
// only — name, manifest location, claimed hostnames — never runtime state;
// status is always derived live from tunnels/services state and processes.
package projects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"roxey/internal/config"
	"roxey/internal/localrelay"
	"roxey/internal/manifest"
)

type Project struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`  // directory containing roxey.yaml
	TLD     string   `json:"tld"`   // relay TLD this project runs under
	Local   bool     `json:"local"` // local relay mode?
	Hosts   []string `json:"hosts"` // environment hosts (without TLD)
	Preview bool     `json:"preview,omitempty"`
	Branch  string   `json:"branch,omitempty"`
	// AutoStart brings the project up at login/boot (service install).
	AutoStart bool   `json:"autoStart,omitempty"`
	AddedAt   string `json:"addedAt"`
}

type Registry struct {
	Projects map[string]*Project `json:"projects"`
}

func path() string { return filepath.Join(config.Dir(), "projects.json") }

// Load returns an empty registry if none exists yet.
func Load() (*Registry, error) {
	data, err := os.ReadFile(path())
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{Projects: map[string]*Project{}}, nil
		}
		return nil, err
	}
	var rg Registry
	if err := json.Unmarshal(data, &rg); err != nil {
		return nil, err
	}
	if rg.Projects == nil {
		rg.Projects = map[string]*Project{}
	}
	return &rg, nil
}

func (rg *Registry) Save() error {
	if rg.Projects == nil {
		rg.Projects = map[string]*Project{}
	}
	if err := os.MkdirAll(config.Dir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(), data, 0600)
}

func (rg *Registry) Get(name string) (*Project, bool) {
	p, ok := rg.Projects[name]
	return p, ok
}

// Register adds or updates a project. The name is derived from the manifest
// directory unless taken by a different path, in which case a -2/-3… suffix
// is appended. Returns the final name.
func (rg *Registry) Register(dirPath, tld string, local bool, hosts []string, branch string, preview bool) (string, error) {
	dirPath = filepath.Clean(dirPath)
	base := filepath.Base(dirPath)

	name := base
	for i := 2; ; i++ {
		if existing, ok := rg.Projects[name]; !ok || SameDir(existing.Path, dirPath) {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}

	rg.Projects[name] = &Project{
		Name:    name,
		Path:    dirPath,
		TLD:     tld,
		Local:   local,
		Hosts:   hosts,
		Branch:  branch,
		Preview: preview,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := rg.Save(); err != nil {
		return "", err
	}
	return name, nil
}

func (rg *Registry) Forget(name string) bool {
	if _, ok := rg.Projects[name]; !ok {
		return false
	}
	delete(rg.Projects, name)
	_ = rg.Save()
	return true
}

// Prune drops entries whose manifest directory no longer exists and returns
// their names.
func (rg *Registry) Prune() []string {
	var gone []string
	for name, p := range rg.Projects {
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			gone = append(gone, name)
			delete(rg.Projects, name)
		}
	}
	if len(gone) > 0 {
		_ = rg.Save()
	}
	sort.Strings(gone)
	return gone
}

// SortedNames lists project names alphabetically.
func (rg *Registry) SortedNames() []string {
	names := make([]string, 0, len(rg.Projects))
	for n := range rg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// AllLocalHostFQDNs returns every hostname (host + "." + tld) claimed by any
// project running against a local relay, plus roxey.<tld> for each distinct
// local TLD. Used for cumulative cert SANs and /etc/hosts entries.
func (rg *Registry) AllLocalHostFQDNs() []string {
	set := map[string]bool{}
	for _, p := range rg.Projects {
		if !p.Local {
			continue
		}
		set[localrelay.AdminSubdomain+"."+p.TLD] = true
		for _, h := range p.Hosts {
			set[h+"."+p.TLD] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PrimaryLocalTLD returns the TLD the shared relay serves: the default when
// any local project uses it, else the first registered local TLD. Empty when
// no local project is registered.
func (rg *Registry) PrimaryLocalTLD() string {
	first := ""
	for _, n := range rg.SortedNames() {
		p := rg.Projects[n]
		if !p.Local {
			continue
		}
		if p.TLD == manifest.DefaultLocalTLD {
			return p.TLD
		}
		if first == "" {
			first = p.TLD
		}
	}
	if first != "" {
		return first
	}
	return manifest.DefaultLocalTLD
}

// FindHostCollisions reports hosts claimed by more than one live project.
func (rg *Registry) FindHostCollisions() map[string][]string {
	seen := map[string][]string{}
	for _, p := range rg.Projects {
		for _, h := range p.Hosts {
			key := h + "." + p.TLD
			seen[key] = append(seen[key], p.Name)
		}
	}
	out := map[string][]string{}
	for fqdn, owners := range seen {
		if len(owners) > 1 {
			out[fqdn] = owners
		}
	}
	return out
}

// SameDir compares two directory paths after cleaning.
func SameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
