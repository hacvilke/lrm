package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestInviteRoundTrip(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	inv := Invite(id)
	if !strings.HasPrefix(inv, "lrmpair1_") {
		t.Fatalf("invite missing prefix: %s", inv)
	}
	pub, err := VerifyInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	if string(pub) != string(id.Pub) {
		t.Fatal("invite pubkey mismatch")
	}
}

func TestInviteTamperDetected(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	inv := Invite(id)
	// Flip a character in the payload.
	tampered := inv[:len(inv)-4] + "AAAA"
	if _, err := VerifyInvite(tampered); err == nil {
		t.Fatal("tampered invite must not verify")
	}
	if _, err := VerifyInvite("nonsense"); err == nil {
		t.Fatal("garbage invite must not verify")
	}
	if _, err := VerifyInvite("lrmpair1_short"); err == nil {
		t.Fatal("truncated invite must not verify")
	}
}

func TestInviteCannotBeMintedForOthers(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	a, _ := LoadOrCreateIdentity()
	// b is a DIFFERENT machine with its own keypair; b's invite must
	// prove b's key, never a's.
	bpub, bpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := &Identity{Priv: bpriv, Pub: bpub}
	pub, err := VerifyInvite(Invite(b))
	if err != nil {
		t.Fatal(err)
	}
	if string(pub) != string(b.Pub) {
		t.Fatal("invite did not verify to its own key")
	}
	if string(pub) == string(a.Pub) {
		t.Fatal("invite verified against the wrong key")
	}
}

func TestBookPairPersistUnpair(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	book, err := LoadBook()
	if err != nil {
		t.Fatal(err)
	}
	if book.Count() != 0 {
		t.Fatal("fresh book must be empty")
	}
	peerID, err := book.Pair(id.Pub, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	// Reload: must be persisted.
	book2, err := LoadBook()
	if err != nil {
		t.Fatal(err)
	}
	if book2.Count() != 1 {
		t.Fatalf("want 1 paired device, got %d", book2.Count())
	}
	e := book2.Get(peerID)
	if e == nil || e.User != "laptop" || e.Pub != hexEncode(id.Pub) {
		t.Fatalf("bad entry: %+v", e)
	}
	if e.PeerID() != peerID {
		t.Fatalf("entry PeerID %q ≠ book key %q", e.PeerID(), peerID)
	}

	// Unpair by short prefix.
	if err := book2.Unpair(peerID[:8]); err != nil {
		t.Fatal(err)
	}
	if book2.Count() != 0 {
		t.Fatal("unpair left entries behind")
	}
	if err := book2.Unpair(peerID[:8]); err == nil {
		t.Fatal("unpair of unknown device must fail")
	}
}

func TestTouchLearningNeverCreatesTrust(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	book, _ := LoadBook()
	// Touch an unpaired device: must be ignored.
	book.Touch(hexEncode(id.Pub), "stranger", []string{"10.0.0.9:8443"})
	if book.Count() != 0 {
		t.Fatal("learning created trust for an unpaired device")
	}
	// Pair, then touch: fields update.
	peerID, err := book.Pair(id.Pub, "")
	if err != nil {
		t.Fatal(err)
	}
	book.Touch(hexEncode(id.Pub), "laptop", []string{"10.0.0.9:8443"})
	e := book.Get(peerID)
	if e == nil || e.User != "laptop" || len(e.Addrs) != 1 || e.Addrs[0] != "10.0.0.9:8443" {
		t.Fatalf("touch did not learn: %+v", e)
	}
	if e.LastSeen.IsZero() || e.PairedAt.IsZero() {
		t.Fatal("touch must set last_seen; pair must set paired_at")
	}
	// Repeated touch must not duplicate addresses.
	book.Touch(hexEncode(id.Pub), "laptop", []string{"10.0.0.9:8443"})
	if n := len(book.Get(peerID).Addrs); n != 1 {
		t.Fatalf("duplicate address learned: %v", book.Get(peerID).Addrs)
	}
	// Garbage pubkey must not panic or corrupt.
	book.Touch("zz-not-hex", "", nil)
	book.Touch("", "", nil)
}

func TestIdentityPersists(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	a, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if a.HexID() != b.HexID() {
		t.Fatal("node identity must be stable across loads")
	}
	if a.ShortID() != a.HexID()[:8] {
		t.Fatal("ShortID must be the first 8 hex chars")
	}
}

func TestListSorted(t *testing.T) {
	t.Setenv("LRM_HOME", t.TempDir())
	book, _ := LoadBook()
	// Two dummy pubkeys.
	for i := 0; i < 2; i++ {
		pub := make([]byte, 32)
		pub[0] = byte(i + 1)
		if _, err := book.Pair(pub, ""); err != nil {
			t.Fatal(err)
		}
	}
	got := book.List()
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].PeerID() > got[1].PeerID() {
		t.Fatal("List must be sorted")
	}
}

func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}
