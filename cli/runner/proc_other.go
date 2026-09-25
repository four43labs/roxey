//go:build !unix

package runner

import (
	"os"
	"os/exec"
)

// Roxey itself ships for macOS and Linux; these fallbacks keep the package
// building for importers on other platforms, without process-group semantics.

func newSession(*exec.Cmd) {}

func signalGroup(negPgid int, _ bool) { signalProcess(-negPgid, true) }

func signalProcess(pid int, _ bool) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil
}
