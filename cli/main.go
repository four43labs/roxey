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
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"roxey/internal/config"
	"roxey/internal/doctor"
	"roxey/internal/localca"
	"roxey/internal/localrelay"
	"roxey/internal/manifest"
	"roxey/internal/preview"
	"roxey/internal/projects"
	"roxey/internal/runner"
	"roxey/internal/service"
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
	case "projects":
		err = cmdProjects(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "_service-relay": // internal: boot-time relay entrypoint (runs as root)
		err = cmdServiceRelay(os.Args[2:])
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
  up [-d] [--preview[=slug]] [file]   Bring up a manifest's environments
  down [file]                         Tear down a manifest's tunnels + services
  projects [--forget n] [--prune]     List known projects and their status
  service install|uninstall|status    Manage the boot-time relay daemon
  logs <name>                         Tail a service started by ` + "`up -d`" + `
  doctor [roxey.yaml]                 Diagnose state, relays, certs, and ports
  start <service>[/path/*] <target>   Ad-hoc tunnel, e.g. roxey start myapp localhost:3000
  stop <service>[/path]               Stop a running tunnel
  list                                List tunnels running on this machine

up/down/logs/doctor accept --project <name> to run against a registered
project from any directory. Inside a linked git worktree (or with
--preview[=slug]), up creates an isolated fork preview.`)
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
	previewFlag := false
	var previewSlug string
	var file string
	var project string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-d" || a == "--detach":
			detach = true
		case a == "--preview":
			previewFlag = true
		case strings.HasPrefix(a, "--preview="):
			previewFlag = true
			previewSlug = strings.TrimPrefix(a, "--preview=")
		case a == "--project" && i+1 < len(args):
			i++
			project = args[i]
		case strings.HasPrefix(a, "--project="):
			project = strings.TrimPrefix(a, "--project=")
		case file == "" && !strings.HasPrefix(a, "-"):
			file = a
		default:
			return fmt.Errorf("unexpected argument %q", a)
		}
	}

	// --project resolves the manifest from the central registry.
	if project != "" {
		if file != "" {
			return fmt.Errorf("use either --project or a manifest path, not both")
		}
		reg, err := projects.Load()
		if err != nil {
			return err
		}
		p, ok := reg.Get(project)
		if !ok {
			return fmt.Errorf("unknown project %q — see `roxey projects`", project)
		}
		file = filepath.Join(p.Path, "roxey.yaml")
	}
	if previewFlag && previewSlug != "" {
		// explicit slug only makes sense from the manifest's own directory
	} else if previewFlag && file == "" {
		file = "roxey.yaml"
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

	// Preview identity: explicit flag wins; otherwise auto-detect worktrees.
	projectName := filepath.Base(filepath.Dir(absFile))
	branch := ""
	isPreview := previewFlag
	if previewFlag {
		if previewSlug != "" {
			branch = previewSlug
			previewSlug = preview.Slugify(previewSlug)
		} else if b, ok := preview.Detect(m.Dir); ok {
			branch = b
			previewSlug = preview.Slugify(b)
		} else {
			previewSlug = "local"
			branch = "local"
		}
	} else if b, ok := preview.Detect(m.Dir); ok {
		isPreview = true
		branch = b
		previewSlug = preview.Slugify(b)
	}
	if isPreview {
		projectName = preview.Apply(m, projectName, previewSlug)
	}
	m.ResolveEnvTemplates()

	absDir := m.Dir

	// Crash guard: if anything below panics, tear down whatever this run
	// already spawned before propagating.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "[roxey] panic — stopping services and tunnels started by this run")
			_ = teardownManifest(absFile)
			panic(r)
		}
	}()

	tld := m.RelayServer.TLD

	// Collect declared hosts for the registry (post-preview-rewrite).
	var envHosts []string
	for _, e := range m.Environments {
		envHosts = append(envHosts, e.Host)
	}

	// Auto-register / update this project in the central registry.
	reg, err := projects.Load()
	if err != nil {
		return err
	}
	registeredName, err := reg.Register(absDir, tld, m.RelayServer.Local, envHosts, branch, isPreview)
	if err != nil {
		return err
	}
	_ = registeredName

	// Resolve the relay and API key.
	apiKey := m.RelayServer.APIKey
	if m.RelayServer.Local {
		if err := ensureLocalRelay(m, reg); err != nil {
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
			if r.IsRun() && r.Port == 0 { // preview/auto port: assign a free one
				port, err := runner.FreePort()
				if err != nil {
					return fmt.Errorf("assign free port for %s%s: %w", e.Host, r.Path, err)
				}
				r.Port = port
			}
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

	// Restart semantics: anything this manifest left running from a
	// previous (possibly crashed) run is stopped first, then brought up
	// fresh. This keeps `roxey up` deterministic and orphans impossible.
	services = runner.PruneDead(services)
	for k, svc := range services {
		if svc.ManifestPath != absFile {
			continue
		}
		if processAlive(svc.PID) {
			runner.KillGroup(svc.PID)
			fmt.Printf("[service] restarted %-18s (was pid %d)\n", svc.Name, svc.PID)
		}
		delete(services, k)
	}
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	for k, info := range tunnels {
		if info.ManifestPath == absFile && processAlive(info.PID) {
			stopKey(tunnels, k)
		}
	}
	if err := config.SaveTunnels(tunnels); err != nil {
		return err
	}

	childDone := make(map[string]<-chan struct{})

	startService := func(rr runRoute) error {
		r := rr.route
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
	for _, rr := range append(append([]runRoute{}, runs...), proxyRoutes...) {
		r := rr.route
		target := r.Target
		if r.IsRun() {
			target = fmt.Sprintf("localhost:%d", r.Port)
		}
		pathPrefix := strings.TrimSuffix(r.Path, "/") // "/" (root) registers as ""
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

// ensureLocalRelay provisions CA/trust/hosts and makes sure the shared
// local relay is running with an API key saved. Cert SANs and the
// /etc/hosts block cover every registered local project (cumulative), so
// bringing one project up never breaks another's URLs.
func ensureLocalRelay(m *manifest.Manifest, reg *projects.Registry) error {
	tld := m.RelayServer.TLD

	// Cumulative host set: everything registered + this manifest's hosts.
	hosts := reg.AllLocalHostFQDNs()
	for _, e := range m.Environments {
		hosts = append(hosts, e.Host+"."+tld)
	}
	if tld == "localhost" {
		hosts = append(hosts, "localhost")
	}
	hosts = dedupe(hosts)

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
		if err := localrelay.SyncHosts(hosts); err != nil {
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

func cmdDown(args []string) error {
	var file string
	var project string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-d" || a == "--detach" {
			continue // tolerated for symmetry with up
		}
		switch {
		case strings.HasPrefix(a, "--project="):
			project = strings.TrimPrefix(a, "--project=")
		case a == "--project" && i+1 < len(args):
			i++
			project = args[i]
		case file == "":
			file = a
		default:
			return fmt.Errorf("unexpected argument %q", a)
		}
	}
	if project != "" {
		if file != "" {
			return fmt.Errorf("use either --project or a manifest path, not both")
		}
		reg, err := projects.Load()
		if err != nil {
			return err
		}
		p, ok := reg.Get(project)
		if !ok {
			return fmt.Errorf("unknown project %q — see `roxey projects`", project)
		}
		file = filepath.Join(p.Path, "roxey.yaml")
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

	// Registry bookkeeping: forget this project, then re-sync the hosts
	// block to whatever other projects still claim.
	reg, err := projects.Load()
	if err != nil {
		return err
	}
	for _, p := range reg.Projects {
		if projects.SameDir(p.Path, m.Dir) {
			reg.Forget(p.Name) // covers previews too
		}
	}
	if m.RelayServer.Local && localrelay.NeedsHosts(tld) {
		if err := localrelay.SyncHosts(reg.AllLocalHostFQDNs()); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not update /etc/hosts:", err)
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

// ── doctor ───────────────────────────────────────────────────────────────

func cmdDoctor(args []string) error {
	file := ""
	var project string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--project="):
			project = strings.TrimPrefix(a, "--project=")
		case a == "--project" && i+1 < len(args):
			i++
			project = args[i]
		case strings.HasPrefix(a, "-"):
			continue
		case file == "":
			file = a
		}
	}
	if project != "" {
		reg, err := projects.Load()
		if err != nil {
			return err
		}
		p, ok := reg.Get(project)
		if !ok {
			return fmt.Errorf("unknown project %q — see `roxey projects`", project)
		}
		file = filepath.Join(p.Path, "roxey.yaml")
	}

	rep := &doctor.Report{}
	doctor.CheckState(rep)

	// Relay checks for every registered TLD; remember which were covered so
	// the manifest section doesn't duplicate them.
	checkedRelay := map[string]bool{}
	cfg, _ := config.LoadConfig()
	if cfg != nil {
		tlds := make([]string, 0, len(cfg.Servers))
		for tld := range cfg.Servers {
			tlds = append(tlds, tld)
		}
		sort.Strings(tlds)
		for _, tld := range tlds {
			if sc := cfg.Servers[tld]; sc.Local {
				doctor.CheckLocalRelay(rep, tld)
			} else {
				doctor.CheckRemoteRelay(rep, tld)
			}
			checkedRelay[tld] = true
		}
	}

	// Boot-service status.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		fmt.Printf("service:\n")
		fmt.Printf("  relay daemon:  %s\n", onOff(service.RelayRunning()))
		reg2, _ := projects.Load()
		var auto []string
		for _, n := range reg2.SortedNames() {
			if p := reg2.Projects[n]; p.AutoStart && !p.Preview {
				state := "agent missing"
				if service.ProjectAgentInstalled(n) {
					state = "installed"
				}
				auto = append(auto, n+" ("+state+")")
			}
		}
		if len(auto) == 0 {
			fmt.Println("  autostart:     none")
		} else {
			fmt.Printf("  autostart:     %s\n", strings.Join(auto, ", "))
		}
	}

	// Manifest checks when one is available.
	if file == "" {
		if _, err := os.Stat("roxey.yaml"); err == nil {
			file = "roxey.yaml"
		}
	}
	if file != "" {
		m, err := manifest.Load(file)
		if err != nil {
			rep.Failf("fix the errors above in "+file, "manifest %s is invalid", file)
		} else {
			fmt.Printf("manifest: %s\n", file)
			mi := doctor.ManifestInfo{
				TLD:             m.RelayServer.TLD,
				Local:           m.RelayServer.Local,
				HasAPIKey:       m.RelayServer.APIKey != "",
				SkipRelayChecks: checkedRelay[m.RelayServer.TLD],
				OwnedPorts:      map[int]string{},
			}
			if !mi.HasAPIKey {
				if sc, ok := config.ServerFor(m.RelayServer.TLD); ok {
					mi.HasAPIKey = sc.APIKey != ""
				}
			}
			// Ports held by this project's own live services are fine.
			services, _ := config.LoadServices()
			manifestAbs, _ := filepath.Abs(file)
			for _, svc := range services {
				if svc.ManifestPath == manifestAbs && processAlive(svc.PID) {
					mi.OwnedPorts[svc.Port] = svc.Name
				}
			}
			for ei := range m.Environments {
				e := &m.Environments[ei]
				for ri := range e.Routes {
					r := &e.Routes[ri]
					if r.IsRun() {
						mi.RunRoutes = append(mi.RunRoutes, doctor.RunRoute{
							Name: e.Host + r.Path, Cwd: r.Cwd, Command: r.Command, Port: r.Port,
						})
					} else {
						mi.ProxyTargs = append(mi.ProxyTargs, r.Target)
					}
				}
			}
			doctor.CheckManifest(rep, mi)
		}
	}
	// Print.
	for _, res := range rep.Results {
		sym := "✓"
		switch res.Status {
		case doctor.Warn:
			sym = "!"
		case doctor.Fail:
			sym = "✗"
		}
		fmt.Printf(" %s  %s\n", sym, res.Msg)
		if res.Fix != "" && res.Status != doctor.OK {
			fmt.Printf("      fix: %s\n", res.Fix)
		}
	}
	fails := 0
	warns := 0
	for _, res := range rep.Results {
		switch res.Status {
		case doctor.Warn:
			warns++
		case doctor.Fail:
			fails++
		}
	}
	fmt.Printf("\n%d ok, %d warning(s), %d failure(s)\n",
		len(rep.Results)-fails-warns, warns, fails)
	if fails > 0 {
		return fmt.Errorf("doctor found %d failure(s)", fails)
	}
	return nil
}

// ── service (boot-time relay + autostart) ────────────────────────────────

// cmdServiceRelay is the boot-time entrypoint (runs as root, launched by
// launchd/systemd): it targets the real user's state directory, provisions
// certs for every registered local project, then execs the relay binary on
// port 443. The daemon supervisor restarts us if we ever exit.
func cmdServiceRelay(args []string) error {
	stateDir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--state-dir" && i+1 < len(args) {
			i++
			stateDir = args[i]
		}
	}
	if stateDir == "" {
		return fmt.Errorf("_service-relay requires --state-dir")
	}
	config.SetDir(stateDir)

	reg, err := projects.Load()
	if err != nil {
		return err
	}
	tld := primaryLocalTLDFromRegistry(reg)
	if tld == "" {
		return fmt.Errorf("no local projects registered; nothing to serve")
	}

	hosts := reg.AllLocalHostFQDNs()
	if tld == "localhost" {
		hosts = append(hosts, "localhost")
	}
	res, err := localca.Ensure(dedupe(hosts))
	if err != nil {
		return fmt.Errorf("certs: %w", err)
	}
	if !localca.Trusted(res.CAPath) { // as root this needs no password
		if err := localca.TrustCA(res.CAPath); err != nil {
			fmt.Fprintln(os.Stderr, "[service] warning: could not trust CA:", err)
		}
	}

	sc, _ := config.ServerFor(tld)
	adminUser, adminPass := sc.AdminUser, sc.AdminPass
	if adminUser == "" || adminPass == "" {
		adminUser, adminPass = localrelay.RandomToken(), localrelay.RandomToken()
		_ = config.SetServer(tld, config.ServerConfig{
			Local: true, AdminUser: adminUser, AdminPass: adminPass, APIKey: sc.APIKey,
		})
	}

	bin, err := localrelay.EnsureBinary()
	if err != nil {
		return fmt.Errorf("relay binary: %w", err)
	}

	certDir := config.CertDir()
	env := append(os.Environ(),
		"ROXEY_DOMAIN="+tld,
		"ROXEY_ADMIN_HOST=roxey."+tld,
		"ROXEY_ADMIN_USER="+adminUser,
		"ROXEY_ADMIN_PASS="+adminPass,
		"ROXEY_DB_PATH="+filepath.Join(stateDir, "local_"+strings.ReplaceAll(tld, ".", "_")+".db"),
		"PORT=443",
		"ROXEY_TLS_CERT="+filepath.Join(certDir, "leaf.crt"),
		"ROXEY_TLS_KEY="+filepath.Join(certDir, "leaf.key"),
	)
	fmt.Printf("[service] starting relay for *.%s at roxey.%s\n", tld, tld)
	return syscall.Exec(bin, []string{bin}, env)
}

// primaryLocalTLD picks the TLD the shared relay serves: "dev" when any
// project uses the default, else the first registered local TLD.
func primaryLocalTLD() string {
	reg, err := projects.Load()
	if err != nil {
		return manifest.DefaultLocalTLD
	}
	return primaryLocalTLDFromRegistry(reg)
}

func primaryLocalTLDFromRegistry(reg *projects.Registry) string {
	first := ""
	for _, n := range reg.SortedNames() {
		p := reg.Projects[n]
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

func onOff(b bool) string {
	if b {
		return "installed/running"
	}
	return "not installed"
}

func cmdService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: roxey service <install|uninstall|status>")
	}
	mgr, err := service.NewManager()
	if err != nil {
		return err
	}

	switch args[0] {
	case "install":
		reg, err := projects.Load()
		if err != nil {
			return err
		}
		if err := mgr.InstallRelay(); err != nil {
			return fmt.Errorf("install relay daemon: %w", err)
		}
		fmt.Println("[service] relay installed (starts at boot, restarts on crash)")
		for _, name := range reg.SortedNames() {
			p := reg.Projects[name]
			if p.AutoStart && !p.Preview {
				if err := mgr.InstallProjectAgent(name); err != nil {
					fmt.Fprintf(os.Stderr, "warning: autostart agent for %s: %v\n", name, err)
					continue
				}
				fmt.Printf("[service] autostart enabled for %s\n", name)
			}
		}
		return nil

	case "uninstall":
		reg, _ := projects.Load()
		for _, name := range reg.SortedNames() {
			if p := reg.Projects[name]; p.AutoStart && !p.Preview {
				_ = mgr.UninstallProjectAgent(name)
			}
		}
		if err := mgr.UninstallRelay(); err != nil {
			return err
		}
		fmt.Println("[service] relay daemon removed.")
		return nil

	case "status":
		installed := service.RelayRunning() // loaded implies installed
		_ = installed
		tld := primaryLocalTLD()
		fmt.Printf("relay daemon:  %s\n", onOff(service.RelayRunning()))
		if localrelay.HealthOK(tld) {
			fmt.Printf("relay health:  ok (roxey.%s)\n", tld)
		} else {
			fmt.Printf("relay health:  NOT responding\n")
		}
		reg, _ := projects.Load()
		var auto []string
		for _, n := range reg.SortedNames() {
			p := reg.Projects[n]
			if p.AutoStart && !p.Preview {
				auto = append(auto, n)
			}
		}
		if len(auto) == 0 {
			fmt.Println("autostart:     none (enable with `roxey projects --autostart <name>`)")
		} else {
			fmt.Printf("autostart:     %s\n", strings.Join(auto, ", "))
		}
		return nil
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

// ── projects ─────────────────────────────────────────────────────────────

func cmdProjects(args []string) error {
	forget, prune := "", false
	var autostartName string
	autostartOn := true
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prune":
			prune = true
		case (args[i] == "--forget" || args[i] == "-f") && i+1 < len(args):
			i++
			forget = args[i]
		case strings.HasPrefix(args[i], "--autostart="):
			v := strings.TrimPrefix(args[i], "--autostart=")
			if strings.HasPrefix(v, "off:") || v == "off" {
				return fmt.Errorf("usage: roxey projects --autostart-off <name> | --autostart=<name>")
			}
			autostartName, autostartOn = v, true
		case args[i] == "--autostart" && i+1 < len(args):
			i++
			autostartName, autostartOn = args[i], true
		case args[i] == "--autostart-off" && i+1 < len(args):
			i++
			autostartName, autostartOn = args[i], false
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	reg, err := projects.Load()
	if err != nil {
		return err
	}

	if autostartName != "" {
		p, ok := reg.Get(autostartName)
		if !ok {
			return fmt.Errorf("unknown project %q", autostartName)
		}
		if p.Preview {
			return fmt.Errorf("previews cannot be autostarted")
		}
		p.AutoStart = autostartOn
		if err := reg.Save(); err != nil {
			return err
		}
		mgr, err := service.NewManager()
		if err != nil {
			return err
		}
		if autostartOn {
			if err := mgr.InstallProjectAgent(autostartName); err != nil {
				return fmt.Errorf("install autostart agent: %w", err)
			}
			fmt.Printf("Autostart enabled for %s.\n", autostartName)
		} else {
			if err := mgr.UninstallProjectAgent(autostartName); err != nil {
				return fmt.Errorf("remove autostart agent: %w", err)
			}
			fmt.Printf("Autostart disabled for %s.\n", autostartName)
		}
		return nil
	}

	if forget != "" {
		if !reg.Forget(forget) {
			return fmt.Errorf("unknown project %q", forget)
		}
		fmt.Printf("Forgot %s.\n", forget)
		return nil
	}
	if pruned := reg.Prune(); prune && len(pruned) > 0 {
		fmt.Printf("Pruned %d project(s): %s\n", len(pruned), strings.Join(pruned, ", "))
	}

	tunnels, _ := config.LoadTunnels()
	services, _ := config.LoadServices()

	names := reg.SortedNames()
	if len(names) == 0 {
		fmt.Println("No known projects. Run `roxey up` inside a project to register it.")
		return nil
	}

	fmt.Printf("%-28s %-9s %-6s %s\n", "NAME", "STATUS", "PREVIEW", "PATH")
	for _, name := range names {
		p := reg.Projects[name]
		status, urls := projectStatus(p, tunnels, services)
		tag := ""
		if p.Preview {
			tag = p.Branch
		}
		fmt.Printf("%-28s %-9s %-6s %s\n", name, status, tag, p.Path)
		if urls > 0 {
			_ = urls
		}
	}
	return nil
}

// projectStatus derives running/stopped/partial from live processes.
func projectStatus(p *projects.Project, tunnels map[string]config.TunnelInfo, services map[string]config.ServiceInfo) (string, int) {
	live := 0
	for _, t := range tunnels {
		if projects.SameDir(filepath.Dir(t.ManifestPath), p.Path) && t.ManifestPath != "" && processAlive(t.PID) {
			live++
		}
	}
	svcLive := 0
	for _, s := range services {
		if projects.SameDir(s.ManifestPath, p.Path) && s.ManifestPath != "" && processAlive(s.PID) {
			svcLive++
		}
	}
	total := live + svcLive
	switch {
	case total > 0 && svcLive > 0 && live == 0:
		return "services", total
	case total > 0:
		return "running", total
	default:
		return "stopped", 0
	}
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
