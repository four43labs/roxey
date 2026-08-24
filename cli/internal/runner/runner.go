// Package runner spawns and manages the app processes behind `roxey up`
// run-routes: detached (or piped for foreground mode), with PORT/HOST plus
// manifest environment injected, a TCP readiness poll, and process-group
// teardown on `down`/Ctrl-C.
package runner

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"roxey/internal/config"
)

// SpawnOptions describes one run-route service to start.
type SpawnOptions struct {
	Name        string
	Cwd         string
	Command     string
	Port        int
	Environment map[string]string
	LogFile     string // required for detached mode; empty = pipe output
}

// Spawn starts the service detached from the terminal (setsid) writing its
// output to opts.LogFile, and returns the PID once the port accepts TCP
// connections.
func Spawn(opts SpawnOptions) (int, error) {
	if err := os.MkdirAll(filepath.Dir(opts.LogFile), 0700); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd, err := buildCmd(opts, logFile, logFile)
	if err != nil {
		return 0, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %q: %w", opts.Command, err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap when it exits; we only track the PID

	if err := WaitPort(opts.Port, 30*time.Second); err != nil {
		KillGroup(pid)
		return pid, err
	}
	return pid, nil
}

// SpawnForeground starts the service with stdout/stderr multiplexed through
// writeFn (prefixed per line), returning once the port is ready. The returned
// channel yields when the child exits.
func SpawnForeground(opts SpawnOptions, writeFn func(line string)) (int, <-chan struct{}, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return 0, nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return 0, nil, err
	}

	cmd, err := buildCmd(opts, stdoutW, stderrW)
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return 0, nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start %q: %w", opts.Command, err)
	}
	pid := cmd.Process.Pid
	stdoutW.Close()
	stderrW.Close()

	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range []*os.File{stdoutR, stderrR} {
		wg.Add(1)
		go func(f *os.File) {
			defer wg.Done()
			defer f.Close()
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				writeFn(scanner.Text())
			}
		}(f)
	}
	go func() { wg.Wait(); close(done) }()

	if err := WaitPort(opts.Port, 30*time.Second); err != nil {
		KillGroup(pid)
		<-done
		return pid, done, err
	}
	return pid, done, nil
}

// FreePort asks the kernel for an available TCP port (preview auto-ports).
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func buildCmd(opts SpawnOptions, stdout, stderr *os.File) (*exec.Cmd, error) {
	parts, err := splitCommand(opts.Command)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = opts.Cwd
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", opts.Port),
		"HOST=127.0.0.1",
	)
	for k, v := range opts.Environment { // manifest env wins over shell env
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd, nil
}

// splitCommand parses a command line into words using shlex-style rules:
// single- and double-quoted segments keep their spaces but lose their
// quotes, so commands like
//
//	bash -c 'set -a && source .env && exec air'
//
// arrive at exec.Command as ["bash", "-c", "set -a && source .env && exec air"].
func splitCommand(s string) ([]string, error) {
	var (
		out    []string
		cur    strings.Builder
		inWord bool
		quote  byte
	)
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			inWord = true // a quote always continues or starts a word
			quote = c
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in command %q", quote, s)
	}
	flush()
	if len(out) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return out, nil
}

// WaitPort polls until addr accepts TCP connections or timeout elapses.
func WaitPort(port int, timeout time.Duration) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("port %d did not accept connections within %s", port, timeout)
}

// KillGroup SIGTERMs then SIGKILLs the whole process group of pid (spawned
// setsid, so pgid == pid).
func KillGroup(pid int) {
	if pid <= 0 {
		return
	}
	neg := -pid
	_ = syscall.Kill(neg, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(neg, syscall.SIGKILL)
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// PruneDead removes services.json entries whose processes have exited.
func PruneDead(services map[string]config.ServiceInfo) map[string]config.ServiceInfo {
	out := make(map[string]config.ServiceInfo, len(services))
	for k, info := range services {
		if alive(info.PID) {
			out[k] = info
		} else if info.LogFile != "" {
			_ = os.Truncate(info.LogFile, 0)
		}
	}
	return out
}
