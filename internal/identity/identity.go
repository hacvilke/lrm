// Package identity manages LRM peer cryptographic identities.
//
// Every LRM node owns an Ed25519 keypair. The PeerID is defined as
// SHA-256(public_key) — a 32-byte content-addressed fingerprint used in
// vector clocks, replication logs, Port Keys, and conflict branch names.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Identity is a local peer's long-term keypair.
type Identity struct {
	Priv   ed25519.PrivateKey `json:"-"`
	Pub    ed25519.PublicKey  `json:"public_key"`
	PeerID [32]byte           `json:"peer_id"`
}

// Generate creates a fresh random identity.
func Generate() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return FromKeys(priv, pub), nil
}

// FromKeys builds an Identity from an existing keypair.
func FromKeys(priv ed25519.PrivateKey, pub ed25519.PublicKey) *Identity {
	return &Identity{Priv: priv, Pub: pub, PeerID: Fingerprint(pub)}
}

// Fingerprint returns SHA-256(pubkey).
func Fingerprint(pub ed25519.PublicKey) [32]byte {
	return sha256.Sum256(pub)
}

// ShortID returns the first 8 hex chars of the PeerID (for branch names / logs).
func (i *Identity) ShortID() string {
	return hex.EncodeToString(i.PeerID[:4])
}

// HexID returns the full hex PeerID.
func (i *Identity) HexID() string {
	return hex.EncodeToString(i.PeerID[:])
}

// HexPub returns the hex-encoded public key.
func (i *Identity) HexPub() string {
	return hex.EncodeToString(i.Pub)
}

// Sign signs msg with the private key.
func (i *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(i.Priv, msg)
}

// Verify checks a signature against a public key.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	return ed25519.Verify(pub, msg, sig)
}

// VerifyPeer binds a signature to an expected PeerID: it checks that
// SHA-256(pub) == expected AND the signature is valid. This prevents
// MITM peers from swapping keys during the Port-Key handshake.
func VerifyPeer(expectedPeerID [32]byte, pub ed25519.PublicKey, msg, sig []byte) bool {
	if Fingerprint(pub) != expectedPeerID {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// LoadOrCreate loads the identity from dir/identity.key (0600, raw 32-byte
// seed) or creates + persists a new one.
func LoadOrCreate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "identity.key")
	seed, err := os.ReadFile(path)
	if err == nil {
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("corrupt identity file %s: want %d bytes got %d", path, ed25519.SeedSize, len(seed))
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		return FromKeys(priv, pub), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	id, err := Generate()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, id.Priv.Seed(), 0o600); err != nil {
		return nil, fmt.Errorf("persist identity: %w", err)
	}
	return id, nil
}

// ParsePeerID parses a 64-char hex PeerID.
func ParsePeerID(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("peer id must be 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}
