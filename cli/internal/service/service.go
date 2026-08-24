// Package service installs and manages the boot-time relay daemon and
// per-project autostart units: a root LaunchDaemon (macOS) / systemd system
// unit (Linux) keeps the local relay alive on port 443, and optional user
// agents bring registered projects up at login.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	darwinLabel     = "com.four43labs.roxey-relay"
	projectLabelPfx = "com.four43labs.roxey.project."
)

type Manager struct {
	BinPath  string // absolute path to the roxey binary
	StateDir string // the real user's ~/.roxey (daemons run with another $HOME)
}

func NewManager() (*Manager, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Manager{BinPath: bin, StateDir: stateDirForUser()}, nil
}

// stateDirForUser resolves ~/.roxey of the *invoking* user even when running
// under sudo (the daemon itself must target that directory).
func stateDirForUser() string {
	home := os.Getenv("SUDO_USER")
	if home == "" {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, ".roxey")
	}
	out, err := exec.Command("sh", "-c", "eval echo ~"+home).Output()
	if err != nil {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, ".roxey")
	}
	return filepath.Join(strings.TrimSpace(string(out)), ".roxey")
}

// ── relay daemon ─────────────────────────────────────────────────────────

// InstallRelay writes and enables the boot-time relay unit (sudo-elevated).
func (m *Manager) InstallRelay() error {
	switch runtime.GOOS {
	case "darwin":
		return m.darwinInstallRelay()
	case "linux":
		return m.linuxInstallRelay()
	default:
		return fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

func (m *Manager) UninstallRelay() error {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", "system", darwinLabel).Run()
		return sudo("rm", "-f", "/Library/LaunchDaemons/"+darwinLabel+".plist")
	case "linux":
		_ = exec.Command("sudo", "systemctl", "disable", "--now", "roxey-relay").Run()
		return sudo("rm", "-f", "/etc/systemd/system/roxey-relay.service")
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

// RelayRunning reports whether the boot daemon's process is loaded/active.
func RelayRunning() bool {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", "system/"+darwinLabel).CombinedOutput()
		return err == nil && strings.Contains(string(out), "state")
	case "linux":
		err := exec.Command("systemctl", "is-active", "--quiet", "roxey-relay").Run()
		return err == nil
	default:
		return false
	}
}

func (m *Manager) darwinInstallRelay() error {
	logs := filepath.Join(m.StateDir, "logs", "relay-service.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>_service-relay</string>
    <string>--state-dir</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>UserName</key><string>root</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, darwinLabel, m.BinPath, m.StateDir, logs, logs)

	tmp, err := os.CreateTemp("", "roxey-relay-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(plist); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	dst := "/Library/LaunchDaemons/" + darwinLabel + ".plist"
	_ = exec.Command("launchctl", "bootout", "system", darwinLabel).Run()
	if err := sudo("cp", tmp.Name(), dst); err != nil {
		return err
	}
	if err := sudo("plutil", "-lint", dst); err != nil {
		return err
	}
	return sudo("launchctl", "bootstrap", "system", dst)
}

func (m *Manager) linuxInstallRelay() error {
	unit := fmt.Sprintf(`[Unit]
Description=Roxey local relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s _service-relay --state-dir %s
Restart=always
User=root

[Install]
WantedBy=multi-user.target
`, m.BinPath, m.StateDir)

	tmp, err := os.CreateTemp("", "roxey-relay-*.service")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(unit); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	dst := "/etc/systemd/system/roxey-relay.service"
	if err := sudo("cp", tmp.Name(), dst); err != nil {
		return err
	}
	for _, c := range [][]string{
		{"sudo", "systemctl", "daemon-reload"},
		{"sudo", "systemctl", "enable", "--now", "roxey-relay"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = os.Stdin
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	return nil
}

// ── project autostart agents (user level) ────────────────────────────────

// InstallProjectAgent enables login-time `up -d --project name`.
func (m *Manager) InstallProjectAgent(name string) error {
	switch runtime.GOOS {
	case "darwin":
		label := projectLabelPfx + name
		dir := homeDir("Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>up</string>
    <string>-d</string>
    <string>--project</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`, label, m.BinPath, name)
		dst := filepath.Join(dir, label+".plist")
		if err := os.WriteFile(dst, []byte(plist), 0644); err != nil {
			return err
		}
		_ = exec.Command("launchctl", "bootout", "gui/"+fmt.Sprint(os.Getuid()), label).Run()
		return exec.Command("launchctl", "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), dst).Run()
	case "linux":
		dir := homeDir(".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		unitName := "roxey-project-" + name + ".service"
		unit := fmt.Sprintf(`[Unit]
Description=Roxey project %s

[Service]
ExecStart=%s up -d --project %s

[Install]
WantedBy=default.target
`, name, m.BinPath, name)
		dst := filepath.Join(dir, unitName)
		if err := os.WriteFile(dst, []byte(unit), 0644); err != nil {
			return err
		}
		for _, c := range [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", unitName},
		} {
			if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, out)
			}
		}
		return nil
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

func (m *Manager) UninstallProjectAgent(name string) error {
	switch runtime.GOOS {
	case "darwin":
		label := projectLabelPfx + name
		_ = exec.Command("launchctl", "bootout", "gui/"+fmt.Sprint(os.Getuid()), label).Run()
		return os.Remove(homeDir("Library", "LaunchAgents", label+".plist"))
	case "linux":
		unitName := "roxey-project-" + name + ".service"
		_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
		err := os.Remove(homeDir(".config", "systemd", "user", unitName))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		if os.IsNotExist(err) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

// ProjectAgentInstalled reports whether the autostart unit exists on disk.
func ProjectAgentInstalled(name string) bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := os.Stat(homeDir("Library", "LaunchAgents", projectLabelPfx+name+".plist"))
		return err == nil
	case "linux":
		_, err := os.Stat(homeDir(".config", "systemd", "user", "roxey-project-"+name+".service"))
		return err == nil
	default:
		return false
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func sudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func homeDir(elem ...string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(append([]string{home}, elem...)...)
}
