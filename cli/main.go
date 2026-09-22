// Command roxey is the local CLI: authenticate against Roxey relays, start
// ad-hoc tunnels, or bring up a whole project topology from a roxey.yaml
// manifest (proxy routes, spawned services, and — in local mode — the
// relay itself).
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"roxey/internal/config"
	"roxey/internal/controlplane"
	"roxey/internal/doctor"
	"roxey/internal/helper"
	"roxey/internal/localca"
	"roxey/internal/localrelay"
	"roxey/internal/manifest"
	"roxey/internal/preview"
	"roxey/internal/projects"
	"roxey/internal/relayapi"
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
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "_service-relay": // internal: legacy boot-time relay entrypoint (runs as root)
		err = cmdServiceRelay(os.Args[2:])
	case "_daemon": // internal: privileged helper daemon (runs as root)
		err = cmdDaemon(os.Args[2:])
	case "_install-daemon": // internal: install the helper daemon (runs via sudo)
		err = cmdInstallDaemon(os.Args[2:])
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
  up [-d] [--preview[=slug]] [--online] [--protect <secret>] [file]
                                      Bring up a manifest's environments
  down [file]                         Tear down a manifest's tunnels + services
  projects [--forget n] [--prune]     List known projects and their status
  service install|uninstall|status    Manage the boot-time privileged helper
  setup                               One-time: install the helper (single sudo),
                                      then local mode never prompts again
  logs <name>                         Tail a service started by ` + "`up -d`" + `
  doctor [roxey.yaml]                 Diagnose state, relays, certs, and ports
  start <service>[/path/*] <target>   Ad-hoc tunnel, e.g. roxey start myapp localhost:3000
                                      add --protect <secret> to require a password
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

// targetPort extracts the port from a proxy target like "localhost:3001" or
// "http://127.0.0.1:3001". ok is false when the target carries no explicit
// port. Used to publish proxy routes to a control plane.
func targetPort(target string) (int, bool) {
	s := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	hostport := s
	if i := strings.Index(s, "/"); i >= 0 {
		hostport = s[:i]
	}
	_, rawPort, err := net.SplitHostPort(hostport)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(rawPort)
	if err != nil {
		return 0, false
	}
	return p, true
}

// startOne spawns the detached tunnel worker for one route and records it.
func startOne(tld, service, pathPrefix, target, protect, gateGroup, manifestPath string) error {
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

	runArgs := []string{"_run", tld, service, pathPrefix, target}
	if protect != "" || gateGroup != "" {
		runArgs = append(runArgs, protect)
	}
	if gateGroup != "" {
		runArgs = append(runArgs, gateGroup)
	}
	proc := exec.Command(self, runArgs...)
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
	var rest []string
	protect := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--protect" && i+1 < len(args) {
			i++
			protect = args[i]
			continue
		}
		if strings.HasPrefix(args[i], "--protect=") {
			protect = strings.TrimPrefix(args[i], "--protect=")
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: roxey start <service-name>[/path/*] <localhost:port> [--protect <secret>]")
	}
	spec, target := rest[0], rest[1]
	tld := defaultTLD()

	sc, ok := config.ServerFor(tld)
	if !ok || sc.APIKey == "" {
		return fmt.Errorf("no API key saved for %s, run `roxey auth --tld %s <key>` first", tld, tld)
	}

	service, pathPrefix := parseServiceSpec(spec)
	if err := startOne(tld, service, pathPrefix, target, protect, "", ""); err != nil {
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
		return fmt.Errorf("usage: roxey _run <tld> <service> <pathPrefix> <target> [protect] [gate-group]")
	}
	protect, gateGroup := "", ""
	if len(args) > 4 {
		protect = args[4]
	}
	if len(args) > 5 {
		gateGroup = args[5]
	}
	return tunnel.Run(args[0], args[1], args[2], args[3], protect, gateGroup)
}

// ── up / down ────────────────────────────────────────────────────────────

type runRoute struct {
	name    string // env host + path, unique within manifest
	env     string // environment host label
	protect string // shared secret gating the environment, if any
	route   *manifest.Route
}

type upOptions struct {
	detach       bool
	preview      bool
	previewSlug  string
	online       bool
	protect      string
	manifestFile string
	project      string
}

func parseUpOptions(args []string) (upOptions, error) {
	var opts upOptions
	protectSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-d" || a == "--detach":
			opts.detach = true
		case a == "--preview":
			opts.preview = true
		case strings.HasPrefix(a, "--preview="):
			opts.preview = true
			opts.previewSlug = strings.TrimPrefix(a, "--preview=")
		case a == "--online":
			opts.online = true
		case a == "--protect":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return opts, fmt.Errorf("--protect requires a secret")
			}
			i++
			protectSet = true
			opts.protect = args[i]
		case strings.HasPrefix(a, "--protect="):
			protectSet = true
			opts.protect = strings.TrimPrefix(a, "--protect=")
		case a == "--project" && i+1 < len(args):
			i++
			opts.project = args[i]
		case strings.HasPrefix(a, "--project="):
			opts.project = strings.TrimPrefix(a, "--project=")
		case opts.manifestFile == "" && !strings.HasPrefix(a, "-"):
			opts.manifestFile = a
		default:
			return opts, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if protectSet && opts.protect == "" {
		return opts, fmt.Errorf("--protect requires a non-empty secret")
	}
	return opts, nil
}

func cmdUp(args []string) error {
	opts, err := parseUpOptions(args)
	if err != nil {
		return err
	}
	file, project := opts.manifestFile, opts.project

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
	if opts.preview && opts.previewSlug != "" {
		// explicit slug only makes sense from the manifest's own directory
	} else if opts.preview && file == "" {
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

	// Control-plane mode: publish the topology to an Xhetra control plane
	// instead of tunnelling to a Roxey relay. The control plane assigns the
	// public hosts and owns authentication.
	cp := controlplane.FromEnv()
	cpMode := cp != nil

	// Preview identity: explicit flag wins; otherwise auto-detect worktrees.
	projectName := filepath.Base(filepath.Dir(absFile))
	branch := ""
	previewSlug := opts.previewSlug
	isPreview := opts.preview
	if opts.preview {
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
	if !cpMode {
		if opts.online && !isPreview && hasDottedHosts(m) {
			isPreview = true
			previewSlug = preview.PathSlug(projectName, m.Dir)
			branch = previewSlug
		}
		if isPreview {
			projectName = preview.Apply(m, projectName, previewSlug)
		}
		if opts.online {
			oldTLD, wasLocal := m.RelayServer.TLD, m.RelayServer.Local
			m.RelayServer.Local = false
			m.RelayServer.TLD = defaultTLD()
			if wasLocal || oldTLD != m.RelayServer.TLD {
				m.RelayServer.APIKey = ""
			}
		}
	} else {
		// The control plane owns the public domain and authentication; ignore
		// any relay/local settings in the manifest.
		m.RelayServer.Local = false
		m.RelayServer.APIKey = ""
		m.RelayServer.TLD = cp.Domain
	}
	if opts.protect != "" {
		for i := range m.Environments {
			m.Environments[i].Protect = opts.protect
		}
	}

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

	apiKey := ""
	displayHosts := map[string]string{}
	var runnerName string

	if cpMode {
		// Control-plane mode: the deployment assigns the public hosts and owns
		// authentication. Keep the manifest's declared ports so the sandbox
		// mirrors local development, and publish one topology per repo.
		runnerName = preview.PathSlug(projectName, absDir)
		if err := preview.AssignPorts(m, false, runner.FreePort); err != nil {
			return err
		}
		origins := make([]string, 0, len(m.Environments))
		for i := range m.Environments {
			origins = append(origins, m.Environments[i].Host)
		}
		assigned, err := cp.LookupHosts(origins)
		if err != nil {
			return fmt.Errorf("control plane host lookup: %w", err)
		}
		m.AssignedHostMap = map[string]string{}
		for i := range m.Environments {
			orig := m.Environments[i].Host
			label, ok := assigned[orig]
			if !ok || label == "" {
				return fmt.Errorf("control plane did not assign a host for %q", orig)
			}
			m.Environments[i].Host = label
			m.HostMap[orig] = label
			m.AssignedHostMap[orig] = label
		}
		// Hosted mode: `{{online}}` renders as "1".
		if err := m.ResolveEnvTemplates(true); err != nil {
			return err
		}
	} else {
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
		apiKey = m.RelayServer.APIKey
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

		if err := preview.AssignPorts(m, isPreview, runner.FreePort); err != nil {
			return err
		}

		// Hosted labels include the account suffix. Resolve them before route
		// environment templates so children receive authoritative public hosts.
		if !m.RelayServer.Local && apiKey != "" {
			requested := make([]string, 0, len(m.Environments))
			for _, e := range m.Environments {
				requested = append(requested, e.Host)
			}
			displayHosts, err = relayapi.LookupHosts(tld, apiKey, requested)
			if err != nil {
				return fmt.Errorf("lookup hosted relay names: %w", err)
			}
			m.AssignedHostMap = map[string]string{}
			for declared, effective := range m.HostMap {
				assigned, ok := displayHosts[effective]
				if !ok || assigned == "" {
					return fmt.Errorf("hosted relay did not assign a host for %q", effective)
				}
				m.AssignedHostMap[declared] = assigned
			}
		}
		if err := m.ResolveEnvTemplates(opts.online); err != nil {
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
			rr := runRoute{name: e.Host + r.Path, env: e.Host, protect: e.Protect, route: r}
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

	// Reclaim declared run-route ports still held by orphans (e.g. roxey
	// itself was SIGKILLed and never tore its setsid'd children down).
	for _, rr := range runs {
		if pids := runner.ReclaimPort(rr.route.Port); len(pids) > 0 {
			fmt.Printf("[service] reclaimed port %-6d (was held by pid %v)\n", rr.route.Port, pids)
		}
	}

	childDone := make(map[string]<-chan struct{})
	detach := opts.detach

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
	// Start every run-route, tolerating individual failures: a preview of the
	// services that did come up is far more useful than none, and the agent
	// can fix the failing one and re-run. Failed services are reported below.
	var failedServices []string
	for _, rr := range runs {
		if err := startService(rr); err != nil {
			fmt.Fprintf(os.Stderr, "[service] %s did not start: %v\n", rr.name, err)
			failedServices = append(failedServices, rr.name)
			continue
		}
	}
	if err := config.SaveServices(services); err != nil {
		return err
	}

	gateGroup := ""
	if hasProtectedEnvironment(m) {
		gateGroup, err = newGateGroup()
		if err != nil {
			return err
		}
	}

	if cpMode {
		// Publish the topology to the control plane instead of opening tunnels.
		// The control plane resolves each port to the sandbox provider's URL and
		// fronts every host behind its own authentication.
		topology := make([]controlplane.Environment, 0, len(m.Environments))
		for _, e := range m.Environments {
			routes := make([]controlplane.Route, 0, len(e.Routes))
			for _, r := range e.Routes {
				port := r.Port
				if !r.IsRun() {
					if p, ok := targetPort(r.Target); ok {
						port = p
					}
				}
				if port <= 0 {
					continue
				}
				routes = append(routes, controlplane.Route{Path: r.Path, Port: port})
			}
			if len(routes) == 0 {
				continue
			}
			topology = append(topology, controlplane.Environment{Host: e.Host, Routes: routes})
		}
		if err := cp.Publish(runnerName, topology); err != nil {
			return fmt.Errorf("publish to control plane: %w", err)
		}
	} else {
		// Open tunnels for every route.
		for _, rr := range append(append([]runRoute{}, runs...), proxyRoutes...) {
			r := rr.route
			target := r.Target
			if r.IsRun() {
				target = fmt.Sprintf("localhost:%d", r.Port)
			}
			pathPrefix := strings.TrimSuffix(r.Path, "/") // "/" (root) registers as ""
			if err := startOne(tld, rr.env, pathPrefix, target, rr.protect, gateGroup, absFile); err != nil {
				return err
			}
		}
	}

	fmt.Println()
	for _, e := range m.Environments {
		label := e.Host
		if h, ok := displayHosts[e.Host]; ok {
			label = h
		}
		fmt.Printf("%-32s", "https://"+label+"."+tld)
		var parts []string
		for _, r := range e.Routes {
			if r.IsRun() {
				parts = append(parts, r.Path+" -> "+r.Command)
			} else {
				parts = append(parts, r.Path+" -> "+r.Target)
			}
		}
		if e.Protect != "" {
			parts = append(parts, "(protected)")
		}
		fmt.Printf("  %s\n", strings.Join(parts, ", "))
	}
	fmt.Println()

	if len(failedServices) > 0 {
		fmt.Printf(
			"warning: %d service(s) did not start: %s\n",
			len(failedServices),
			strings.Join(failedServices, ", "),
		)
	}

	if detach {
		return config.SaveServices(services)
	}
	return waitForeground(childDone, absFile)
}

// waitForeground blocks until Ctrl-C or a spawned child exits, then tears
// everything belonging to this manifest down.
func waitForeground(childDone map[string]<-chan struct{}, manifestPath string) error {
	sig := make(chan os.Signal, 1)
	// SIGHUP/SIGQUIT included so closing the terminal or an abrupt stop still
	// tears down spawned services instead of orphaning them.
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

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
//
// Privileged steps go through the helper daemon when installed (no prompts);
// otherwise a first interactive run auto-installs it, with a plain sudo
// fallback.
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

	// Certificates are unprivileged (the relay reloads them on change), so
	// always provision them as the invoking user.
	res, err := localca.Ensure(hosts)
	if err != nil {
		return fmt.Errorf("local certs: %w", err)
	}

	if err := applyLocalPrivileged(tld, hosts, res.CAPath); err != nil {
		return err
	}

	sc, _ := config.ServerFor(tld)
	if sc.APIKey == "" {
		key, err := localrelay.LoginAndCreateKey(tld, "auto-created by roxey up")
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

// applyLocalPrivileged trusts the CA, syncs /etc/hosts, and ensures the relay
// is running — via the helper daemon, auto-installing it once when
// interactive, or falling back to interactive sudo.
func applyLocalPrivileged(tld string, hosts []string, caPath string) error {
	if helper.Available() {
		return applyViaHelper(tld, hosts, caPath)
	}

	if interactive() {
		fmt.Println("[setup] installing the roxey helper (one-time admin prompt; no more sudo after this)...")
		if err := installDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: helper install failed: %v\n", err)
		} else if waitHelper(15 * time.Second) {
			return applyViaHelper(tld, hosts, caPath)
		} else {
			fmt.Fprintln(os.Stderr, "warning: helper did not start")
		}
		fmt.Println("[setup] continuing with one-off sudo; run `roxey setup` to make this prompt-free")
	} else {
		fmt.Fprintln(os.Stderr, "hint: run `roxey setup` once (as this user) to make local mode prompt-free for agents")
	}

	return applyViaSudo(tld, hosts, caPath)
}

func applyViaHelper(tld string, hosts []string, caPath string) error {
	if !localca.Trusted(caPath) {
		fmt.Println("[certs] trusting local CA...")
		if err := helper.TrustCA(); err != nil {
			return err
		}
	}
	if localrelay.NeedsHosts(tld) {
		if err := helper.SyncHosts(hosts); err != nil {
			return fmt.Errorf("helper /etc/hosts sync: %w", err)
		}
	}
	// Hosts sync already starts the relay; ensure it explicitly too for
	// .localhost TLDs where no hosts entry is needed.
	if err := helper.EnsureRelay(); err != nil {
		return err
	}
	if err := waitRelay(tld, 30*time.Second); err != nil {
		return err
	}
	return nil
}

func applyViaSudo(tld string, hosts []string, caPath string) error {
	if !localca.Trusted(caPath) {
		fmt.Println("[certs] trusting local CA (may ask for your password)...")
		if err := localca.TrustCA(caPath); err != nil {
			return err
		}
	}
	if localrelay.NeedsHosts(tld) {
		if err := localrelay.SyncHosts(hosts); err != nil {
			return fmt.Errorf("/etc/hosts sync: %w", err)
		}
	}
	return localrelay.EnsureRunning(tld)
}

// syncHostsAny applies the /etc/hosts block via the daemon when available,
// else the interactive sudo path.
func syncHostsAny(hosts []string) error {
	if helper.Available() {
		return helper.SyncHosts(hosts)
	}
	return localrelay.SyncHosts(hosts)
}

// interactive reports whether stdin is a terminal (agents are not).
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// installDaemon elevates once to install the privileged helper.
func installDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	relayBin, err := localrelay.EnsureBinary()
	if err != nil {
		return fmt.Errorf("relay binary: %w", err)
	}
	cmd := exec.Command("sudo", "-p", "roxey needs one-time admin access to install its helper: ",
		exe, "_install-daemon", "--state-dir", config.Dir(), "--relay-bin", relayBin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func waitHelper(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if helper.Available() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func waitRelay(tld string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if localrelay.HealthOK(tld) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("local relay did not become healthy within %s", timeout)
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

	// Control-plane mode: remove the published topology so its hosts stop
	// resolving at the deployment's edge.
	if cp := controlplane.FromEnv(); cp != nil {
		projectName := filepath.Base(filepath.Dir(absFile))
		runnerName := preview.PathSlug(projectName, m.Dir)
		if err := cp.Delete(runnerName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: control plane delete failed: %v\n", err)
		}
	}

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
		if err := syncHostsAny(reg.AllLocalHostFQDNs()); err != nil {
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
	doctor.CheckHelper(rep)

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
		fmt.Printf("  helper daemon: %s\n", onOff(service.DaemonRunning()))
		fmt.Printf("  helper socket: %s\n", onOff(helper.Available()))
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

	bin, err := localrelay.EnsureBinary()
	if err != nil {
		return fmt.Errorf("relay binary: %w", err)
	}

	relayEnv, err := localrelay.RelayEnv(tld)
	if err != nil {
		return err
	}
	env := append(os.Environ(), relayEnv...)
	fmt.Printf("[service] starting relay for *.%s at roxey.%s\n", tld, tld)
	return syscall.Exec(bin, []string{bin}, env)
}

// ── privileged helper daemon ─────────────────────────────────────────────

// cmdDaemon is the privileged helper entrypoint (runs as root under
// launchd/systemd): it owns /etc/hosts, CA trust, and the port-443 relay,
// and serves the unprivileged CLI over ~/.roxey/helper.sock.
func cmdDaemon(args []string) error {
	stateDir, relayBin := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--state-dir" && i+1 < len(args):
			i++
			stateDir = args[i]
		case strings.HasPrefix(args[i], "--state-dir="):
			stateDir = strings.TrimPrefix(args[i], "--state-dir=")
		case args[i] == "--relay-bin" && i+1 < len(args):
			i++
			relayBin = args[i]
		case strings.HasPrefix(args[i], "--relay-bin="):
			relayBin = strings.TrimPrefix(args[i], "--relay-bin=")
		}
	}
	if stateDir == "" {
		return fmt.Errorf("_daemon requires --state-dir")
	}
	config.SetDir(stateDir)
	if relayBin == "" {
		if p := service.RootBinary("roxey-relay"); fileExists(p) {
			relayBin = p
		} else if p, err := localrelay.EnsureBinary(); err == nil {
			relayBin = p
		} else {
			return fmt.Errorf("relay binary: %w", err)
		}
	}
	return helper.RunDaemon(stateDir, relayBin)
}

// cmdInstallDaemon installs the boot helper (invoked via sudo).
func cmdInstallDaemon(args []string) error {
	stateDir, relayBin := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--state-dir" && i+1 < len(args):
			i++
			stateDir = args[i]
		case strings.HasPrefix(args[i], "--state-dir="):
			stateDir = strings.TrimPrefix(args[i], "--state-dir=")
		case args[i] == "--relay-bin" && i+1 < len(args):
			i++
			relayBin = args[i]
		case strings.HasPrefix(args[i], "--relay-bin="):
			relayBin = strings.TrimPrefix(args[i], "--relay-bin=")
		}
	}
	if stateDir == "" {
		return fmt.Errorf("_install-daemon requires --state-dir")
	}
	config.SetDir(stateDir)
	mgr, err := service.NewManager()
	if err != nil {
		return err
	}
	mgr.StateDir = stateDir // honor the explicit dir even without SUDO_USER
	return mgr.InstallDaemon(relayBin)
}

// cmdSetup performs the one-time privileged setup: install the helper daemon,
// then reconcile CA trust and /etc/hosts for every registered local project.
func cmdSetup(_ []string) error {
	if !interactive() && !helper.Available() {
		return fmt.Errorf("`roxey setup` needs an interactive terminal for the one-time admin prompt")
	}
	if !helper.Available() {
		if err := installDaemon(); err != nil {
			return err
		}
		if !waitHelper(15 * time.Second) {
			return fmt.Errorf("helper daemon did not start; check `roxey service status` and its logs")
		}
	}

	reg, err := projects.Load()
	if err != nil {
		return err
	}
	hosts := reg.AllLocalHostFQDNs()
	if len(hosts) == 0 {
		fmt.Println("[setup] helper installed — no local projects registered yet")
		return nil
	}
	tld := reg.PrimaryLocalTLD()
	res, err := localca.Ensure(hosts)
	if err != nil {
		return fmt.Errorf("local certs: %w", err)
	}
	if err := applyViaHelper(tld, hosts, res.CAPath); err != nil {
		return err
	}
	fmt.Println("[setup] done — roxey local mode now runs without sudo prompts")
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
	return reg.PrimaryLocalTLD()
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
		relayBin, err := localrelay.EnsureBinary()
		if err != nil {
			return fmt.Errorf("relay binary: %w", err)
		}
		if err := mgr.InstallDaemon(relayBin); err != nil {
			return fmt.Errorf("install helper daemon: %w", err)
		}
		fmt.Println("[service] helper installed (owns relay + /etc/hosts + CA; starts at boot)")
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
		if err := mgr.UninstallDaemon(); err != nil {
			return err
		}
		fmt.Println("[service] helper daemon removed.")
		return nil

	case "status":
		tld := primaryLocalTLD()
		fmt.Printf("helper daemon: %s\n", onOff(service.DaemonRunning()))
		fmt.Printf("helper socket: %s\n", onOff(helper.Available()))
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

func hasDottedHosts(m *manifest.Manifest) bool {
	for _, e := range m.Environments {
		if strings.Contains(e.Host, ".") {
			return true
		}
	}
	return false
}

func hasProtectedEnvironment(m *manifest.Manifest) bool {
	for _, e := range m.Environments {
		if e.Protect != "" {
			return true
		}
	}
	return false
}

func newGateGroup() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate gate group: %w", err)
	}
	return "up_" + hex.EncodeToString(b), nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
