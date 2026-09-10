// Package relay implements the paired-peer relay: an unreachable peer's
// traffic is piped through an online paired device, end-to-end encrypted
// (the relay is blind — it cannot decrypt what it forwards).
//
// Wire: the client opens a normal secure session to the relay daemon, then
// sends one envelope message on the first control stream:
//
//	{"fam":"relay","type":"connect","extra":{
//	    "target":"host:port", "node":"<client device PeerID>",
//	    "ts":"<unix>",      "sig":"<ed25519 sig by the device key>"}}
//
// sig covers "lrm-relay-v1|<target>|<ts>". The relay verifies the device
// is PAIRED (address book) and the signature is fresh, dials the target,
// replies {"fam":"relay","type":"open"}, and from then on the stream is a
// raw bidirectional pipe. The client runs its real transport handshake
// with the TARGET over the pipe — the relay never sees plaintext.
package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/identity"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/node"
	"github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
)

// Fam is the envelope family name.
const Fam = "relay"

// authDomain prefixes the signed relay request.
const authDomain = "lrm-relay-v1"

// maxSkew is how old a relay request may be (clock skew + replay window).
const maxSkew = 2 * time.Minute

// VerifyRequest authenticates a fam=relay connect message against the
// address book: the requesting device must be paired, the signature must
// match the pinned device key, and the timestamp must be fresh. Returns
// the requested target address.
func VerifyRequest(m sync.Msg, book *node.Book) (string, error) {
	if m.Fam != Fam || m.Type != "connect" {
		return "", fmt.Errorf("not a relay connect message")
	}
	return CheckAuth(m.Extra, book)
}

// CheckAuth validates the shared auth extras (target/node/ts/sig signed
// by the device key) against the address book. Used by both the relay
// and the punch-signaling handlers.
func CheckAuth(extra map[string]string, book *node.Book) (string, error) {
	target := strings.TrimSpace(extra["target"])
	if target == "" {
		return "", fmt.Errorf("request missing target")
	}
	if host, port, err := net.SplitHostPort(target); err != nil || host == "" || port == "" {
		return "", fmt.Errorf("target must be host:port")
	}
	nodeHex := extra["node"]
	tsStr := extra["ts"]
	sigHex := extra["sig"]
	if nodeHex == "" || tsStr == "" || sigHex == "" {
		return "", fmt.Errorf("request missing auth fields")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad timestamp")
	}
	if d := time.Since(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return "", fmt.Errorf("request stale")
	}
	if book == nil {
		return "", fmt.Errorf("no address book")
	}
	entry := book.Get(nodeHex)
	if entry == nil {
		return "", fmt.Errorf("device %s is not paired", shortHex(nodeHex))
	}
	pub, err := hex.DecodeString(entry.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("pinned device key corrupt")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", fmt.Errorf("bad signature encoding")
	}
	msg := authDomain + "|" + target + "|" + tsStr
	if !ed25519.Verify(pub, []byte(msg), sig) {
		return "", fmt.Errorf("signature check failed")
	}
	return target, nil
}

// punchDomain prefixes the signed punch-signaling delivery.
const punchDomain = "lrm-punch-v1"

// SignPunchDelivery stamps an offer with the relaying device's signature
// (over "<punchDomain>|<offerPeer>|<ts>") so the target can verify the
// delivery came from a device IT paired — not a stranger scanning ports.
func SignPunchDelivery(nodeKey *node.Identity, offerPeer string) (ts, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	msg := punchDomain + "|" + offerPeer + "|" + ts
	return ts, hex.EncodeToString(ed25519.Sign(nodeKey.Priv, []byte(msg)))
}

// VerifyPunchDelivery checks the relay's delivery auth against the
// target's address book: paired device, fresh timestamp, valid signature
// over the delivered offer's peer ID.
func VerifyPunchDelivery(nodeHex, offerPeer, tsStr, sigHex string, book *node.Book) error {
	if nodeHex == "" || tsStr == "" || sigHex == "" {
		return fmt.Errorf("punch delivery not authenticated")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad punch delivery timestamp")
	}
	if d := time.Since(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return fmt.Errorf("punch delivery stale")
	}
	if book == nil {
		return fmt.Errorf("no address book")
	}
	entry := book.Get(nodeHex)
	if entry == nil {
		return fmt.Errorf("device %s is not paired", shortHex(nodeHex))
	}
	pub, err := hex.DecodeString(entry.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("pinned device key corrupt")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("bad signature encoding")
	}
	msg := punchDomain + "|" + offerPeer + "|" + tsStr
	if !ed25519.Verify(pub, []byte(msg), sig) {
		return fmt.Errorf("signature check failed")
	}
	return nil
}

// Pipe relays bytes between a mux stream and the dialed target until
// either side closes. The relay never inspects the payload.
func Pipe(dst net.Conn, st *mux.Stream) error {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(dst, st)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(st, dst)
		errCh <- err
	}()
	<-errCh
	_ = dst.Close()
	_ = st.Close()
	<-errCh
	return nil
}

// Conn adapts a mux stream (plus its session) to net.Conn so the
// transport handshake can run over the relay pipe. Deadlines are
// best-effort no-ops — callers wrap handshakes with a watchdog.
type Conn struct {
	st    *mux.Stream
	sess  *mux.Session
	local string
	rem   string
}

// Read implements net.Conn.
func (c *Conn) Read(p []byte) (int, error) { return c.st.Read(p) }

// Write implements net.Conn.
func (c *Conn) Write(p []byte) (int, error) { return c.st.Write(p) }

// Close closes the stream and the relay session.
func (c *Conn) Close() error {
	_ = c.st.Close()
	return c.sess.Close()
}

type strAddr string

func (a strAddr) Network() string { return "lrm-relay" }
func (a strAddr) String() string  { return string(a) }

// LocalAddr implements net.Conn.
func (c *Conn) LocalAddr() net.Addr { return strAddr(c.local) }

// RemoteAddr implements net.Conn.
func (c *Conn) RemoteAddr() net.Addr { return strAddr(c.rem) }

// SetDeadline is a best-effort no-op (stream reads are channel-based).
func (c *Conn) SetDeadline(t time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }

// Dial opens a relay pipe to targetAddr through the daemon at relayAddr.
// The returned net.Conn speaks the raw wire to the TARGET: run a
// transport handshake over it (see Connect).
func Dial(ctx context.Context, relayAddr, targetAddr string, repoID *identity.Identity, nodeID *node.Identity) (net.Conn, error) {
	sc, err := transport.Dial(ctx, relayAddr, repoID, repoID.Pub, nil) // TOFU to the relay
	if err != nil {
		return nil, fmt.Errorf("dial relay %s: %w", relayAddr, err)
	}
	sess := mux.NewSession(sc, true)
	st, err := sess.OpenStream()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := authDomain + "|" + targetAddr + "|" + ts
	sig := ed25519.Sign(nodeID.Priv, []byte(msg))
	req := sync.Msg{
		Fam: Fam, Type: "connect",
		Extra: map[string]string{
			"target": targetAddr,
			"node":   nodeID.HexID(),
			"ts":     ts,
			"sig":    hex.EncodeToString(sig),
		},
	}
	if err := sync.WriteMsg(st, req); err != nil {
		_ = sess.Close()
		return nil, err
	}
	resp, err := sync.ReadMsg(st)
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("relay response: %w", err)
	}
	if resp.Type != "open" {
		_ = sess.Close()
		if resp.Error != "" {
			return nil, fmt.Errorf("relay refused: %s", resp.Error)
		}
		return nil, fmt.Errorf("relay refused (type %q)", resp.Type)
	}
	return &Conn{st: st, sess: sess, local: relayAddr, rem: targetAddr}, nil
}

// Connect dials a target through a paired-peer relay and runs the full
// transport handshake with the TARGET over the pipe (the relay is blind).
// expectPeerID optionally pins the target's identity (Port Key dial).
func Connect(ctx context.Context, relayAddr, targetAddr string, repoID *identity.Identity, nodeID *node.Identity, expectPeerID []byte) (*transport.SecureConn, error) {
	rc, err := Dial(ctx, relayAddr, targetAddr, repoID, nodeID)
	if err != nil {
		return nil, err
	}
	type hs struct {
		sc  *transport.SecureConn
		err error
	}
	ch := make(chan hs, 1)
	go func() {
		sc, err := transport.HandshakeOver(rc, repoID, repoID.Pub, expectPeerID)
		ch <- hs{sc, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			_ = rc.Close()
		}
		return r.sc, r.err
	case <-time.After(25 * time.Second):
		_ = rc.Close()
		return nil, fmt.Errorf("relay handshake timeout")
	case <-ctx.Done():
		_ = rc.Close()
		return nil, ctx.Err()
	}
}

func shortHex(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
