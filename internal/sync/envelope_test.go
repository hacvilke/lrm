package sync

import (
	"fmt"
	"net"
	"testing"

	stdsync "sync"

	"github.com/lrm-project/lrm/internal/mux"
)

// TestEnvelopeNegotiationV2 verifies two current peers negotiate protocol
// version 2 and exchange family capability lists.
func TestEnvelopeNegotiationV2(t *testing.T) {
	a := mkRepo(t, "alice")
	commitFile(t, a, "f.txt", "v1\n", "a1")
	b := mkRepo(t, "bob")

	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false)
	sessB := mux.NewSession(pb, true)
	defer sessA.Close()
	defer sessB.Close()

	engA := New(a)
	engA.AdvertiseFams = []string{"relay", "punch"} // daemon-style advert
	engB := New(b)

	var wg stdsync.WaitGroup
	var resA, resB *SyncResult
	wg.Add(2)
	go func() { defer wg.Done(); resA, _ = engA.SyncWithSession(sessA, false, "") }()
	go func() { defer wg.Done(); resB, _ = engB.SyncWithSession(sessB, true, "") }()
	wg.Wait()

	if resA == nil || resB == nil {
		t.Fatal("nil results")
	}
	if engA.SessionVer != 2 || engB.SessionVer != 2 {
		t.Fatalf("want session ver 2, got a=%d b=%d", engA.SessionVer, engB.SessionVer)
	}
	if resA.SessionVer != 2 || resB.SessionVer != 2 {
		t.Fatalf("results should carry ver 2, got a=%d b=%d", resA.SessionVer, resB.SessionVer)
	}
	if !engB.RemoteFams["sync"] || !engB.RemoteFams["relay"] || !engB.RemoteFams["punch"] {
		t.Fatalf("dialer should see a's families, got %v", engB.RemoteFams)
	}
	if !engA.RemoteFams["sync"] {
		t.Fatalf("responder should see b's sync family, got %v", engA.RemoteFams)
	}
}

// TestEnvelopeLegacyPeer verifies a v1 peer (hello without pv/fams/ws —
// the pre-envelope wire format) still syncs: negotiated version falls
// back to 1 and nothing breaks.
func TestEnvelopeLegacyPeer(t *testing.T) {
	b := mkRepo(t, "bob") // dialer (current build), empty repo

	pa, pb := net.Pipe()
	sessFake := mux.NewSession(pa, false) // fake v1 responder
	defer sessFake.Close()
	go func() {
		ctl, err := sessFake.AcceptStream()
		if err != nil {
			return
		}
		defer ctl.Close()
		m, err := ReadMsg(ctl)
		if err != nil {
			return
		}
		if m.PV != ProtoVersion {
			fmt.Printf("legacy fake: dialer offered pv=%d\n", m.PV)
		}
		// Reply with a strict v1 hello: no pv, no fams, no ws.
		legacy := `{"type":"hello","peer":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","branch":"main"}` + "\n"
		if _, err := ctl.Write([]byte(legacy)); err != nil {
			return
		}
		// Dialer repo is empty and remote heads are empty: it sends done.
		for {
			m, err := ReadMsg(ctl)
			if err != nil {
				return
			}
			if m.Type == "done" {
				return
			}
			if m.Type == "want" {
				_ = WriteMsg(ctl, Msg{Type: "have"})
			}
			if m.Type == "push-have" || m.Type == "push-tip" {
				_ = WriteMsg(ctl, Msg{Type: "push-ok"})
			}
		}
	}()

	sessB := mux.NewSession(pb, true)
	defer sessB.Close()
	engB := New(b)
	res, err := engB.SyncWithSession(sessB, true, "")
	if err != nil {
		t.Fatalf("sync with legacy peer failed: %v", err)
	}
	if engB.SessionVer != 1 {
		t.Fatalf("legacy peer must negotiate down to v1, got %d", engB.SessionVer)
	}
	if res.SessionVer != 1 {
		t.Fatalf("result should record v1, got %d", res.SessionVer)
	}
	if len(engB.RemoteFams) != 0 {
		t.Fatalf("legacy peer advertises no families, got %v", engB.RemoteFams)
	}
}
