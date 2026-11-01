//go:build !windows

package desktop

import (
	"os"
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own process group so it survives the panel
// being closed (Ctrl-C in the terminal that launched the app sends SIGINT to
// the foreground group, which would otherwise take the daemon down with it).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the daemon to stop, then insists.
func terminate(p *os.Process) error {
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return p.Kill()
	}
	return nil
}

// alive reports whether a process exists and is not a zombie.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 succeeds only if the process exists and we may signal it.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	if st, ok := statZombie(pid); ok {
		return !st
	}
	return true
}
