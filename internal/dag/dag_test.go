package dag

import (
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/vectorclock"
)

func TestPutGetWalk(t *testing.T) {
	casStore, _ := cas.New(t.TempDir())
	d := New(casStore)
	c1 := &Commit{Version: 1, Tree: "abc", Author: "a", PeerHex: "alice", Timestamp: 1, Message: "one", Clock: vectorclock.Clock{"alice": 1}}
	h1, err := d.Put(c1)
	if err != nil {
		t.Fatal(err)
	}
	c2 := &Commit{Version: 1, Tree: "def", Parents: []string{cas.Hex(h1)}, Author: "a", PeerHex: "alice", Timestamp: 2, Message: "two", Clock: vectorclock.Clock{"alice": 2}}
	h2, err := d.Put(c2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Get(h2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message != "two" || len(got.Parents) != 1 {
		t.Fatal("get mismatch")
	}
	hashes, _, err := d.WalkTipOrder(h2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 {
		t.Fatalf("walk len=%d", len(hashes))
	}
}
