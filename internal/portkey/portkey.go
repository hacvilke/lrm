// Package portkey implements the Secure Connection String (Port Key) system.
//
// A Port Key is a self-contained, verifiable connection string:
//
//	lrm_key: [Public_IP_or_DDNS]:[Mapped_External_Port]@[Peer_Public_ID_hex]
//	— or compact form —
//	lrm1_<base64url(version || ipLen || ip || port || ed25519_pubkey)>
//
// The embedded public key lets the dialer authenticate the host during the
// Noise-style handshake: if the peer cannot prove ownership of the matching
// private key, the connection is dropped instantly (MITM protection).
package portkey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Version is the current Port Key protocol version (keys carrying a
// workspace ID). v1 keys (no workspace) remain decodable for back-compat.
const Version uint8 = 2

// Version1 is the legacy workspace-less key version.
const Version1 uint8 = 1

// Prefix is the compact encoding prefix.
const Prefix = "lrm1_"

// LrmPortKey is the decoded connection string (per WAN spec addendum).
type LrmPortKey struct {
	Version    uint8
	ExternalIP net.IP
	Port       uint16
	PeerID     []byte // SHA-256 fingerprint of PubKey (32 bytes)
	PubKey     ed25519.PublicKey
	Workspace  []byte // 16-byte workspace ID (v2 keys only; nil = v1)
}

// Fingerprint returns SHA-256(pubkey).
func Fingerprint(pub ed25519.PublicKey) []byte {
	sum := sha256.Sum256(pub)
	return sum[:]
}

// Generate builds a key for local identity at ip:port. A non-empty ws
// (16 bytes) produces a v2 key scoped to that workspace; nil produces a
// legacy v1 key.
func Generate(pub ed25519.PublicKey, ip net.IP, port uint16, ws []byte) *LrmPortKey {
	ver := Version1
	var wsCopy []byte
	if len(ws) == 16 {
		ver = Version
		wsCopy = make([]byte, 16)
		copy(wsCopy, ws)
	}
	return &LrmPortKey{Version: ver, ExternalIP: ip, Port: port, PeerID: Fingerprint(pub), PubKey: pub, Workspace: wsCopy}
}

// WorkspaceHex returns the workspace ID as hex ("" for v1 keys).
func (k *LrmPortKey) WorkspaceHex() string {
	if len(k.Workspace) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", k.Workspace)
}

// Encode renders the compact shareable string.
func (k *LrmPortKey) Encode() string {
	ip := k.ExternalIP
	ipLen := len(ip.To4())
	if ipLen != 4 {
		ipLen = len(ip.To16())
		if ipLen != 16 {
			ipLen = 0
		}
	}
	ver := k.Version
	if len(k.Workspace) != 16 {
		ver = Version1
	}
	raw := make([]byte, 0, 1+1+16+2+32+16)
	raw = append(raw, ver)
	raw = append(raw, byte(ipLen))
	if ipLen == 4 {
		raw = append(raw, ip.To4()...)
	} else if ipLen == 16 {
		raw = append(raw, ip.To16()...)
	}
	raw = append(raw, byte(k.Port>>8), byte(k.Port))
	raw = append(raw, k.PubKey...)
	if ver == Version {
		raw = append(raw, k.Workspace...)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Human renders the long human-readable form.
func (k *LrmPortKey) Human() string {
	return fmt.Sprintf("lrm_key: [%s]:[%d]@[%x]", k.ExternalIP.String(), k.Port, k.PeerID)
}

// Addr returns host:port dial target.
func (k *LrmPortKey) Addr() string {
	return net.JoinHostPort(k.ExternalIP.String(), strconv.Itoa(int(k.Port)))
}

// Decode parses either the compact or human form.
func Decode(s string) (*LrmPortKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, Prefix) {
		return decodeCompact(s)
	}
	if strings.Contains(s, "lrm_key") || strings.Contains(s, "@") {
		return decodeHuman(s)
	}
	// Bare base64 without prefix.
	if k, err := decodeCompact(Prefix + s); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unrecognized port key format")
}

func decodeCompact(s string) (*LrmPortKey, error) {
	b64 := strings.TrimPrefix(s, Prefix)
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode port key: %w", err)
	}
	if len(raw) < 1+1+2+32 {
		return nil, fmt.Errorf("port key too short (%d bytes)", len(raw))
	}
	ver := raw[0]
	if ver != Version && ver != Version1 {
		return nil, fmt.Errorf("unsupported port key version %d", ver)
	}
	ipLen := int(raw[1])
	off := 2
	if len(raw) < off+ipLen+2+32 {
		return nil, fmt.Errorf("truncated port key")
	}
	var ip net.IP
	if ipLen == 4 || ipLen == 16 {
		ip = make(net.IP, ipLen)
		copy(ip, raw[off:off+ipLen])
	}
	off += ipLen
	port := uint16(raw[off])<<8 | uint16(raw[off+1])
	off += 2
	pub := make(ed25519.PublicKey, 32)
	copy(pub, raw[off:off+32])
	off += 32
	var ws []byte
	if ver == Version {
		if len(raw) < off+16 {
			return nil, fmt.Errorf("truncated port key (missing workspace id)")
		}
		ws = make([]byte, 16)
		copy(ws, raw[off:off+16])
	}
	return &LrmPortKey{Version: ver, ExternalIP: ip, Port: port, PeerID: Fingerprint(pub), PubKey: pub, Workspace: ws}, nil
}

// decodeHuman parses: lrm_key: [IP]:[Port]@[PeerID-hex]
// Note: human form carries only the fingerprint, NOT the full public key,
// so it cannot authenticate the handshake alone. DecodeHuman therefore
// returns a key with nil PubKey; callers must obtain the full pubkey via
// the compact form or a verified side-channel. For sprint usability we
// still allow dialing, but transport will run an unauthenticated-ephemeral
// fallback and PRINT A WARNING (TOFU) instead of strong verification.
func decodeHuman(s string) (*LrmPortKey, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "lrm_key:"))
	s = strings.TrimSpace(s)
	// Expected: [IP]:[Port]@[PeerHex] (brackets optional)
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return nil, fmt.Errorf("human port key missing '@peer'")
	}
	peerHex := strings.Trim(strings.TrimSpace(s[at+1:]), "[]")
	peerID, err := hexDecode(peerHex)
	if err != nil || len(peerID) != 32 {
		return nil, fmt.Errorf("bad peer id in port key")
	}
	hostport := strings.TrimSpace(s[:at])
	host, portStr, err := splitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Allow DDNS names: resolve at dial time; store as-is via LookupIP.
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve %s: %v", host, err)
		}
		ip = ips[0]
	}
	portN, err := strconv.Atoi(portStr)
	if err != nil || portN <= 0 || portN > 65535 {
		return nil, fmt.Errorf("bad port in port key")
	}
	return &LrmPortKey{Version: Version, ExternalIP: ip, Port: uint16(portN), PeerID: peerID}, nil
}

func splitHostPort(s string) (host, port string, err error) {
	s = strings.TrimSpace(s)
	// [IP]:[Port]
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", fmt.Errorf("bad [ip]:[port] form")
		}
		host = s[1:end]
		rest := strings.TrimSpace(s[end+1:])
		rest = strings.TrimPrefix(rest, ":")
		port = strings.Trim(strings.TrimSpace(rest), "[]")
		if host == "" || port == "" {
			return "", "", fmt.Errorf("bad [ip]:[port] form")
		}
		return host, port, nil
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", fmt.Errorf("bad host:port: %w", err)
	}
	return h, p, nil
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		var v uint
		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &v)
		if err != nil {
			return nil, err
		}
		out[i] = byte(v)
	}
	return out, nil
}
