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

	"github.com/four43labs/roxey/cli/internal/localrelay"
)

const (
	darwinDaemonLabel = "com.four43labs.roxey-daemon"
	linuxDaemonUnit   = "roxey-daemon.service"

	// Legacy relay-only unit, replaced by the combined privileged daemon.
	legacyDarwinLabel = "com.four43labs.roxey-relay"
	legacyLinuxUnit   = "roxey-relay.service"

	projectLabelPfx = "com.four43labs.roxey.project."
)

// RootDir is where root-owned copies of roxey's binaries live, so a
// user-writable Homebrew/path binary can never be executed as root.
func RootDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/roxey"
	case "linux":
		return "/usr/local/libexec/roxey"
	default:
		return ""
	}
}

// RootBinary returns the root-owned path of a named binary.
func RootBinary(name string) string { return filepath.Join(RootDir(), name) }

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

// ── privileged helper daemon ─────────────────────────────────────────────

// InstallDaemon installs the boot-time privileged helper (sudo-elevated):
// it owns /etc/hosts, CA trust, and the port-443 relay, and serves the
// unprivileged CLI over a Unix socket. relayBin, when set, is copied to a
// root-owned path the daemon executes.
func (m *Manager) InstallDaemon(relayBin string) error {
	if err := m.installRootBinaries(relayBin); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return m.darwinInstallDaemon()
	case "linux":
		return m.linuxInstallDaemon()
	default:
		return fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

// installRootBinaries copies the CLI and relay into a root-owned directory so
// the daemon never execs a user-writable binary as root.
func (m *Manager) installRootBinaries(relayBin string) error {
	if err := sudo("mkdir", "-p", RootDir()); err != nil {
		return err
	}
	if err := sudo("cp", m.BinPath, RootBinary("roxey")); err != nil {
		return err
	}
	if err := sudo("chmod", "0755", RootBinary("roxey")); err != nil {
		return err
	}
	if relayBin == "" {
		p, err := localrelay.EnsureBinary()
		if err != nil {
			return fmt.Errorf("relay binary: %w", err)
		}
		relayBin = p
	}
	if err := sudo("cp", relayBin, RootBinary("roxey-relay")); err != nil {
		return err
	}
	return sudo("chmod", "0755", RootBinary("roxey-relay"))
}

func (m *Manager) UninstallDaemon() error {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", "system", darwinDaemonLabel).Run()
		_ = exec.Command("launchctl", "bootout", "system", legacyDarwinLabel).Run()
		_ = sudo("rm", "-f", "/Library/LaunchDaemons/"+darwinDaemonLabel+".plist")
		_ = sudo("rm", "-f", "/Library/LaunchDaemons/"+legacyDarwinLabel+".plist")
		_ = sudo("rm", "-rf", RootDir())
		return nil
	case "linux":
		_ = exec.Command("sudo", "systemctl", "disable", "--now", linuxDaemonUnit).Run()
		_ = exec.Command("sudo", "systemctl", "disable", "--now", legacyLinuxUnit).Run()
		_ = sudo("rm", "-f", "/etc/systemd/system/"+linuxDaemonUnit)
		_ = sudo("rm", "-f", "/etc/systemd/system/"+legacyLinuxUnit)
		_ = sudo("systemctl", "daemon-reload")
		_ = sudo("rm", "-rf", RootDir())
		return nil
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

// DaemonRunning reports whether the boot helper's process is loaded/active.
func DaemonRunning() bool {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", "system/"+darwinDaemonLabel).CombinedOutput()
		return err == nil && strings.Contains(string(out), "state")
	case "linux":
		return exec.Command("systemctl", "is-active", "--quiet", linuxDaemonUnit).Run() == nil
	default:
		return false
	}
}

func (m *Manager) darwinInstallDaemon() error {
	logs := filepath.Join(m.StateDir, "logs", "daemon.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>_daemon</string>
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
`, darwinDaemonLabel, RootBinary("roxey"), m.StateDir, logs, logs)

	tmp, err := os.CreateTemp("", "roxey-daemon-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(plist); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	dst := "/Library/LaunchDaemons/" + darwinDaemonLabel + ".plist"
	// Retire any legacy relay-only unit before installing the daemon.
	_ = exec.Command("launchctl", "bootout", "system", legacyDarwinLabel).Run()
	_ = sudo("rm", "-f", "/Library/LaunchDaemons/"+legacyDarwinLabel+".plist")
	_ = exec.Command("launchctl", "bootout", "system", darwinDaemonLabel).Run()
	if err := sudo("cp", tmp.Name(), dst); err != nil {
		return err
	}
	if err := sudo("plutil", "-lint", dst); err != nil {
		return err
	}
	return sudo("launchctl", "bootstrap", "system", dst)
}

func (m *Manager) linuxInstallDaemon() error {
	unit := fmt.Sprintf(`[Unit]
Description=Roxey privileged helper (local relay, /etc/hosts, CA trust)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s _daemon --state-dir %s
Restart=always
User=root

[Install]
WantedBy=multi-user.target
`, RootBinary("roxey"), m.StateDir)

	tmp, err := os.CreateTemp("", "roxey-daemon-*.service")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(unit); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	dst := "/etc/systemd/system/" + linuxDaemonUnit
	_ = exec.Command("sudo", "systemctl", "disable", "--now", legacyLinuxUnit).Run()
	_ = sudo("rm", "-f", "/etc/systemd/system/"+legacyLinuxUnit)
	if err := sudo("cp", tmp.Name(), dst); err != nil {
		return err
	}
	for _, c := range [][]string{
		{"sudo", "systemctl", "daemon-reload"},
		{"sudo", "systemctl", "enable", "--now", linuxDaemonUnit},
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
