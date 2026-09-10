// Package punch implements TCP simultaneous-open hole punching.
//
// Both peers reserve N local ports (listening with SO_REUSEPORT so the
// same port can also originate dials), exchange candidates
// (publicIP:port, STUN-derived, port-preservation assumed), and then
// simultaneously dial each other's candidates. Outbound SYNs open the
// NAT pinholes; the first connection that establishes is the punched
// session. Works on full-cone and endpoint-independent NATs; symmetric
// NATs fail — the relay remains the fallback rung.
//
// Roles: the peer with the LOWER repo PeerID dials (PunchDial), the
// other accepts (PunchAccept) while spray-dialing to keep its own NAT
// mappings open. Signaling (offer/answer exchange) rides a paired-peer
// relay — see the daemon's fam=punch handler.
package punch

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Preamble prefixes punch signaling lines on raw connections. A real LRM
// handshake starts with a raw X25519 ephemeral key (uniformly random
// bytes), so a 10-byte ASCII prefix is unambiguous in practice.
const Preamble = "LRMPUNCH1"

// FamName is the envelope family used for punch signaling relays.
const FamName = "punch"

// Offer is the punch signaling payload.
type Offer struct {
	Peer  string   `json:"peer"`           // repo PeerID hex
	Pub   string   `json:"pub"`            // repo ed25519 public key hex
	Cands []string `json:"cands"`          // host:port candidates
	WS    string   `json:"ws,omitempty"`   // workspace id hex (display)
	User  string   `json:"user,omitempty"` // display name
}

// EncodeLine renders "<PREAMBLE><json>\n".
func EncodeLine(o Offer) []byte {
	raw, _ := json.Marshal(o)
	line := make([]byte, 0, len(Preamble)+len(raw)+1)
	line = append(line, Preamble...)
	line = append(line, raw...)
	line = append(line, '\n')
	return line
}

// ParseLine decodes a punch line (preamble + json).
func ParseLine(b []byte) (Offer, error) {
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, Preamble) {
		return Offer{}, fmt.Errorf("not a punch line")
	}
	return ParsePayload([]byte(s[len(Preamble):]))
}

// ParsePayload decodes a punch offer from bare JSON.
func ParsePayload(b []byte) (Offer, error) {
	var o Offer
	if err := json.Unmarshal(b, &o); err != nil {
		return Offer{}, fmt.Errorf("bad punch payload: %w", err)
	}
	if o.Peer == "" || o.Pub == "" || len(o.Cands) == 0 {
		return Offer{}, fmt.Errorf("punch offer missing fields")
	}
	return o, nil
}

// Reserved is a set of punch ports held open by reuseport listeners.
type Reserved struct {
	listeners []net.Listener
	ports     []int
	cands     []string
}

// Reserve binds n wildcard listeners (SO_REUSEPORT) and records punch
// candidates for the given public IP. The listeners double as landing
// pads for the peer's dials and as source ports for ours.
func Reserve(n int, ip net.IP) (*Reserved, error) {
	if n <= 0 {
		n = 3
	}
	r := &Reserved{}
	for i := 0; i < n; i++ {
		// Explicit IPv4: the reuseport listen-and-dial trick must keep
		// BOTH sockets in the same address family (a dual-stack ":0"
		// listener is IPv6 — an IPv4 dial bind then collides with the
		// established conns sitting in its backlog: EADDRNOTAVAIL).
		ln, err := listenReusePort("tcp4", "0.0.0.0:0")
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("punch reserve: %w", err)
		}
		r.listeners = append(r.listeners, ln)
		port := ln.Addr().(*net.TCPAddr).Port
		r.ports = append(r.ports, port)
		if ip != nil {
			r.cands = append(r.cands, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)))
		}
	}
	return r, nil
}

// Ports returns the reserved local ports.
func (r *Reserved) Ports() []int { return r.ports }

// Cands returns the advertised candidates (empty if no public IP).
func (r *Reserved) Cands() []string { return r.cands }

// Close releases all reserved listeners.
func (r *Reserved) Close() {
	for _, ln := range r.listeners {
		_ = ln.Close()
	}
	r.listeners = nil
}

// debugf prints punch diagnostics when LRM_PUNCH_DEBUG is set.
func debugf(format string, args ...any) {
	if os.Getenv("LRM_PUNCH_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[punch] "+format+"\n", args...)
	}
}

// replayConn is a net.Conn that replays already-read bytes before
// passing reads through to the underlying conn.
type replayConn struct {
	net.Conn
	head []byte
}

func (c *replayConn) Read(p []byte) (int, error) {
	if len(c.head) > 0 {
		n := copy(p, c.head)
		c.head = c.head[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// dialOnce dials addr from local port lp with reuseport.
func (r *Reserved) dialOnce(ctx context.Context, addr string, lp int, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{
		Timeout:   timeout,
		KeepAlive: -1,
		// Same family as the reserved listeners (IPv4) + explicit
		// wildcard so the reuseport bind joins the same group.
		LocalAddr: &net.TCPAddr{IP: net.IPv4zero, Port: lp},
	}
	d.Control = reuseDialControl // SO_REUSEPORT: dial from reserved ports
	conn, err := d.DialContext(ctx, "tcp4", addr)
	if err != nil {
		debugf("dial %s from port %d: %v", addr, lp, err)
	} else {
		debugf("dial %s from port %d: CONNECTED", addr, lp)
	}
	return conn, err
}

// PunchDial (initiator role): spray-dial the peer's candidates from all
// reserved ports until one connects or the window closes.
func (r *Reserved) PunchDial(ctx context.Context, peerCands []string, window time.Duration) (net.Conn, error) {
	if len(peerCands) == 0 {
		return nil, fmt.Errorf("punch: no candidates")
	}
	// Drain-and-discard: the responder's hole-openers park conns in OUR
	// listeners' backlogs (CLOSE_WAIT zombies we never Accept), which
	// keeps the reverse 4-tuples allocated — our spray dials then fail to
	// bind with EADDRNOTAVAIL forever. Accept and close them continuously
	// so the tuples free up for the spray. Only the peer's hole-openers
	// can land here (a simultaneous open links dial sockets directly and
	// never touches a listener), so discarding is always safe.
	for _, ln := range r.listeners {
		go func(ln net.Listener) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return // listener closed (win or teardown)
				}
				debugf("drained parked conn from %s", conn.RemoteAddr())
				_ = conn.Close()
			}
		}(ln)
	}
	deadline := time.Now().Add(window)
	var won atomic.Bool
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, len(peerCands)*len(r.ports)+1)
	var wg sync.WaitGroup
	for time.Now().Before(deadline) && !won.Load() {
		for _, cand := range peerCands {
			for _, lp := range r.ports {
				if won.Load() {
					break
				}
				wg.Add(1)
				go func(cand string, lp int) {
					defer wg.Done()
					conn, err := r.dialOnce(ctx, cand, lp, 1200*time.Millisecond)
					if err != nil {
						select {
						case ch <- result{err: err}:
						default:
						}
						return
					}
					if !won.CompareAndSwap(false, true) {
						_ = conn.Close() // lost the race
						return
					}
					ch <- result{conn: conn}
				}(cand, lp)
			}
		}
		// Drain failures while waiting for a winner.
		wait := time.Until(deadline)
		if wait > 600*time.Millisecond {
			wait = 600 * time.Millisecond
		}
		timer := time.NewTimer(wait)
	waitLoop:
		for {
			select {
			case res := <-ch:
				if res.conn != nil {
					timer.Stop()
					wg.Wait()
					r.Close() // listeners done: resets parked silent conns
					return res.conn, nil
				}
			case <-timer.C:
				break waitLoop
			case <-ctx.Done():
				timer.Stop()
				wg.Wait()
				return nil, ctx.Err()
			}
		}
		timer.Stop()
	}
	wg.Wait()
	return nil, fmt.Errorf("punch: no candidate connected within %s", window)
}

// The initiator's losing parallel dials ALSO land here — only its
// winning conn ever carries bytes, the rest sit silent. So every accepted
// conn is validated CONCURRENTLY by the caller's callback (in practice:
// run the LRM responder handshake; dead conns fail instantly with EOF,
// silent losers block until closed or deadline). The first conn the
// callback accepts wins and is returned; all others are closed.
func (r *Reserved) PunchAccept(ctx context.Context, peerCands []string, window time.Duration, validate func(net.Conn) bool) (net.Conn, error) {
	ch := make(chan net.Conn, len(r.listeners)+16)
	done := make(chan struct{})
	valid := make(chan net.Conn, 4)
	var mu sync.Mutex
	var live []net.Conn // every accepted conn, so losers can be closed
	var wg sync.WaitGroup
	// Hole-openers: dial the peer's candidates from our ports to keep the
	// NAT mappings open (the mapping exists the moment the SYN leaves).
	//
	// A hole-opener can ALSO land a real session: if our dial crosses the
	// initiator's spray dial for the same socket pair, TCP simultaneous
	// open links the two DIAL sockets directly and the peer's handshake
	// bytes arrive on OUR dial conn. But when our SYN reaches the peer
	// BEFORE their spray dial exists, it completes against their LISTENER
	// instead — a useless conn that would park the 4-tuple and starve the
	// peer's spray (their SYNs to an established socket get dropped). So:
	// peek briefly for data; silent listener-conns are closed fast to free
	// the tuple for the peer's next spray round; conns carrying data are
	// simultaneous-open winners and go to the validator pool.
	for _, cand := range peerCands {
		for _, lp := range r.ports {
			go func(cand string, lp int) {
				conn, err := r.dialOnce(ctx, cand, lp, 1200*time.Millisecond)
				if err != nil {
					return // best-effort mapping; dial timeouts are fine
				}
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				var head [1]byte
				n, rerr := conn.Read(head[:])
				_ = conn.SetReadDeadline(time.Time{})
				if rerr != nil || n == 0 {
					_ = conn.Close() // silent listener-conn: free the tuple
					return
				}
				debugf("hole-opener got data from %s (simultaneous open)", cand)
				select {
				case ch <- &replayConn{Conn: conn, head: head[:n]}:
				case <-done:
					_ = conn.Close()
				}
			}(cand, lp)
		}
	}
	// Landing pads: accept continuously, feed the validator. They exit
	// when the reserved listeners close (teardown below) or on done.
	for _, ln := range r.listeners {
		go func(ln net.Listener) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return // listener closed (teardown)
				}
				select {
				case ch <- conn:
				case <-done:
					_ = conn.Close()
					return
				}
			}
		}(ln)
	}
	closeLive := func(keep net.Conn) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range live {
			if c != keep {
				_ = c.Close() // unblocks validators stuck reading them
			}
		}
	}
	teardown := func() {
		close(done)
		r.Close()   // unblocks acceptors; the Reserved set is one-shot
		go func() { // close anything still queued
			for {
				select {
				case extra := <-ch:
					if extra != nil {
						_ = extra.Close()
					}
				default:
					return
				}
			}
		}()
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		select {
		case conn := <-ch:
			if conn == nil {
				continue
			}
			debugf("accepted conn from %s", conn.RemoteAddr())
			mu.Lock()
			live = append(live, conn)
			mu.Unlock()
			wg.Add(1)
			go func(c net.Conn) { // validate in parallel: silent losers stall
				defer wg.Done()
				if validate == nil || validate(c) {
					debugf("VALIDATED OK %s <- %s", c.LocalAddr(), c.RemoteAddr())
					select {
					case valid <- c:
					case <-done:
						_ = c.Close()
					}
				} else {
					debugf("validate rejected conn from %s", c.RemoteAddr())
					_ = c.Close()
				}
			}(conn)
		case w := <-valid:
			teardown()
			closeLive(w)
			wg.Wait()
			for { // late validated conns (raced the winner) — close them too
				select {
				case c := <-valid:
					_ = c.Close()
				default:
					return w, nil
				}
			}
		case <-timer.C:
			teardown()
			closeLive(nil)
			wg.Wait()
			return nil, fmt.Errorf("punch: no incoming connection within %s", window)
		case <-ctx.Done():
			teardown()
			closeLive(nil)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
}
