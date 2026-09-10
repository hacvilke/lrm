package mux

import (
	"net"
	"testing"
	"time"
)

// TestSessionCloseWakesReaders verifies that closing a session unblocks
// goroutines blocked in Stream.Read — the property the daemon's keepalive
// relies on to avoid leaking a reader per dead session.
func TestSessionCloseWakesReaders(t *testing.T) {
	a, b := net.Pipe()
	sa := NewSession(a, true)  // dialer
	sb := NewSession(b, false) // responder
	defer sb.Close()

	st, err := sa.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := st.Read(buf)
		done <- err
	}()

	time.Sleep(100 * time.Millisecond) // let the Read block
	sa.Close()                         // session dies underneath it

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read must return an error after session close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read stayed blocked after session close — reader leak")
	}
}

// TestSessionCloseWakesAccept verifies AcceptStream also unblocks.
func TestSessionCloseWakesAccept(t *testing.T) {
	a, b := net.Pipe()
	sa := NewSession(a, true)
	_ = NewSession(b, false)
	defer sa.Close()

	done := make(chan error, 1)
	go func() {
		_, err := sa.AcceptStream()
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	sa.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("AcceptStream must return an error after session close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcceptStream stayed blocked after session close")
	}
}
