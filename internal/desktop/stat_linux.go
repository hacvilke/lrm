//go:build linux

package desktop

import (
	"os"
	"strconv"
	"strings"
)

// statZombie reads /proc/<pid>/stat and reports whether the process is a
// zombie. A killed daemon that has not been reaped still answers signal 0, so
// without this the panel would claim a dead daemon is running.
func statZombie(pid int) (zombie bool, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false, false
	}
	// The comm field can contain spaces and parens; state is the field after
	// the last ')'. Saves parsing the whole line.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return false, false
	}
	state := string(b[i+2])
	return state == "Z" || state == "X", true
}
