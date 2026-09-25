package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/four43labs/roxey/cli/internal/config"
	"github.com/four43labs/roxey/cli/internal/localrelay"
	"github.com/four43labs/roxey/cli/runner"
)

// RelaySupervisor owns the root relay child process: it starts the relay for
// the primary local TLD, restarts it if it dies, and tears it down on stop.
type RelaySupervisor struct {
	bin string

	mu  sync.Mutex
	tld string
	cmd *exec.Cmd
}

// NewRelaySupervisor supervises the relay binary at bin.
func NewRelaySupervisor(bin string) *RelaySupervisor {
	return &RelaySupervisor{bin: bin}
}

// Ensure starts (or restarts) the relay for tld. It is a no-op when a child
// is already running for the same tld.
func (r *RelaySupervisor) Ensure(tld string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.tld == tld {
		return nil
	}
	r.stopLocked()
	if err := r.startLocked(tld); err != nil {
		return err
	}
	go r.watch(tld, r.cmd)
	return nil
}

// Stop terminates the child (if any) and prevents restarts.
func (r *RelaySupervisor) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

// Status reports the TLD the child is serving and whether it is running.
func (r *RelaySupervisor) Status() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tld, r.cmd != nil
}

func (r *RelaySupervisor) startLocked(tld string) error {
	env, err := localrelay.RelayEnv(tld)
	if err != nil {
		return err
	}
	logPath := filepath.Join(config.LogDir(), "relay-service.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	cmd := exec.Command(r.bin)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	logf.Close() // child holds its own descriptor

	r.tld = tld
	r.cmd = cmd
	return nil
}

func (r *RelaySupervisor) stopLocked() {
	if r.cmd == nil {
		return
	}
	cmd := r.cmd
	r.cmd = nil // clear first so watch() sees itself superseded
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// ReclaimPort443 kills any pre-existing roxey-relay process holding port 443
// that this daemon does not own (e.g. left behind by an earlier `sudo -b`
// spawn), so the supervised relay can bind.
func ReclaimPort443() {
	for _, pid := range runner.PortListeners(443) {
		if pid == os.Getpid() {
			continue
		}
		out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || !strings.Contains(string(out), "roxey-relay") {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// watch waits on a child and restarts it if it exits unexpectedly.
func (r *RelaySupervisor) watch(tld string, cmd *exec.Cmd) {
	_ = cmd.Wait()

	r.mu.Lock()
	current := r.cmd == cmd
	if current {
		r.cmd = nil
	}
	r.mu.Unlock()
	if !current {
		return // superseded or stopped
	}

	time.Sleep(2 * time.Second)
	_ = r.Ensure(tld)
}
