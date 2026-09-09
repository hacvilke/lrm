package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

type testID struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func (t *testID) Sign(msg []byte) []byte { return ed25519.Sign(t.priv, msg) }

func TestHandshakeAndRoundTrip(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	idA := &testID{privA, pubA}
	idB := &testID{privB, pubB}

	ln, err := Listen("127.0.0.1:0", idB, pubB)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	acceptCh := make(chan *SecureConn, 1)
	errCh := make(chan error, 1)
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		acceptCh <- sc
	}()

	expectB := sha256.Sum256(pubB)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer, err := Dial(ctx, ln.Addr().String(), idA, pubA, expectB[:])
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialer.Close()

	var responder *SecureConn
	select {
	case responder = <-acceptCh:
	case err := <-errCh:
		t.Fatalf("accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("accept timeout")
	}
	defer responder.Close()

	if string(dialer.Remote.PubKey) != string(pubB) {
		t.Fatal("dialer authenticated wrong peer")
	}
	if string(responder.Remote.PubKey) != string(pubA) {
		t.Fatal("responder authenticated wrong peer")
	}
	// Round-trip data.
	msg := []byte("lrm secure channel works")
	go func() {
		_, _ = dialer.Write(msg)
	}()
	buf := make([]byte, 1024)
	n, err := responder.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestMITMDropped(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	_, privB, _ := ed25519.GenerateKey(rand.Reader)
	pubEvil, _, _ := ed25519.GenerateKey(rand.Reader) // attacker the dialer does NOT expect
	idA := &testID{privA, pubA}

	// Responder uses privB (honest) but dialer expects evil key → must fail.
	_, privHonest, _ := ed25519.GenerateKey(rand.Reader)
	_ = privB
	_ = privHonest
	pubHonest, privHonest2, _ := ed25519.GenerateKey(rand.Reader)
	idHonest := &testID{privHonest2, pubHonest}
	ln, err := Listen("127.0.0.1:0", idHonest, pubHonest)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _, _ = ln.Accept() }()

	expectEvil := sha256.Sum256(pubEvil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Dial(ctx, ln.Addr().String(), idA, pubA, expectEvil[:])
	if err == nil {
		t.Fatal("expected MITM/peer-mismatch to be dropped")
	}
}
