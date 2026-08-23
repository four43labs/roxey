// Command roxey is the local CLI: authenticate against Roxey relays, start
// ad-hoc tunnels, or bring up a whole project topology from a roxey.yaml
// manifest (proxy routes, spawned services, and — in local mode — the
// relay itself).
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"roxey/internal/config"
	"roxey/internal/localca"
	"roxey/internal/localrelay"
	"roxey/internal/manifest"
	"roxey/internal/runner"
	"roxey/internal/tunnel"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultTLD() string { return env("ROXEY_DOMAIN", "f43.run") }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "list":
		err = cmdList()
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "_run": // internal: background tunnel worker
		err = cmdRun(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Usage: roxey <command> [args]

Commands:
  auth [--tld <tld>] [api-key]        Save an API key for a relay server
  up [-d] [roxey.yaml]                Bring up a manifest's environments
  down [roxey.yaml]                   Tear down a manifest's tunnels + services
  logs <name>                         Tail a service started by ` + "`up -d`" + `
  start <service>[/path/*] <target>   Ad-hoc tunnel, e.g. roxey start myapp localhost:3000
  stop <service>[/path]               Stop a running tunnel
  list                                List tunnels running on this machine`)
}

// ── auth ─────────────────────────────────────────────────────────────────

func cmdAuth(args []string) error {
	tld := defaultTLD()
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--tld" && i+1 < len(args) {
			tld = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}

	apiKey := ""
	if len(rest) > 0 {
		apiKey = rest[0]
	} else {
		fmt.Print("Paste your Roxey API key: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		apiKey = strings.TrimSpace(line)
	}
	if apiKey == "" {
		return fmt.Errorf("no API key provided")
	}

	if err := config.SetServer(tld, config.ServerConfig{APIKey: apiKey}); err != nil {
		return err
	}
	fmt.Printf("Saved. Relay host: %s\n", localrelay.RelayHost(tld))
	return nil
}

// ── ad-hoc tunnels ───────────────────────────────────────────────────────

// parseServiceSpec splits "myapp/api/*" into ("myapp", "/api"); a bare
// "myapp" has no path prefix (roots the whole subdomain).
func parseServiceSpec(spec string) (service, pathPrefix string) {
	idx := strings.Index(spec, "/")
	if idx == -1 {
		return spec, ""
	}
	service = spec[:idx]
	p := strings.TrimSuffix(spec[idx:], "/*")
	p = strings.TrimSuffix(p, "/")
	return service, p
}

func tunnelKey(service, pathPrefix string) string {
	if pathPrefix == "" {
		return service
	}
	return service + pathPrefix
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// startOne spawns the detached tunnel worker for one route and records it.
func startOne(tld, service, pathPrefix, target, manifestPath string) error {
	key := tunnelKey(service, pathPrefix)

	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	if existing, ok := tunnels[key]; ok && processAlive(existing.PID) {
		return fmt.Errorf("tunnel already running for %s (pid %d)", key, existing.PID)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.LogDir(), 0700); err != nil {
		return err
	}
	logPath := filepath.Join(config.LogDir(), strings.ReplaceAll(key, "/", "_")+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	proc := exec.Command(self, "_run", tld, service, pathPrefix, target)
	proc.Stdout = logFile
	proc.Stderr = logFile
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from this terminal/session
	if err := proc.Start(); err != nil {
		return err
	}
	pid := proc.Process.Pid
	_ = proc.Process.Release()

	tunnels[key] = config.TunnelInfo{
		PID: pid, Service: service, PathPrefix: pathPrefix, TLD: tld, Target: target,
		ManifestPath: manifestPath,
		StartedAt:    time.Now().UTC().Format(time.RFC3339), LogFile: logPath,
	}
	return config.SaveTunnels(tunnels)
}

func cmdStart(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roxey start <service-name>[/path/*] <localhost:port>")
	}
	spec, target := args[0], args[1]
	tld := defaultTLD()

	sc, ok := config.ServerFor(tld)
	if !ok || sc.APIKey == "" {
		return fmt.Errorf("no API key saved for %s, run `roxey auth --tld %s <key>` first", tld, tld)
	}

	service, pathPrefix := parseServiceSpec(spec)
	if err := startOne(tld, service, pathPrefix, target, ""); err != nil {
		return err
	}
	fmt.Printf("Tunnel started: https://%s.%s%s -> %s\n", service, tld, pathPrefix, target)
	return nil
}

func stopKey(tunnels map[string]config.TunnelInfo, k string) {
	info := tunnels[k]
	if proc, err := os.FindProcess(info.PID); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	delete(tunnels, k)
}

func cmdStop(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roxey stop <service-name>[/path]")
	}
	spec := args[0]
	service, pathPrefix := parseServiceSpec(spec)
	key := tunnelKey(service, pathPrefix)

	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}

	var keysToStop []string
	if _, ok := tunnels[key]; ok {
		keysToStop = []string{key}
	} else if !strings.Contains(spec, "/") {
		for k, info := range tunnels {
			if info.Service == service {
				keysToStop = append(keysToStop, k)
			}
		}
	}
	if len(keysToStop) == 0 {
		return fmt.Errorf("no running tunnel found for %s", spec)
	}

	for _, k := range keysToStop {
		info := tunnels[k]
		stopKey(tunnels, k)
		fmt.Printf("Stopped %s (pid %d)\n", k, info.PID)
	}
	return config.SaveTunnels(tunnels)
}

func cmdList() error {
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	services, err := config.LoadServices()
	if err != nil {
		return err
	}
	if len(tunnels) == 0 && len(services) == 0 {
		fmt.Println("No active tunnels or services.")
		return nil
	}
	for key, info := range tunnels {
		status := "running"
		if !processAlive(info.PID) {
			status = "dead"
		}
		fmt.Printf("%s\tpid %d\t%s\t-> %s\tsince %s\n", key, info.PID, status, info.Target, info.StartedAt)
	}
	for name, svc := range services {
		status := "running"
		if !processAlive(svc.PID) {
			status = "dead"
		}
		fmt.Printf("%s [service]\tpid %d\t%s\tport %d\tcmd %s\n", name, svc.PID, status, svc.Port, svc.Command)
	}
	return nil
}

func cmdRun(args []string) error {
	if len(args) < 4 {
		return fmt.Errorf("usage: roxey _run <tld> <service> <pathPrefix> <target>")
	}
	return tunnel.Run(args[0], args[1], args[2], args[3])
}

// ── up / down ────────────────────────────────────────────────────────────

type runRoute struct {
	name  string // env host + path, unique within manifest
	env   string // environment host label
	route *manifest.Route
}

func cmdUp(args []string) error {
	detach := false
	var file string
	for _, a := range args {
		switch {
		case a == "-d" || a == "--detach":
			detach = true
		case file == "":
			file = a
		default:
			return fmt.Errorf("unexpected argument %q", a)
		}
	}
	if file == "" {
		file = "roxey.yaml"
	}
	absFile, err := filepath.Abs(file)
	if err != nil {
		return err
	}

	m, err := manifest.Load(absFile)
	if err != nil {
		return err
	}
	tld := m.RelayServer.TLD

	// Resolve the relay and API key.
	apiKey := m.RelayServer.APIKey
	if m.RelayServer.Local {
		if err := ensureLocalRelay(m); err != nil {
			return err
		}
	} else if apiKey == "" {
		sc, ok := config.ServerFor(tld)
		if !ok || sc.APIKey == "" {
			return fmt.Errorf("no API key for %s; set relay_server.api_key or run `roxey auth --tld %s <key>`", tld, tld)
		}
		apiKey = sc.APIKey
	}
	if apiKey != "" {
		if err := config.SetServer(tld, config.ServerConfig{
			APIKey: apiKey,
			Local:  m.RelayServer.Local,
		}); err != nil {
			return err
		}
	}

	// Collect run-routes and spawn services first.
	var runs []runRoute
	var proxyRoutes []runRoute
	for ei := range m.Environments {
		e := &m.Environments[ei]
		for ri := range e.Routes {
			r := &e.Routes[ri]
			rr := runRoute{name: e.Host + r.Path, env: e.Host, route: r}
			if r.IsRun() {
				runs = append(runs, rr)
			} else {
				proxyRoutes = append(proxyRoutes, rr)
			}
		}
	}

	services, err := config.LoadServices()
	if err != nil {
		return err
	}
	services = runner.PruneDead(services)

	childDone := make(map[string]<-chan struct{})

	startService := func(rr runRoute) error {
		r := rr.route
		// Idempotent: skip services already running for this manifest.
		if existing, ok := services[svcKey(absFile, rr.name)]; ok && processAlive(existing.PID) {
			fmt.Printf("[service] %-24s already running (pid %d)\n", rr.name, existing.PID)
			return nil
		}
		logPath := filepath.Join(config.LogDir(), sanitizeName(rr.name)+".log")
		opts := runner.SpawnOptions{
			Name: rr.name, Cwd: r.Cwd, Command: r.Command, Port: r.Port,
			Environment: r.Environment, LogFile: logPath,
		}
		if detach {
			pid, err := runner.Spawn(opts)
			if err != nil {
				return fmt.Errorf("start %s: %w", rr.name, err)
			}
			services[svcKey(absFile, rr.name)] = config.ServiceInfo{
				Name: rr.name, PID: pid, Port: r.Port, Command: r.Command, Cwd: r.Cwd,
				ManifestPath: absFile, StartedAt: now(), LogFile: logPath,
			}
			fmt.Printf("[service] %-24s pid %-7d port %d\n", rr.name, pid, r.Port)
			return nil
		}

		pid, done, err := runner.SpawnForeground(opts, func(line string) {
			fmt.Printf("[%s] %s\n", rr.name, line)
		})
		if err != nil {
			return fmt.Errorf("start %s: %w", rr.name, err)
		}
		childDone[rr.name] = done
		services[svcKey(absFile, rr.name)] = config.ServiceInfo{
			Name: rr.name, PID: pid, Port: r.Port, Command: r.Command, Cwd: r.Cwd,
			ManifestPath: absFile, StartedAt: now(),
		}
		fmt.Printf("[service] %-24s pid %-7d port %d\n", rr.name, pid, r.Port)
		return nil
	}
	for _, rr := range runs {
		if err := startService(rr); err != nil {
			return err
		}
	}
	if err := config.SaveServices(services); err != nil {
		return err
	}

	// Open tunnels for every route.
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	for _, rr := range append(append([]runRoute{}, runs...), proxyRoutes...) {
		r := rr.route
		target := r.Target
		if r.IsRun() {
			target = fmt.Sprintf("localhost:%d", r.Port)
		}
		pathPrefix := strings.TrimSuffix(r.Path, "/") // "/" (root) registers as ""
		key := tunnelKey(rr.env, pathPrefix)
		if existing, ok := tunnels[key]; ok && processAlive(existing.PID) {
			continue // idempotent re-up
		}
		if err := startOne(tld, rr.env, pathPrefix, target, absFile); err != nil {
			return err
		}
	}

	fmt.Println()
	for _, e := range m.Environments {
		fmt.Printf("%-32s", m.PublicURL(e.Host))
		var parts []string
		for _, r := range e.Routes {
			if r.IsRun() {
				parts = append(parts, r.Path+" -> "+r.Command)
			} else {
				parts = append(parts, r.Path+" -> "+r.Target)
			}
		}
		fmt.Printf("  %s\n", strings.Join(parts, ", "))
	}
	fmt.Println()

	if detach {
		return config.SaveServices(services)
	}
	return waitForeground(childDone, absFile)
}

// waitForeground blocks until Ctrl-C or a spawned child exits, then tears
// everything belonging to this manifest down.
func waitForeground(childDone map[string]<-chan struct{}, manifestPath string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	exited := make(chan string, 1)
	for name, done := range childDone {
		go func(name string, done <-chan struct{}) {
			<-done
			exited <- name
		}(name, done)
	}

	var err error
	select {
	case s := <-sig:
		fmt.Printf("\n[roxey] %v received, shutting down...\n", s)
	case name := <-exited:
		err = fmt.Errorf("service %s exited unexpectedly; shutting down", name)
	}
	if e := teardownManifest(manifestPath); e != nil && err == nil {
		err = e
	}
	if err == nil {
		fmt.Println("[roxey] all tunnels and services stopped.")
	}
	return err
}

// teardownManifest stops the tunnels and kills the services recorded for
// manifestPath.
func teardownManifest(manifestPath string) error {
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	for k, info := range tunnels {
		if info.ManifestPath != manifestPath {
			continue
		}
		stopKey(tunnels, k)
		fmt.Printf("[tunnel] stopped %s\n", k)
	}
	if err := config.SaveTunnels(tunnels); err != nil {
		return err
	}

	services, err := config.LoadServices()
	if err != nil {
		return err
	}
	for k, svc := range services {
		if svc.ManifestPath != manifestPath {
			continue
		}
		runner.KillGroup(svc.PID)
		delete(services, k)
		fmt.Printf("[service] stopped %s\n", svc.Name)
	}
	return config.SaveServices(services)
}

// ensureLocalRelay provisions CA/trust/hosts and makes sure the local relay
// is running with an API key saved.
func ensureLocalRelay(m *manifest.Manifest) error {
	tld := m.RelayServer.TLD

	var hosts []string
	for _, e := range m.Environments {
		hosts = append(hosts, e.Host+"."+tld)
	}
	hosts = append(hosts, localrelay.RelayHost(tld))
	if tld == "localhost" {
		hosts = append(hosts, "localhost")
	}

	res, err := localca.Ensure(hosts)
	if err != nil {
		return fmt.Errorf("local certs: %w", err)
	}
	if !localca.Trusted(res.CAPath) {
		fmt.Println("[certs] trusting local CA (may ask for your password)...")
		if err := localca.TrustCA(res.CAPath); err != nil {
			return err
		}
	}

	if localrelay.NeedsHosts(tld) {
		if err := localrelay.SyncHosts(localrelay.HostsEntries(tld, envHostsOf(m))); err != nil {
			return fmt.Errorf("/etc/hosts sync: %w", err)
		}
	}

	if err := localrelay.EnsureRunning(tld); err != nil {
		return err
	}

	sc, _ := config.ServerFor(tld)
	if sc.APIKey == "" {
		key, err := localrelay.CreateAPIKey(tld, "auto-created by roxey up")
		if err != nil {
			return err
		}
		sc.APIKey = key
		if err := config.SetServer(tld, sc); err != nil {
			return err
		}
		fmt.Printf("[relay] created API key for %s\n", tld)
	}
	m.RelayServer.APIKey = sc.APIKey
	return nil
}

func envHostsOf(m *manifest.Manifest) []string {
	out := make([]string, 0, len(m.Environments))
	for _, e := range m.Environments {
		out = append(out, e.Host)
	}
	return out
}

func cmdDown(args []string) error {
	var file string
	for _, a := range args {
		if a == "-d" || a == "--detach" {
			continue // tolerated for symmetry with up
		}
		if file == "" {
			file = a
		} else {
			return fmt.Errorf("unexpected argument %q", a)
		}
	}
	if file == "" {
		file = "roxey.yaml"
	}
	absFile, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	m, err := manifest.Load(absFile)
	if err != nil {
		return err
	}
	tld := m.RelayServer.TLD

	// Stop this manifest's tunnels.
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	stopped := 0
	for k, info := range tunnels {
		if info.ManifestPath != absFile && !(info.ManifestPath == "" && sameHost(k, m)) {
			continue
		}
		stopKey(tunnels, k)
		stopped++
	}
	if err := config.SaveTunnels(tunnels); err != nil {
		return err
	}

	// Kill this manifest's services.
	services, err := config.LoadServices()
	if err != nil {
		return err
	}
	killed := 0
	for k, svc := range services {
		if svc.ManifestPath != absFile {
			continue
		}
		runner.KillGroup(svc.PID)
		delete(services, k)
		killed++
	}
	if err := config.SaveServices(services); err != nil {
		return err
	}

	// If no other live entries still use this TLD's hosts block, clean it.
	if m.RelayServer.Local && localrelay.NeedsHosts(tld) && !anySurvivorOn(tunnels, services, tld) {
		if err := localrelay.RemoveHosts(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not clean /etc/hosts:", err)
		}
	}

	fmt.Printf("Stopped %d tunnel(s), %d service(s).\n", stopped, killed)
	return nil
}

func anySurvivorOn(tunnels map[string]config.TunnelInfo, services map[string]config.ServiceInfo, tld string) bool {
	for _, t := range tunnels {
		if processAlive(t.PID) && t.TLD == tld {
			return true
		}
	}
	for _, s := range services {
		if processAlive(s.PID) {
			return true
		}
	}
	return false
}

func sameHost(key string, m *manifest.Manifest) bool {
	for _, e := range m.Environments {
		if key == e.Host || strings.HasPrefix(key, e.Host+"/") {
			return true
		}
	}
	return false
}

// ── logs ─────────────────────────────────────────────────────────────────

func cmdLogs(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roxey logs <name>")
	}
	name := args[0]
	services, err := config.LoadServices()
	if err != nil {
		return err
	}
	var logFile string
	for _, svc := range services {
		if svc.Name == name {
			logFile = svc.LogFile
		}
	}
	if logFile == "" {
		logFile = filepath.Join(config.LogDir(), strings.ReplaceAll(name, "/", "_")+".log")
	}
	if _, err := os.Stat(logFile); err != nil {
		return fmt.Errorf("no logs found for %s", name)
	}
	cmd := exec.Command("tail", "-n", "40", "-f", logFile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	return cmd.Run()
}

// ── helpers ──────────────────────────────────────────────────────────────

func svcKey(manifestPath, name string) string {
	base := filepath.Base(filepath.Dir(manifestPath))
	return strings.ReplaceAll(base, ":", "_") + ":" + name
}

func sanitizeName(n string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(n)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
