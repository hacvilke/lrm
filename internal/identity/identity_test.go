package identity

import (
	"crypto/ed25519"
	"testing"
)

func TestGenerateAndSign(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.PeerID) != 32 {
		t.Fatal("bad peer id len")
	}
	msg := []byte("hello lrm")
	sig := id.Sign(msg)
	if !Verify(id.Pub, msg, sig) {
		t.Fatal("signature verify failed")
	}
	if !VerifyPeer(id.PeerID, id.Pub, msg, sig) {
		t.Fatal("peer verify failed")
	}
	// Wrong peer id must fail.
	other, _ := Generate()
	if VerifyPeer(other.PeerID, id.Pub, msg, sig) {
		t.Fatal("wrong peer id should fail")
	}
	_ = ed25519.PublicKey{}
}

func TestLoadOrCreatePersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.HexID() != b.HexID() {
		t.Fatal("identity not persisted")
	}
	if len(a.ShortID()) != 8 {
		t.Fatal("bad short id")
	}
}
