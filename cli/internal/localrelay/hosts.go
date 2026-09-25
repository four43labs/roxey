package localrelay

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/four43labs/roxey/cli/internal/config"
)

const (
	hostsBegin = "# BEGIN roxey (managed by `roxey up`; do not edit)"
	hostsEnd   = "# END roxey"
)

// NeedsHosts reports whether <host>.<tld> names need /etc/hosts entries
// (.localhost resolves natively; custom TLDs do not).
func NeedsHosts(tld string) bool {
	return !strings.HasSuffix(tld, ".localhost") && tld != "localhost"
}

// HostsEntries returns the loopback hostnames to sync for a manifest:
// roxey.<tld> plus every environment host.
func HostsEntries(tld string, envHosts []string) []string {
	out := []string{RelayHost(tld)}
	for _, h := range envHosts {
		out = append(out, h+"."+tld)
	}
	if net.ParseIP(tld) == nil && strings.Count(strings.TrimSuffix(tld, "."), ".") == 0 {
		out = append(out, tld) // bare TLD too so https://<tld>/ style URLs resolve
	}
	return out
}

// RenderBlock returns the managed /etc/hosts block for entries, or "" when
// there is nothing to claim. It is pure so the privileged helper daemon can
// rebuild the block server-side from validated entries.
func RenderBlock(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	return hostsBegin + "\n127.0.0.1 " + strings.Join(entries, " ") + "\n" + hostsEnd + "\n"
}

// SyncHosts replaces the managed roxey block in /etc/hosts with entries
// (sudo-elevated). No-op for .localhost TLDs.
func SyncHosts(entries []string) error {
	if len(entries) == 0 {
		return nil
	}
	return writeHosts(RenderBlock(entries))
}

// RemoveHosts drops the managed block from /etc/hosts.
func RemoveHosts() error { return writeHosts("") }

// WriteManagedBlock replaces the managed roxey block in /etc/hosts with block
// (which must come from RenderBlock). When running as root (the helper daemon)
// it writes directly; otherwise it elevates with sudo.
func WriteManagedBlock(block string) error { return writeHosts(block) }

func writeHosts(newBlock string) error {
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	content := mergeHostsBlock(string(data), newBlock)
	if content == string(data) {
		return nil
	}

	if os.Geteuid() == 0 {
		return writeHostsRoot(content)
	}

	tmp, err := os.CreateTemp("", "roxey-hosts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cmd := exec.Command("sudo", "-p", "roxey needs admin rights to update /etc/hosts: ",
		"cp", tmp.Name(), "/etc/hosts")
	cmd.Stdin = os.Stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("update /etc/hosts: %v: %s", err, out)
	}
	return nil
}

// writeHostsRoot atomically replaces /etc/hosts as root via a same-directory
// temp file, so an interrupted write can never truncate it.
func writeHostsRoot(content string) error {
	tmp, err := os.CreateTemp("/etc", "roxey-hosts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, "/etc/hosts")
}

func mergeHostsBlock(existing, newBlock string) string {
	lines := strings.Split(existing, "\n")
	var kept []string
	inBlock := false
	for _, l := range lines {
		switch {
		case l == hostsBegin:
			inBlock = true
			continue
		case l == hostsEnd:
			inBlock = false
			continue
		case inBlock:
			continue
		default:
			kept = append(kept, l)
		}
	}
	content := strings.Join(kept, "\n")
	if content != "" && !strings.HasSuffix(content, "\n\n") {
		content = strings.TrimRight(content, "\n") + "\n\n"
	}
	content += newBlock
	return content
}

// HostsSynced is a small helper for callers to log where state went.
func HostsSyncedPath() string { return filepath.Join(config.Dir(), "hosts-synced") }
