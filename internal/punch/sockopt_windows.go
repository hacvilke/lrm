//go:build windows

package punch

import (
	"net"
	"syscall"
)

// reuseDialControl: SO_REUSEPORT is unavailable on Windows, so punching
// from reserved ports is unsupported there — the relay rung still works.
func reuseDialControl(network, address string, c syscall.RawConn) error {
	return nil // no-op: dials fall back to ephemeral ports
}

// listenReusePort binds a plain listener (no port reuse on Windows).
func listenReusePort(network, addr string) (net.Listener, error) {
	cfg := net.ListenConfig{}
	return cfg.Listen(nil, network, addr)
}
