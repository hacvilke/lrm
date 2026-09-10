// Package node manages the machine-wide LRM device layer:
//
//	~/.lrm/node.key      — this machine's Ed25519 node keypair (one PeerID
//	                       per DEVICE, shared across every repo/workspace)
//	~/.lrm/peers.json    — the address book of paired devices
//
// Pairing is a trust relationship between machines. It is independent of
// workspaces (which scope repos): a paired device is verified by key and
// remembered with its last-seen addresses, laying the groundwork for
// dial-by-device and peer relay.
//
// LRM_HOME overrides the home directory (used by tests and multi-tenant
// machines).
package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// invitePrefix tags device-pairing invite strings.
const invitePrefix = "lrmpair1_"

// inviteMsg is the signed payload of an invite (domain-separated).
var inviteMsg = []byte("lrm-pair-v1")

// Dir returns the machine-wide LRM home (~/.lrm, or $LRM_HOME).
func Dir() string {
	if d := os.Getenv("LRM_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lrm"
	}
	return filepath.Join(home, ".lrm")
}

// Identity is this machine's node keypair.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// HexID returns the hex node PeerID = SHA-256(pub).
func (i *Identity) HexID() string {
	sum := sha256.Sum256(i.Pub)
	return hex.EncodeToString(sum[:])
}

// ShortID returns the first 8 hex chars of the node PeerID.
func (i *Identity) ShortID() string {
	id := i.HexID()
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// LoadOrCreateIdentity loads (or mints) the node keypair at ~/.lrm/node.key.
func LoadOrCreateIdentity() (*Identity, error) {
	d := Dir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(d, "node.key")
	if seed, err := os.ReadFile(path); err == nil {
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("corrupt node key %s: want %d bytes got %d", path, ed25519.SeedSize, len(seed))
		}
		priv := ed25519.NewKeyFromSeed(seed)
		return &Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		return nil, err
	}
	return &Identity{Priv: priv, Pub: pub}, nil
}

// Entry is one device in the address book.
type Entry struct {
	Pub      string    `json:"pub"`                 // hex ed25519 node pubkey
	User     string    `json:"user,omitempty"`      // learned display name
	Addrs    []string  `json:"addrs,omitempty"`     // last-seen dial addresses
	LastSeen time.Time `json:"last_seen,omitempty"` // last mesh contact
	PairedAt time.Time `json:"paired_at,omitempty"` // when pairing happened
}

// PeerID returns the hex node PeerID for this entry.
func (e *Entry) PeerID() string {
	b, err := hex.DecodeString(e.Pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Book is the machine-wide address book of devices.
type Book struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry // key: hex node PeerID
}

// LoadBook reads ~/.lrm/peers.json (empty book if missing).
func LoadBook() (*Book, error) {
	path := filepath.Join(Dir(), "peers.json")
	b := &Book{path: path, entries: map[string]*Entry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &b.entries); err != nil {
		return nil, fmt.Errorf("corrupt address book %s: %w", path, err)
	}
	if b.entries == nil {
		b.entries = map[string]*Entry{}
	}
	return b, nil
}

// Save persists the book.
func (b *Book) Save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveLocked()
}

func (b *Book) saveLocked() error {
	raw, err := json.MarshalIndent(b.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(b.path, raw, 0o600)
}

// Pair records a device as paired (pub = raw ed25519 pubkey).
// Returns the device's node PeerID hex.
func (b *Book) Pair(pub []byte, user string) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("node pubkey must be %d bytes", ed25519.PublicKeySize)
	}
	sum := sha256.Sum256(pub)
	id := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[id]
	if !ok {
		e = &Entry{}
		b.entries[id] = e
	}
	e.Pub = hex.EncodeToString(pub)
	if user != "" {
		e.User = user
	}
	if e.PairedAt.IsZero() {
		e.PairedAt = time.Now()
	}
	return id, b.saveLocked()
}

// Unpair removes a device by PeerID (full hex or unique short prefix).
func (b *Book) Unpair(peerHex string) error {
	peerHex = strings.ToLower(strings.TrimSpace(peerHex))
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[peerHex]; ok {
		delete(b.entries, peerHex)
		return b.saveLocked()
	}
	// Short prefix: must match exactly one device.
	var matches []string
	for id := range b.entries {
		if strings.HasPrefix(id, peerHex) {
			matches = append(matches, id)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no paired device matches %q", peerHex)
	}
	if len(matches) > 1 {
		return fmt.Errorf("%q is ambiguous (%d devices match)", peerHex, len(matches))
	}
	delete(b.entries, matches[0])
	return b.saveLocked()
}

// Get returns a copy of the entry for a full node PeerID (nil if absent).
func (b *Book) Get(peerHex string) *Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[strings.ToLower(peerHex)]
	if !ok {
		return nil
	}
	cp := *e
	return &cp
}

// Touch refreshes the learned fields (user, addrs, last-seen) of an
// already-paired device. Unknown devices are ignored — learning never
// creates trust.
func (b *Book) Touch(pubHex, user string, addrs []string) {
	if pubHex == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return
	}
	h := sha256.Sum256(raw)
	id := hex.EncodeToString(h[:])
	e, ok := b.entries[id]
	if !ok {
		return // not paired — learning must not create trust
	}
	if user != "" {
		e.User = user
	}
	for _, a := range addrs {
		found := false
		for _, ex := range e.Addrs {
			if ex == a {
				found = true
				break
			}
		}
		if !found {
			e.Addrs = append(e.Addrs, a)
			if len(e.Addrs) > 8 {
				e.Addrs = e.Addrs[len(e.Addrs)-8:]
			}
		}
	}
	e.LastSeen = time.Now()
	_ = b.saveLocked()
}

// List returns all paired entries, sorted by user then PeerID.
func (b *Book) List() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, len(b.entries))
	for _, e := range b.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		return out[i].PeerID() < out[j].PeerID()
	})
	return out
}

// Count returns the number of paired devices.
func (b *Book) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Invite renders this machine's pairing invite:
//
//	lrmpair1_<base64url( pub(32) || sig(64) )>
//	sig = Sign(nodeKey, "lrm-pair-v1" || pub)
//
// The self-signature proves the inviter controls the node key, so an
// invite cannot be minted for someone else's device.
func Invite(id *Identity) string {
	sig := ed25519.Sign(id.Priv, append(append([]byte{}, inviteMsg...), id.Pub...))
	raw := make([]byte, 0, len(id.Pub)+len(sig))
	raw = append(raw, id.Pub...)
	raw = append(raw, sig...)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// VerifyInvite checks an invite string and returns the device pubkey it
// proves ownership of.
func VerifyInvite(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, invitePrefix) {
		return nil, fmt.Errorf("invite must start with %s", invitePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, invitePrefix))
	if err != nil {
		return nil, fmt.Errorf("decode invite: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize+ed25519.SignatureSize {
		return nil, fmt.Errorf("malformed invite (%d bytes)", len(raw))
	}
	pub := ed25519.PublicKey(raw[:ed25519.PublicKeySize])
	sig := raw[ed25519.PublicKeySize:]
	msg := append(append([]byte{}, inviteMsg...), pub...)
	if !ed25519.Verify(pub, msg, sig) {
		return nil, fmt.Errorf("invite signature check failed")
	}
	return pub, nil
}
