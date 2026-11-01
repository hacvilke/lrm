// Command lrm-desktop is the same program as lrm with the desktop app
// pre-selected, so it can be double-clicked.
//
// It exists because `lrm desktop` requires a terminal, and the people this is
// for are on Windows double-clicking a file, not typing subcommands. It is not
// a second implementation: it calls the same cli.Run with "desktop" prepended,
// so a fix to the CLI is a fix to the app.
package main

import (
	"os"
	"strings"

	"github.com/lrm-project/lrm/internal/cli"
)

func main() {
	args := os.Args[1:]
	// Double-click (no arguments) and any leading flag ("--port 7723", which
	// is how a shortcut carries options) open the panel. A real command name
	// passes straight through to lrm: the panel runs itself to do real work
	// ("lrm -C <dir> init ..."), so this binary must stay argv-compatible
	// with lrm or the app breaks its own subprocess calls.
	global := len(args) > 0 && (args[0] == "-C" || args[0] == "--directory")
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !global) {
		args = append([]string{"desktop"}, args...)
	}
	os.Exit(cli.Run(args))
}
