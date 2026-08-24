// Package config manages the CLI's local state under ~/.roxey:
//
//   - config.json  — registry of known relay servers, keyed by TLD
//     (remote relays carry an API key; local ones also carry the admin
//     credentials the CLI generated when it first spawned that relay)
//   - tunnels.json — tunnels this machine has started
//   - services.json— app processes this machine has spawned (from
//     `roxey up` run-routes), so `down` can stop them again
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ServerConfig is one known relay server, keyed by its TLD. The relay is
// always expected at roxey.<tld> and public URLs are https://<host>.<tld>.
type ServerConfig struct {
	APIKey string `json:"apiKey,omitempty"`
	Local  bool   `json:"local,omitempty"`
	// Account credentials for local relays: the CLI generates a throwaway
	// account when it spawns the relay (SINGLE_USER mode) and logs in with
	// these to create API keys.
	AdminEmail string `json:"adminEmail,omitempty"`
	AdminPass  string `json:"adminPass,omitempty"`
}

type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

type TunnelInfo struct {
	PID          int    `json:"pid"`
	Service      string `json:"service"`
	PathPrefix   string `json:"pathPrefix,omitempty"`
	TLD          string `json:"tld"`
	Target       string `json:"target"`
	ManifestPath string `json:"manifestPath,omitempty"`
	StartedAt    string `json:"startedAt"`
	LogFile      string `json:"logFile"`
}

// ServiceInfo is an app process spawned by `roxey up` for a run-route.
type ServiceInfo struct {
	Name         string `json:"name"`
	PID          int    `json:"pid"`
	Port         int    `json:"port"`
	Command      string `json:"command"`
	Cwd          string `json:"cwd"`
	ManifestPath string `json:"manifestPath"`
	StartedAt    string `json:"startedAt"`
	LogFile      string `json:"logFile"`
}

// dirOverride lets the boot-time service daemon target the real user's
// state directory (daemons run with root's $HOME).
var dirOverride string

// SetDir overrides the state directory (used by `_service-relay`).
func SetDir(d string) { dirOverride = d }

func Dir() string {
	if dirOverride != "" {
		return dirOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roxey")
}

func LogDir() string  { return filepath.Join(Dir(), "logs") }
func BinDir() string  { return filepath.Join(Dir(), "bin") }
func CertDir() string { return filepath.Join(Dir(), "certs") }

func configPath() string   { return filepath.Join(Dir(), "config.json") }
func tunnelsPath() string  { return filepath.Join(Dir(), "tunnels.json") }
func servicesPath() string { return filepath.Join(Dir(), "services.json") }

func ensureDirs() error {
	for _, d := range []string{Dir(), LogDir()} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}

// LoadConfig returns nil, nil if no servers are registered yet.
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveConfig(cfg *Config) error {
	if cfg.Servers == nil {
		cfg.Servers = map[string]ServerConfig{}
	}
	if err := ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

// ServerFor returns the entry for tld, or ok=false if unknown.
func ServerFor(tld string) (ServerConfig, bool) {
	cfg, err := LoadConfig()
	if err != nil || cfg == nil {
		return ServerConfig{}, false
	}
	sc, ok := cfg.Servers[tld]
	return sc, ok
}

func SetServer(tld string, sc ServerConfig) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]ServerConfig{}
	}
	cfg.Servers[tld] = sc
	return SaveConfig(cfg)
}

func LoadTunnels() (map[string]TunnelInfo, error) {
	data, err := os.ReadFile(tunnelsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]TunnelInfo{}, nil
		}
		return nil, err
	}
	var t map[string]TunnelInfo
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return t, nil
}

func SaveTunnels(t map[string]TunnelInfo) error { return saveJSON(tunnelsPath(), t) }

func LoadServices() (map[string]ServiceInfo, error) {
	data, err := os.ReadFile(servicesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ServiceInfo{}, nil
		}
		return nil, err
	}
	var s map[string]ServiceInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s, nil
}

func SaveServices(s map[string]ServiceInfo) error { return saveJSON(servicesPath(), s) }

func saveJSON(path string, v any) error {
	if err := ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
