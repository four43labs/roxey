// Command roxey is the local CLI: authenticate against a Roxey relay, then
// start/stop background tunnels from a local port to <service>.<domain>.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"roxey/internal/config"
	"roxey/internal/tunnel"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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
	case "_run":
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
  auth [api-key]                      Authenticate this machine with a Roxey API key
  start <service>[/path/*] <target>   Start a tunnel, e.g. roxey start myapp localhost:3000
  stop <service>[/path]               Stop a running tunnel
  list                                List tunnels running on this machine`)
}

func cmdAuth(args []string) error {
	domain := env("ROXEY_DOMAIN", "roxey.run")
	relayHost := env("ROXEY_RELAY_HOST", "relay."+domain)

	apiKey := ""
	if len(args) > 0 {
		apiKey = args[0]
	} else {
		fmt.Print("Paste your Roxey API key: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		apiKey = strings.TrimSpace(line)
	}
	if apiKey == "" {
		return fmt.Errorf("no API key provided")
	}

	if err := config.Save(&config.Config{APIKey: apiKey, RelayHost: relayHost, Domain: domain}); err != nil {
		return err
	}
	fmt.Printf("Saved. Relay host: %s\n", relayHost)
	return nil
}

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

func cmdStart(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roxey start <service-name>[/path/*] <localhost:port>")
	}
	spec, target := args[0], args[1]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("not authenticated, run `roxey auth` first")
	}

	service, pathPrefix := parseServiceSpec(spec)
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

	proc := exec.Command(self, "_run", service, pathPrefix, target)
	proc.Stdout = logFile
	proc.Stderr = logFile
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from this terminal/session
	if err := proc.Start(); err != nil {
		return err
	}
	pid := proc.Process.Pid
	_ = proc.Process.Release()

	tunnels[key] = config.TunnelInfo{
		PID: pid, Service: service, PathPrefix: pathPrefix, Target: target,
		StartedAt: time.Now().UTC().Format(time.RFC3339), LogFile: logPath,
	}
	if err := config.SaveTunnels(tunnels); err != nil {
		return err
	}

	fmt.Printf("Tunnel started: https://%s.%s%s -> %s (pid %d)\n", service, cfg.Domain, pathPrefix, target, pid)
	fmt.Printf("Logs: %s\n", logPath)
	return nil
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
		if proc, err := os.FindProcess(info.PID); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		delete(tunnels, k)
		fmt.Printf("Stopped %s (pid %d)\n", k, info.PID)
	}
	return config.SaveTunnels(tunnels)
}

func cmdList() error {
	tunnels, err := config.LoadTunnels()
	if err != nil {
		return err
	}
	if len(tunnels) == 0 {
		fmt.Println("No active tunnels.")
		return nil
	}
	for key, info := range tunnels {
		status := "running"
		if !processAlive(info.PID) {
			status = "dead"
		}
		fmt.Printf("%s\tpid %d\t%s\t-> %s\tsince %s\n", key, info.PID, status, info.Target, info.StartedAt)
	}
	return nil
}

func cmdRun(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: roxey _run <service> <pathPrefix> <target>")
	}
	return tunnel.Run(args[0], args[1], args[2])
}
