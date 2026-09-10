//go:build darwin || freebsd || netbsd || openbsd

package punch

import (
	"net"
	"syscall"
)

// soReusePort is SO_REUSEPORT on the BSDs / macOS.
const soReusePort = 0x200

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

// listenReusePort binds with SO_REUSEADDR+SO_REUSEPORT.
func listenReusePort(network, addr string) (net.Listener, error) {
	cfg := net.ListenConfig{Control: reuseDialControl}
	return cfg.Listen(nil, network, addr)
}
