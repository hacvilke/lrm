//go:build linux

package punch

import (
	"net"
	"syscall"
)

// soReusePort is SO_REUSEPORT on Linux: lets the same local port be both
// a listener and a dial source — the socket trick TCP hole punching
// depends on.
const soReusePort = 0xf

// reuseDialControl enables SO_REUSEADDR+SO_REUSEPORT on dial sockets so
// dials may originate from our reserved (listening) ports.
func reuseDialControl(network, address string, c syscall.RawConn) error {
	var opErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			opErr = e
			return
		}
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
	})
	if err != nil {
		return err
	}
	return opErr
}

// listenReusePort binds with SO_REUSEADDR+SO_REUSEPORT so dials may use
// the same port.
func listenReusePort(network, addr string) (net.Listener, error) {
	cfg := net.ListenConfig{Control: reuseDialControl}
	return cfg.Listen(nil, network, addr)
}
