//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// newSession detaches cmd into its own session (setsid), so its process group
// can be torn down as a unit and it outlives the terminal.
func newSession(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// signalGroup sends SIGTERM (or SIGKILL) to the process group -pgid.
func signalGroup(negPgid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(negPgid, sig)
}

// signalProcess sends SIGTERM (or SIGKILL) to one process.
func signalProcess(pid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(pid, sig)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
