package portkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
)

func TestCompactRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := Generate(pub, net.ParseIP("203.0.113.7"), 8443, nil)
	enc := k.Encode()
	dec, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Port != 8443 || dec.ExternalIP.String() != "203.0.113.7" {
		t.Fatalf("roundtrip mismatch: %+v", dec)
	}
	if string(dec.PubKey) != string(pub) {
		t.Fatal("pubkey mismatch")
	}
}

func TestHumanRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k := Generate(pub, net.ParseIP("198.51.100.9"), 9443, nil)
	h := k.Human()
	dec, err := Decode(h)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Port != 9443 {
		t.Fatal("human port mismatch")
	}
	if string(dec.PeerID) != string(k.PeerID) {
		t.Fatal("human peer mismatch")
	}
}

func TestCompactV2WorkspaceRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	ws := []byte("0123456789abcdef") // 16 bytes
	k := Generate(pub, net.ParseIP("203.0.113.7"), 8443, ws)
	enc := k.Encode()
	dec, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Version != 2 {
		t.Fatalf("want v2 key, got v%d", dec.Version)
	}
	if string(dec.Workspace) != string(ws) {
		t.Fatalf("workspace mismatch: %x ≠ %x", dec.Workspace, ws)
	}
	if dec.WorkspaceHex() != "30313233343536373839616263646566" {
		t.Fatalf("WorkspaceHex mismatch: %s", dec.WorkspaceHex())
	}
	if string(dec.PubKey) != string(pub) {
		t.Fatal("pubkey mismatch")
	}
}

func TestV1KeyStillDecodes(t *testing.T) {
	// A v1 key (no workspace): Generate with nil ws must encode as v1 and
	// decode without a workspace.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k := Generate(pub, net.ParseIP("192.0.2.4"), 9000, nil)
	dec, err := Decode(k.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if dec.Version != 1 || dec.Workspace != nil || dec.WorkspaceHex() != "" {
		t.Fatalf("want clean v1 key, got v%d ws=%x", dec.Version, dec.Workspace)
	}
}

func TestV2KeyWithWrongLengthFallsBackToV1(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k := Generate(pub, net.ParseIP("192.0.2.4"), 9000, []byte("short"))
	if k.Version != 1 {
		t.Fatalf("short ws must produce v1 key, got v%d", k.Version)
	}
}
