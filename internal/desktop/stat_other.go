//go:build !linux && !windows

package desktop

// statZombie is a no-op away from Linux; signal 0 is the best answer available
// without /proc, and a zombie there is rare enough not to be worth a syscall
// wrapper per platform.
func statZombie(pid int) (zombie bool, ok bool) { return false, false }
