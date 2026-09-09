// Command lrm is the Log Replication Manager: hyper-fast, real-time,
// peer-to-peer version control with zero central servers.
package main

import (
	"os"

	"github.com/lrm-project/lrm/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
