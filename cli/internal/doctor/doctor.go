// Package doctor implements `roxey doctor`: read-only diagnostics for the
// CLI state, the local relay (binary, CA, port 443, hosts entries), remote
// relays, and an optional roxey.yaml manifest. Each check reports ok/warn/
// fail with a suggested fix; any fail makes the command exit non-zero.
package doctor

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/four43labs/roxey/cli/internal/config"
	"github.com/four43labs/roxey/cli/internal/helper"
	"github.com/four43labs/roxey/cli/internal/localca"
	"github.com/four43labs/roxey/cli/internal/localrelay"
	"github.com/four43labs/roxey/cli/internal/service"
)

type Status int

const (
	OK Status = iota
	Warn
	Fail
)

type Result struct {
	Status Status
	Msg    string
	Fix    string // suggested remedy, printed under warn/fail lines
}

type Report struct {
	Results []Result
}

func (r *Report) OKf(format string, a ...any) {
	r.Results = append(r.Results, Result{Status: OK, Msg: fmt.Sprintf(format, a...)})
}

func (r *Report) Warnf(format string, a ...any) {
	r.Results = append(r.Results, Result{Status: Warn, Msg: fmt.Sprintf(format, a...)})
}

func (r *Report) Failf(fix, format string, a ...any) {
	r.Results = append(r.Results, Result{Status: Fail, Msg: fmt.Sprintf(format, a...), Fix: fix})
}

func (r *Report) Failed() bool {
	for _, res := range r.Results {
		if res.Status == Fail {
			return true
		}
	}
	return false
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ── CLI & local state ────────────────────────────────────────────────────

// CheckState inspects ~/.roxey: config validity, registered relays, and
// stale tunnel/service entries.
func CheckState(r *Report) {
	cfg, err := config.LoadConfig()
	if err != nil {
		r.Failf("inspect "+config.Dir()+"/config.json — delete it and re-run `roxey auth`",
			"config.json is unreadable: %v", err)
		return
	}
	if cfg == nil || len(cfg.Servers) == 0 {
		r.Warnf("no relay servers configured")
		r.Results[len(r.Results)-1].Fix = "run `roxey auth <api-key>` or `roxey up` with a manifest"
		return
	}

	for tld, sc := range sortedServers(cfg) {
		switch {
		case sc.APIKey == "":
			if sc.Local {
				r.Warnf("%s: local relay without saved API key", tld)
				r.Results[len(r.Results)-1].Fix = "key is auto-created on next `roxey up`"
			} else {
				r.Failf("`roxey auth --tld "+tld+" <key>`", "%s: no API key saved", tld)
			}
		default:
			r.OKf("%s: API key saved", tld)
		}
	}

	tunnels, err := config.LoadTunnels()
	if err != nil {
		r.Failf("delete or fix "+config.Dir()+"/tunnels.json", "tunnels.json unreadable: %v", err)
	} else {
		stale := 0
		for key, info := range tunnels {
			if !procAlive(info.PID) {
				stale++
				r.Warnf("stale tunnel %s (pid %d not running)", key, info.PID)
			}
		}
		if stale > 0 {
			r.Results[len(r.Results)-1].Fix = "entries are pruned automatically on the next `roxey up`/`down` of that manifest"
		}
		if stale == 0 && len(tunnels) > 0 {
			r.OKf("%d tunnel entr%s tracked, all alive", len(tunnels), plural(len(tunnels), "y", "ies"))
		}
	}

	services, err := config.LoadServices()
	if err != nil {
		r.Failf("delete or fix "+config.Dir()+"/services.json", "services.json unreadable: %v", err)
	} else {
		for name, svc := range services {
			if !procAlive(svc.PID) {
				r.Warnf("stale service %s (pid %d not running)", name, svc.PID)
				r.Results[len(r.Results)-1].Fix = "pruned automatically on next `roxey up`"
			}
		}
	}
}

// ── local relay mode ─────────────────────────────────────────────────────

// CheckLocalRelay validates everything `relay_server.local` needs for tld:
// relay binary, CA trust, port 443 ownership, relay health, hosts entries.
func CheckLocalRelay(r *Report, tld string) {
	// Relay binary.
	if p := os.Getenv("ROXEY_RELAY_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil {
			r.Failf("fix ROXEY_RELAY_BIN or unset it", "ROXEY_RELAY_BIN=%s does not exist", p)
		} else {
			r.OKf("relay binary override: %s", p)
		}
	} else if bin := localrelay.BinPath(); fileExecutable(bin) {
		r.OKf("relay binary present: %s", bin)
	} else {
		r.Warnf("relay binary not downloaded yet (%s)", bin)
		r.Results[len(r.Results)-1].Fix = "downloaded automatically on first `roxey up`; needs GitHub Releases reachability"
	}

	// CA + trust.
	caPath := filepath.Join(config.CertDir(), "ca.crt")
	if _, err := os.Stat(caPath); err != nil {
		r.Failf("generated automatically on next `roxey up`", "local CA missing (%s)", caPath)
	} else if localca.Trusted(caPath) {
		r.OKf("local CA trusted by system store")
	} else {
		r.Failf("re-run `roxey up` to install the CA into the system trust store",
			"local CA exists but is NOT trusted by the system store")
	}

	// Port 443 ownership.
	switch {
	case localrelay.HealthOK(tld):
		r.OKf("local relay healthy at roxey.%s (port 443)", tld)
	case portInUse("127.0.0.1:443"):
		culprit := portOwner(":443")
		msg := "port 443 is occupied by another process"
		if culprit != "" {
			msg += " (" + culprit + ")"
		}
		r.Failf("stop that process (e.g. `portless proxy stop`, `sudo lsof -i :443`) before `roxey up`", "%s", msg)
	default:
		r.Failf("start it with `roxey up` on any manifest with `relay_server.local: true`",
			"port 443 free but roxey.%s is not running", tld)
	}

	CheckHostsBlock(r, tld)
}

// CheckHostsBlock validates the managed /etc/hosts block against known TLDs.
func CheckHostsBlock(r *Report, tld string) {
	block, err := managedHostsEntries()
	if err != nil {
		r.Warnf("could not read /etc/hosts: %v", err)
		return
	}
	want := localrelay.HostsEntries(tld, nil)
	missing := 0
	for _, entry := range want {
		found := false
		for _, line := range block {
			if strings.Contains(line, entry) {
				found = true
				break
			}
		}
		if !found {
			missing++
			r.Failf("re-run `roxey up` to sync /etc/hosts", "hosts entry missing: %s", entry)
		}
	}
	if missing == 0 {
		r.OKf("/etc/hosts managed block present for %s", tld)
	}
}

// CheckHelper validates the privileged helper daemon and its socket.
func CheckHelper(r *Report) {
	if helper.Available() {
		r.OKf("privileged helper reachable (prompt-free local mode)")
		return
	}
	if service.DaemonRunning() {
		r.Failf("restart it (`roxey service install`, or reboot) — see ~/.roxey/logs/daemon.log",
			"helper daemon is loaded but its socket is unreachable")
		return
	}
	r.Warnf("privileged helper not installed; local mode will prompt for sudo")
	r.Results[len(r.Results)-1].Fix = "run `roxey setup` once to install it (no more prompts after)"
}

// ── remote relays ────────────────────────────────────────────────────────

// CheckRemoteRelay verifies DNS, TCP, and TLS for roxey.<tld>.
func CheckRemoteRelay(r *Report, tld string) {
	host := localrelay.RelayHost(tld)
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		r.Failf("add DNS records for *."+tld+" pointing at your relay host",
			"%s does not resolve", host)
		return
	}
	r.OKf("%s resolves to %s", host, ips[0])

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 5*time.Second)
	if err != nil {
		r.Failf("check that the relay is running and reachable", "%s:443 unreachable: %v", host, err)
		return
	}
	conn.Close()
	r.OKf("%s:443 reachable", host)

	d := &net.Dialer{Timeout: 5 * time.Second}
	tconn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, "443"),
		&tls.Config{ServerName: host})
	if err != nil {
		r.Failf("if this relay terminates TLS upstream (e.g. Cloudflare), ignore; "+
			"otherwise check its ROXEY_TLS_CERT/KEY setup", "%s TLS handshake failed: %v", host, err)
		return
	}
	state := tconn.ConnectionState()
	tconn.Close()
	r.OKf("%s TLS ok (cert verified, %s)", host, tls.VersionName(state.Version))
}

// ── manifest ─────────────────────────────────────────────────────────────

// ManifestDeps lets cmdDoctor pass in already-loaded pieces without this
// package importing main.
type ManifestInfo struct {
	TLD       string
	Local     bool
	HasAPIKey bool
	// SkipRelayChecks suppresses the local/remote relay section (already
	// run for this TLD at the top level).
	SkipRelayChecks bool
	// OwnedPorts maps ports currently bound by this project's own live
	// services (from services.json) so doctor doesn't flag them.
	OwnedPorts map[int]string
	RunRoutes  []RunRoute
	ProxyTargs []string // proxy targets to probe
}

type RunRoute struct {
	Name    string
	Cwd     string
	Command string
	Port    int
}

// CheckManifest validates run-routes and proxy targets of a loaded manifest.
func CheckManifest(r *Report, mi ManifestInfo) {
	if mi.TLD == "" {
		return
	}
	if mi.Local && !mi.SkipRelayChecks {
		CheckLocalRelay(r, mi.TLD)
	} else if !mi.Local && !mi.SkipRelayChecks && !mi.HasAPIKey {
		r.Failf("`roxey auth --tld "+mi.TLD+" <key>` or set relay_server.api_key",
			"no API key for %s", mi.TLD)
	}

	for _, rr := range mi.RunRoutes {
		if st, err := os.Stat(rr.Cwd); err != nil || !st.IsDir() {
			r.Failf("fix cwd in roxey.yaml", "%s: cwd %q missing", rr.Name, rr.Cwd)
		}
		first := firstWord(rr.Command)
		if first == "bash" || first == "sh" {
			// shell builtins/scripts inside -c strings can't be LookPath'd
			r.OKf("%s: runs via shell (%s …)", rr.Name, first)
		} else if _, err := exec.LookPath(first); err != nil {
			r.Failf("install it or adjust the command in roxey.yaml",
				"%s: command %q not found on PATH", rr.Name, first)
		} else {
			r.OKf("%s: %q found on PATH", rr.Name, first)
		}
		if portInUse(fmt.Sprintf("127.0.0.1:%d", rr.Port)) {
			if owner, ours := mi.OwnedPorts[rr.Port]; ours {
				r.OKf("%s: port %d in use by this project's service %s", rr.Name, rr.Port, owner)
			} else {
				r.Failf("free the port or stop its owner before `roxey up`",
					"%s: port %d already bound by another process", rr.Name, rr.Port)
			}
		} else {
			r.OKf("%s: port %d free", rr.Name, rr.Port)
		}
	}

	for _, target := range mi.ProxyTargs {
		addr := normalizeTarget(target)
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			r.Warnf("proxy target %s not accepting connections right now", addr)
			r.Results[len(r.Results)-1].Fix = "fine if you start it after `roxey up`"
			continue
		}
		conn.Close()
		r.OKf("proxy target %s reachable", addr)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func portInUse(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// portOwner best-effort identifies the listener via lsof (may be empty
// without sudo for root-owned processes).
func portOwner(port string) string {
	out, err := exec.Command("lsof", "-nP", "-i", port, "-sTCP:LISTEN").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) >= 2 {
		return fields[0] + " (pid " + fields[1] + ")"
	}
	return ""
}

func fileExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0
}

func managedHostsEntries() ([]string, error) {
	f, err := os.Open("/etc/hosts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	inBlock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "# BEGIN roxey (managed by `roxey up`; do not edit)":
			inBlock = true
			continue
		case "# END roxey":
			inBlock = false
			continue
		}
		if inBlock && line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func normalizeTarget(t string) string {
	t = strings.TrimPrefix(strings.TrimPrefix(t, "http://"), "https://")
	if !strings.Contains(t, ":") {
		t = "localhost:" + t
	}
	return t
}

func firstWord(cmdline string) string {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func sortedServers(cfg *config.Config) map[string]config.ServerConfig {
	// map iteration order is fine here; named helper just for readability.
	return cfg.Servers
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
