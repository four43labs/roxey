package helper

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"roxey/internal/config"
	"roxey/internal/localca"
	"roxey/internal/localrelay"
	"roxey/internal/projects"
)

// RunDaemon is the root entrypoint (`roxey _daemon`): it reconciles certs,
// CA trust, and the managed /etc/hosts block from the registry, supervises
// the port-443 relay, and serves the helper socket until SIGTERM.
func RunDaemon(stateDir, relayBin string) error {
	config.SetDir(stateDir)

	uid, err := dirOwner(stateDir)
	if err != nil {
		return err
	}

	if err := reconcile(uid); err != nil {
		fmt.Fprintln(os.Stderr, "[daemon] initial reconcile:", err)
	}

	relay := NewRelaySupervisor(relayBin)
	ReclaimPort443()
	if tld := primaryTLD(); tld != "" {
		if err := relay.Ensure(tld); err != nil {
			fmt.Fprintln(os.Stderr, "[daemon] start relay:", err)
		}
	}

	srv := NewServer(stateDir, uid, relay)
	l, err := srv.Listen()
	if err != nil {
		relay.Stop()
		return err
	}
	fmt.Printf("[daemon] listening on %s (uid %d)\n", config.SocketPath(), uid)

	go func() {
		if err := srv.Serve(l); err != nil {
			fmt.Fprintln(os.Stderr, "[daemon] serve:", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	relay.Stop()
	_ = l.Close()
	return nil
}

// reconcile provisions certs for every registered local host, trusts the CA,
// and writes the managed /etc/hosts block. Cert files are chowned back to the
// state-dir owner so the unprivileged CLI can keep reissuing them.
func reconcile(uid uint32) error {
	reg, err := projects.Load()
	if err != nil {
		return err
	}
	hosts := reg.AllLocalHostFQDNs()
	if tld := primaryTLD(); tld == "localhost" {
		hosts = append(hosts, "localhost")
	}
	hosts = dedupe(hosts)
	if len(hosts) == 0 {
		return nil
	}

	res, err := localca.Ensure(hosts)
	if err != nil {
		return fmt.Errorf("certs: %w", err)
	}
	chownCerts(uid)

	if !localca.Trusted(res.CAPath) {
		if err := localca.TrustCA(res.CAPath); err != nil {
			fmt.Fprintln(os.Stderr, "[daemon] trust CA:", err)
		}
	}
	return localrelay.WriteManagedBlock(localrelay.RenderBlock(hosts))
}

func primaryTLD() string {
	reg, err := projects.Load()
	if err != nil || len(reg.Projects) == 0 {
		return ""
	}
	return reg.PrimaryLocalTLD()
}

// dirOwner returns the uid owning dir (the real user when the daemon runs as
// root with the user's state directory).
func dirOwner(dir string) (uint32, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot determine owner of %s", dir)
	}
	return sys.Uid, nil
}

func chownCerts(uid uint32) {
	dir := config.CertDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Chown(filepath.Join(dir, e.Name()), int(uid), -1)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
