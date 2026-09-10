package sync

import (
	stdsync "sync"
	"testing"
	"time"

	"net"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mux"
)

// TestPingRoundKeepalive exercises the persistent-session keepalive: after a
// completed sync, the dialer pings on the same control stream, the responder
// pongs with its tip, and a diverging tip makes the responder hang up (so
// both sides re-dial and fetch).
func TestPingRoundKeepalive(t *testing.T) {
	a := mkRepo(t, "alice") // workspace testWS
	commitFile(t, a, "f.txt", "v1\n", "a1")

	b := mkRepo(t, "bob") // empty local — adopts alice's history on sync

	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false) // alice = responder
	sessB := mux.NewSession(pb, true)  // bob = dialer/initiator

	var wg stdsync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = New(a).SyncWithSession(sessA, false, "")
	}()
	engB := New(b)
	engB.KeepCtl = true // daemon-style persistent session
	go func() {
		defer wg.Done()
		_, _ = engB.SyncWithSession(sessB, true, "")
	}()
	// Wait for the sync to settle (bob adopts alice's tip).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tip, _, _ := b.HeadCommit(); tip != cas.Nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tip, _, _ := b.HeadCommit(); tip == cas.Nil {
		t.Fatal("bob did not adopt alice's history")
	}

	// Round 1: bob pings; alice pongs her tip (which bob now has).
	tip, err := engB.PingRound(2 * time.Second)
	if err != nil {
		t.Fatalf("ping round 1: %v", err)
	}
	if tip == "" {
		t.Fatal("alice should pong a tip")
	}
	if h, err := cas.ParseHex(tip); err != nil || !b.DAG.Has(h) {
		t.Fatalf("bob should already have alice's tip %s", tip)
	}

	// Round 2: bob commits something new; alice sees the unknown tip in his
	// ping, pongs, then hangs up to force a re-dial.
	commitFile(t, b, "g.txt", "v2\n", "b1")
	if _, err := engB.PingRound(2 * time.Second); err != nil {
		t.Fatalf("ping round 2: %v", err)
	}
	// The session must now be tearing down (alice closed it).
	dead := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !dead {
		if _, err := engB.PingRound(500 * time.Millisecond); err != nil {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		t.Fatal("session should be torn down after the responder saw an unknown tip")
	}

	// Round 3: a fresh session syncs bob's news back... (alice re-dials in
	// the daemon; here we just verify the engines still work end-to-end.)
	sessA.Close()
	sessB.Close()
}
