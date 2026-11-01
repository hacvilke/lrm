//go:build windows

package desktop

import (
	"os"
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008 // DETACHED_PROCESS

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess, HideWindow: true}
}

// terminate ends the daemon. Windows has no SIGTERM; Kill is the only portable
// option without pulling in a Win32 handle dance.
func terminate(p *os.Process) error { return p.Kill() }

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
