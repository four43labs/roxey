// Package config manages the CLI's local state under ~/.roxey: the saved
// API key/relay host from `roxey auth`, and the registry of tunnels this
// machine has started (used by `start`/`stop`/`list`).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	APIKey    string `json:"apiKey"`
	RelayHost string `json:"relayHost"`
	Domain    string `json:"domain"`
}

type TunnelInfo struct {
	PID        int    `json:"pid"`
	Service    string `json:"service"`
	PathPrefix string `json:"pathPrefix"`
	Target     string `json:"target"`
	StartedAt  string `json:"startedAt"`
	LogFile    string `json:"logFile"`
}

func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roxey")
}

func LogDir() string { return filepath.Join(Dir(), "logs") }

func configPath() string  { return filepath.Join(Dir(), "config.json") }
func tunnelsPath() string { return filepath.Join(Dir(), "tunnels.json") }

func ensureDirs() error {
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		return err
	}
	return os.MkdirAll(LogDir(), 0700)
}

// Load returns nil, nil if no config has been saved yet (not authenticated).
func Load() (*Config, error) {
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

func Save(cfg *Config) error {
	if err := ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
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

func SaveTunnels(t map[string]TunnelInfo) error {
	if err := ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tunnelsPath(), data, 0600)
}
