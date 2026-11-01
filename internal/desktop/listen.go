package desktop

import (
	"fmt"
	"net"
)

// listen binds the panel to loopback only. It never binds 0.0.0.0: this page
// can start a daemon and mint share keys, so it must not be reachable from the
// network the daemon is meant to serve.
func listen(host string, port int) (net.Listener, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, fmt.Errorf("panel port %d: %w", port, err)
	}
	return ln, nil
}
