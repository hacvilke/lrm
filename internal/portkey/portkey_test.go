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
	k := Generate(pub, net.ParseIP("203.0.113.7"), 8443)
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
	k := Generate(pub, net.ParseIP("198.51.100.9"), 9443)
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
