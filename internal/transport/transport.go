// Package transport provides authenticated, encrypted peer connections.
//
// Handshake (SIGMA-lite over X25519 + Ed25519 + AES-GCM):
//  1. I → R: ephemeral public key (32B)
//  2. R → I: ephemeral public key (32B) + sealed(staticPub_R || sig_R)
//  3. I → R: sealed(staticPub_I || sig_I)
//
//   - sig_R = Sign(sk_R, ePub_I || ePub_R); sig_I = Sign(sk_I, ePub_R || ePub_I)
//   - If the dialer supplies an expected PeerID (from a Port Key) and the
//     responder's key fingerprint mismatches, the connection is dropped
//     instantly (MITM protection).
//
// Post-handshake traffic is AES-GCM frames with per-direction keys and
// monotonically increasing nonces. Large transfers stream frame-by-frame
// (16 KiB plaintext per frame) — never fully buffered.
package transport

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	handshakeTimeout = 10 * time.Second
	maxFramePlain    = 16 * 1024
	maxFrameWire     = maxFramePlain + 12 + 16 + 4 + 12 // plaintext + GCM overhead + len + nonce
)

// PeerKeys holds the authenticated remote identity after handshake.
type PeerKeys struct {
	PubKey []byte   // ed25519 public key (32B)
	PeerID [32]byte // SHA-256(pubkey)
}

// SecureConn is an encrypted net.Conn.
type SecureConn struct {
	raw      net.Conn
	sendAEAD cipher.AEAD
	recvAEAD cipher.AEAD
	sendMu   sync.Mutex
	sendCtr  uint64
	recvCtr  uint64
	readBuf  []byte
	Remote   PeerKeys
	LocalIsInitiator bool
}

// RemoteAddr implements net.Conn.
func (c *SecureConn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// LocalAddr implements net.Conn.
func (c *SecureConn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// SetDeadline implements net.Conn.
func (c *SecureConn) SetDeadline(t time.Time) error { return c.raw.SetDeadline(t) }

// SetReadDeadline implements net.Conn.
func (c *SecureConn) SetReadDeadline(t time.Time) error {
	return c.raw.SetReadDeadline(t)
}

// SetWriteDeadline implements net.Conn.
func (c *SecureConn) SetWriteDeadline(t time.Time) error {
	return c.raw.SetWriteDeadline(t)
}

// Close implements net.Conn.
func (c *SecureConn) Close() error { return c.raw.Close() }

// Write encrypts p in 16KiB frames.
func (c *SecureConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFramePlain {
			n = maxFramePlain
		}
		if err := c.writeFrame(p[:n]); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (c *SecureConn) writeFrame(plain []byte) error {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], c.sendCtr)
	c.sendCtr++
	sealed := c.sendAEAD.Seal(nil, nonce, plain, nil)
	frame := make([]byte, 4+12+len(sealed))
	binary.BigEndian.PutUint32(frame[0:4], uint32(12+len(sealed)))
	copy(frame[4:16], nonce)
	copy(frame[16:], sealed)
	_, err := c.raw.Write(frame)
	return err
}

// Read decrypts the next frame(s) into p.
func (c *SecureConn) Read(p []byte) (int, error) {
	for len(c.readBuf) == 0 {
		if err := c.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *SecureConn) readFrame() error {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.raw, hdr); err != nil {
		return err
	}
	ln := binary.BigEndian.Uint32(hdr)
	if ln < 12+16 || ln > maxFrameWire {
		return fmt.Errorf("bad secure frame length %d", ln)
	}
	buf := make([]byte, ln)
	if _, err := io.ReadFull(c.raw, buf); err != nil {
		return err
	}
	nonce, sealed := buf[:12], buf[12:]
	// Enforce monotonic nonce (replay protection).
	ctr := binary.BigEndian.Uint64(nonce[4:])
	if ctr != c.recvCtr {
		return fmt.Errorf("secure frame nonce out of order (got %d want %d)", ctr, c.recvCtr)
	}
	plain, err := c.recvAEAD.Open(nil, nonce, sealed, nil)
	if err != nil {
		return fmt.Errorf("secure frame decrypt failed: %w", err)
	}
	c.recvCtr++
	c.readBuf = append(c.readBuf, plain...)
	return nil
}

// --- handshake ---

type identityIface interface {
	Sign(msg []byte) []byte
}

// edPub extracts ed25519 pub bytes from common identity types via interface.
func edPubOf(id any) []byte {
	type pubber interface{ PubBytes() []byte }
	if p, ok := id.(pubber); ok {
		return p.PubBytes()
	}
	// Reflection-free fallback: try known struct shape via type switch on
	// []byte field accessors defined in identity package adapter below.
	if g, ok := id.(interface{ GetPub() []byte }); ok {
		return g.GetPub()
	}
	return nil
}

// Dial connects to addr and performs the initiator handshake.
// expectedPeerID may be nil (TOFU mode — caller must verify out-of-band).
func Dial(ctx context.Context, addr string, sign identityIface, localPub []byte, expectedPeerID []byte) (*SecureConn, error) {
	d := net.Dialer{Timeout: handshakeTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	sc, err := initiatorHandshake(raw, sign, localPub, expectedPeerID)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return sc, nil
}

// Listener wraps a TCP listener with responder handshakes.
type Listener struct {
	ln   net.Listener
	sign identityIface
	pub  []byte
}

// Listen starts a secure listener.
func Listen(addr string, sign identityIface, localPub []byte) (*Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Listener{ln: ln, sign: sign, pub: localPub}, nil
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close implements net.Listener.
func (l *Listener) Close() error { return l.ln.Close() }

// Accept performs the responder handshake on the next connection.
func (l *Listener) Accept() (*SecureConn, error) {
	raw, err := l.ln.Accept()
	if err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		_ = raw.Close()
		return l.Accept()
	}
	sc, err := responderHandshake(raw, l.sign, l.pub)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return sc, nil
}

func genEphemeral() (*ecdh.PrivateKey, []byte, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return priv, priv.PublicKey().Bytes(), nil
}

func initiatorHandshake(raw net.Conn, sign identityIface, localPub, expectedPeerID []byte) (*SecureConn, error) {
	ePriv, ePubI, err := genEphemeral()
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(ePubI); err != nil {
		return nil, fmt.Errorf("hs write e1: %w", err)
	}
	ePubR := make([]byte, 32)
	if _, err := io.ReadFull(raw, ePubR); err != nil {
		return nil, fmt.Errorf("hs read e2: %w", err)
	}
	shared, err := ecdhShared(ePriv, ePubR)
	if err != nil {
		return nil, err
	}
	kHS := hsKey(shared, ePubI, ePubR)
	// Read sealed responder static.
	remotePub, err := readSealedStatic(raw, kHS, 0)
	if err != nil {
		return nil, fmt.Errorf("hs read sealed-r: %w", err)
	}
	if len(remotePub) != 96 {
		return nil, fmt.Errorf("hs bad responder static len %d", len(remotePub))
	}
	rPub, rSig := remotePub[:32], remotePub[32:96]
	if !verifySig(rPub, append(append([]byte{}, ePubI...), ePubR...), rSig) {
		return nil, fmt.Errorf("hs responder signature INVALID — possible MITM, dropping")
	}
	rPeerID := sha256.Sum256(rPub)
	if len(expectedPeerID) == 32 && subtle.ConstantTimeCompare(rPeerID[:], expectedPeerID) != 1 {
		return nil, fmt.Errorf("hs peer id MISMATCH (port-key) — dropping (possible MITM)")
	}
	// Send our sealed static.
	msg := append(append([]byte{}, ePubR...), ePubI...)
	sealed, err := sealStatic(kHS, 1, localPub, sign.Sign(msg))
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(sealed); err != nil {
		return nil, fmt.Errorf("hs write sealed-i: %w", err)
	}
	kInit, kResp := sessionKeys(shared, localPub, rPub)
	sendAEAD, err := aes.NewCipher(kInit)
	if err != nil {
		return nil, err
	}
	recvAEAD, err := aes.NewCipher(kResp)
	if err != nil {
		return nil, err
	}
	sendGCM, _ := cipher.NewGCM(sendAEAD)
	recvGCM, _ := cipher.NewGCM(recvAEAD)
	return &SecureConn{raw: raw, sendAEAD: sendGCM, recvAEAD: recvGCM, Remote: PeerKeys{PubKey: rPub, PeerID: rPeerID}, LocalIsInitiator: true}, nil
}

func responderHandshake(raw net.Conn, sign identityIface, localPub []byte) (*SecureConn, error) {
	ePubI := make([]byte, 32)
	if _, err := io.ReadFull(raw, ePubI); err != nil {
		return nil, fmt.Errorf("hs read e1: %w", err)
	}
	ePriv, ePubR, err := genEphemeral()
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(ePubR); err != nil {
		return nil, fmt.Errorf("hs write e2: %w", err)
	}
	shared, err := ecdhShared(ePriv, ePubI)
	if err != nil {
		return nil, err
	}
	kHS := hsKey(shared, ePubI, ePubR)
	msg := append(append([]byte{}, ePubI...), ePubR...)
	sealed, err := sealStatic(kHS, 0, localPub, sign.Sign(msg))
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(sealed); err != nil {
		return nil, fmt.Errorf("hs write sealed-r: %w", err)
	}
	remote, err := readSealedStatic(raw, kHS, 1)
	if err != nil {
		return nil, fmt.Errorf("hs read sealed-i: %w", err)
	}
	if len(remote) != 96 {
		return nil, fmt.Errorf("hs bad initiator static len %d", len(remote))
	}
	iPub, iSig := remote[:32], remote[32:96]
	if !verifySig(iPub, append(append([]byte{}, ePubR...), ePubI...), iSig) {
		return nil, fmt.Errorf("hs initiator signature INVALID — dropping")
	}
	iPeerID := sha256.Sum256(iPub)
	kInit, kResp := sessionKeys(shared, iPub, localPub)
	// Responder sends with kResp, receives with kInit.
	sendBlock, _ := aes.NewCipher(kResp)
	recvBlock, _ := aes.NewCipher(kInit)
	sendGCM, _ := cipher.NewGCM(sendBlock)
	recvGCM, _ := cipher.NewGCM(recvBlock)
	return &SecureConn{raw: raw, sendAEAD: sendGCM, recvAEAD: recvGCM, Remote: PeerKeys{PubKey: iPub, PeerID: iPeerID}}, nil
}

func ecdhShared(priv *ecdh.PrivateKey, peerPub []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("bad ephemeral pubkey: %w", err)
	}
	return priv.ECDH(pub)
}

func hsKey(shared, eI, eR []byte) []byte {
	h := sha256.New()
	h.Write([]byte("lrm-hs-v1"))
	h.Write(shared)
	h.Write(eI)
	h.Write(eR)
	return h.Sum(nil) // 32B → AES-256
}

func sessionKeys(shared, pubI, pubR []byte) (kInit, kResp []byte) {
	base := sha256.Sum256(append(append(append([]byte("lrm-sess-v1"), shared...), pubI...), pubR...))
	k1 := sha256.Sum256(append(base[:], []byte("init")...))
	k2 := sha256.Sum256(append(base[:], []byte("resp")...))
	return k1[:], k2[:]
}

func sealStatic(key []byte, ctr uint64, pub, sig []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], ctr)
	plain := append(append([]byte{}, pub...), sig...)
	sealed := gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 4+len(sealed))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(sealed)))
	copy(out[4:], sealed)
	return out, nil
}

func readSealedStatic(r io.Reader, key []byte, ctr uint64) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	ln := binary.BigEndian.Uint32(hdr)
	if ln > 4096 {
		return nil, fmt.Errorf("sealed static too large (%d)", ln)
	}
	buf := make([]byte, ln)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], ctr)
	return gcm.Open(nil, nonce, buf, nil)
}

// verifySig verifies an ed25519 signature without importing identity
// (avoids import cycle in tests).
func verifySig(pub, msg, sig []byte) bool {
	if len(pub) != 32 || len(sig) != 64 {
		return false
	}
	// Minimal ed25519 verify via stdlib — duplicated import-free:
	return ed25519Verify(pub, msg, sig)
}
