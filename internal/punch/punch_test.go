package punch

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestLineRoundTrip(t *testing.T) {
	o := Offer{Peer: "aa", Pub: "bb", Cands: []string{"1.2.3.4:5", "1.2.3.4:6"}}
	line := EncodeLine(o)
	if !bytes.HasPrefix(line, []byte(Preamble)) {
		t.Fatal("missing preamble")
	}
	got, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if got.Peer != "aa" || len(got.Cands) != 2 || got.Cands[1] != "1.2.3.4:6" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if _, err := ParseLine([]byte("garbage\n")); err == nil {
		t.Fatal("garbage must not parse")
	}
}

// TestPunchLoopback exercises the full simultaneous-open choreography on
// loopback: initiator sprays the responder's candidates, responder
// accepts while hole-opening — one shared TCP connection must emerge and
// carry data both ways.
func TestPunchLoopback(t *testing.T) {
	alice, err := Reserve(3, net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	carol, err := Reserve(3, net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer carol.Close()
	if len(alice.Cands()) != 3 || len(carol.Cands()) != 3 {
		t.Fatalf("want 3 candidates each, got %d/%d", len(alice.Cands()), len(carol.Cands()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	debugf("iter start: alice=%v carol=%v", alice.Ports(), carol.Ports())

	// Carol (responder): validate = a conn is real when the peer's first
	// bytes arrive (dead loser conns EOF instantly).
	type res struct {
		conn net.Conn
		err  error
	}
	carolCh := make(chan res, 1)
	var prefix []byte // bytes consumed by the validator on the winning conn
	go func() {
		conn, err := carol.PunchAccept(ctx, alice.Cands(), 6*time.Second, func(c net.Conn) bool {
			_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
			buf := make([]byte, 4)
			n, err := c.Read(buf)
			if err != nil || n == 0 {
				debugf("validator read %s <- %s: n=%d err=%v", c.LocalAddr(), c.RemoteAddr(), n, err)
				return false // dead loser conn
			}
			debugf("validator GOT DATA %s <- %s: %q", c.LocalAddr(), c.RemoteAddr(), buf[:n])
			// Stash the prefix for the caller and keep the conn.
			prefix = append(buf[:n:n], prefix...)
			return true
		})
		carolCh <- res{conn, err}
	}()
	// Alice (initiator) sprays Carol's candidates, then speaks first.
	connA, err := alice.PunchDial(ctx, carol.Cands(), 6*time.Second)
	if err != nil {
		t.Fatalf("punch dial: %v", err)
	}
	defer connA.Close()
	_ = connA.SetDeadline(time.Now().Add(3 * time.Second))
	debugf("iter winner conn: local=%s remote=%s", connA.LocalAddr(), connA.RemoteAddr())
	if _, err := connA.Write([]byte("through the hole\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	debugf("iter wrote payload on winner")
	cr := <-carolCh
	if cr.err != nil {
		t.Fatalf("punch accept: %v", cr.err)
	}
	defer cr.conn.Close()
	_ = cr.conn.SetDeadline(time.Now().Add(3 * time.Second))
	// Carol already consumed a prefix during validation.
	buf := make([]byte, 64)
	n, err := io.ReadFull(cr.conn, buf[:len("through the hole\n")-len(prefix)])
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	got := string(prefix) + string(buf[:n])
	if got != "through the hole\n" {
		t.Fatalf("payload mismatch: %q", got)
	}
	// And back the other way.
	if _, err := cr.conn.Write([]byte("ack\n")); err != nil {
		t.Fatalf("write back: %v", err)
	}
	n, err = connA.Read(buf)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(buf[:n]) != "ack\n" {
		t.Fatalf("ack mismatch: %q", buf[:n])
	}
}
