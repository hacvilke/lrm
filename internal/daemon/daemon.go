// Package daemon runs the LRM background engine: file watching with
// instant auto-versioning, LAN peer discovery + secure sync, and WAN
// port mapping with hygiene cleanup on exit.
package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/natpmp"
	"github.com/lrm-project/lrm/internal/node"
	"github.com/lrm-project/lrm/internal/staging"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/stun"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
	"github.com/lrm-project/lrm/internal/upnp"
)

// Config tunes the daemon.
type Config struct {
	Port         int
	WatchEvery   time.Duration
	PeerRefresh  time.Duration
	EnableUPnP   bool
	EnableNATPCP bool
}

// DefaultConfig returns defaults.
func DefaultConfig(port int) Config {
	if port == 0 {
		port = store.DefaultPort
	}
	return Config{Port: port, WatchEvery: 2 * time.Second, PeerRefresh: 10 * time.Second, EnableUPnP: true, EnableNATPCP: true}
}

// Daemon is the running background engine.
type Daemon struct {
	repo   *store.Repo
	cfg    Config
	ln     *transport.Listener
	adv    *mdns.Advertiser
	peers  sync.Map // peerHex -> *peerConn (live sessions)
	known  sync.Map // peerHex -> mdns.Peer (discovery cache)
	dialMu sync.Map // peerHex -> struct{} (dial in progress guard)
	pins   map[string]string
	pinsMu sync.Mutex
	// dialNow triggers an immediate dial round (zero-lag propagation).
	// Capacity 1: bursts coalesce into a single round.
	dialNow chan struct{}

	mapMu         sync.Mutex // guards the mapping fields below
	upnpGW        *upnp.Gateway
	upnpMapped    bool
	natGW         net.IP
	natMappedPort uint16
	onSync        func(*lrmsync.SyncResult)
	onChange      func(int)
	onEvent       func(string)
	shutdownOnce  sync.Once
	// Device layer (pairing): machine-wide node identity + address book.
	nodeID *node.Identity
	book   *node.Book
}

type peerConn struct {
	peer   mdns.Peer
	secure *transport.SecureConn
	sess   *mux.Session
}

// OnSync sets a callback for completed syncs (may be nil).
func (d *Daemon) OnSync(fn func(*lrmsync.SyncResult)) { d.onSync = fn }

// OnChange sets a callback for auto-commits (n = files changed).
func (d *Daemon) OnChange(fn func(int)) { d.onChange = fn }

// OnEvent sets a callback for mesh events (may be nil).
func (d *Daemon) OnEvent(fn func(string)) { d.onEvent = fn }

// event emits a mesh event if a listener is attached.
func (d *Daemon) event(format string, args ...any) {
	if d.onEvent != nil {
		d.onEvent(fmt.Sprintf(format, args...))
	}
}

// New creates (but does not start) a daemon for an open repo.
func New(repo *store.Repo, cfg Config) *Daemon {
	if cfg.Port == 0 {
		cfg.Port = store.DefaultPort
	}
	if cfg.WatchEvery <= 0 {
		cfg.WatchEvery = 2 * time.Second
	}
	if cfg.PeerRefresh <= 0 {
		cfg.PeerRefresh = 10 * time.Second
	}
	d := &Daemon{repo: repo, cfg: cfg, pins: map[string]string{}, dialNow: make(chan struct{}, 1)}
	d.loadPins()
	// Device layer (best-effort: pairing features no-op on failure).
	if id, err := node.LoadOrCreateIdentity(); err == nil {
		d.nodeID = id
	}
	if book, err := node.LoadBook(); err == nil {
		d.book = book
	}
	return d
}

// NodeID returns the machine's node identity (nil if unavailable).
func (d *Daemon) NodeID() *node.Identity { return d.nodeID }

// requestDial triggers an immediate dial round (non-blocking, coalescing).
func (d *Daemon) requestDial() {
	select {
	case d.dialNow <- struct{}{}:
	default:
	}
}

// Repo exposes the repo.
func (d *Daemon) Repo() *store.Repo { return d.repo }

// Peers returns currently connected peers.
func (d *Daemon) Peers() []mdns.Peer {
	var out []mdns.Peer
	d.peers.Range(func(_, v any) bool {
		out = append(out, v.(*peerConn).peer)
		return true
	})
	return out
}

// Run starts all loops and blocks until ctx is done, then cleans up
// (stops announcements, tears down port mappings).
func (d *Daemon) Run(ctx context.Context) error {
	// 1. Secure listener.
	ln, err := transport.Listen(fmt.Sprintf(":%d", d.cfg.Port), d.repo.Identity, d.repo.Identity.Pub)
	if err != nil {
		return fmt.Errorf("listen :%d: %w", d.cfg.Port, err)
	}
	d.ln = ln
	// Actual port (if :0).
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		d.cfg.Port = addr.Port
	}
	// 2. LAN announcements (repo identity + workspace + device identity).
	nodeHex, nodePubHex := "", ""
	if d.nodeID != nil {
		nodeHex, nodePubHex = d.nodeID.HexID(), hex.EncodeToString(d.nodeID.Pub)
	}
	d.adv = mdns.StartAdvertiser(d.repo.Identity.HexID(), d.repo.Identity.HexPub(), nodeHex, nodePubHex, d.repo.Config.Workspace, d.repo.Config.User, d.cfg.Port, 2*time.Second)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); d.acceptLoop(ctx) }()
	go func() { defer wg.Done(); d.browseLoop(ctx) }()
	go func() { defer wg.Done(); d.peerLoop(ctx) }()
	go func() { defer wg.Done(); d.watchLoop(ctx) }()
	go func() { defer wg.Done(); d.refreshMappingsLoop(ctx) }()

	// 3. WAN mapping — AFTER the loops are live: NAT discovery can block
	// for ~15s on networks without an IGD (UPnP SSDP timeout + NAT-PMP
	// retransmissions), and that must never delay accepting peers.
	d.tryMapPorts()

	<-ctx.Done()
	d.Shutdown()
	wg.Wait()
	return nil
}

// Shutdown stops everything and removes router mappings.
func (d *Daemon) Shutdown() {
	d.shutdownOnce.Do(func() {
		if d.adv != nil {
			d.adv.Stop()
		}
		if d.ln != nil {
			_ = d.ln.Close()
		}
		d.peers.Range(func(_, v any) bool {
			pc := v.(*peerConn)
			_ = pc.sess.Close()
			return true
		})
		d.unmapPorts()
		_ = d.repo.Close()
	})
}

// --- accept inbound peers ---

func (d *Daemon) acceptLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sc, err := d.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		go d.handleInbound(sc)
	}
}

func (d *Daemon) handleInbound(sc *transport.SecureConn) {
	peerHex := fmt.Sprintf("%x", sc.Remote.PeerID)
	if !d.checkPin(peerHex, sc.Remote.PubKey) {
		_ = sc.Close()
		return
	}
	sess := mux.NewSession(sc, false)
	if _, exists := d.peers.Load(peerHex); exists {
		_ = sess.Close() // duplicate session; keep the existing one
		return
	}
	d.peers.Store(peerHex, &peerConn{peer: mdns.Peer{PeerHex: peerHex, SeenAt: time.Now(), Source: "inbound"}, secure: sc, sess: sess})
	defer d.peers.Delete(peerHex)
	eng := lrmsync.New(d.repo)
	d.stampNode(eng)
	res, err := eng.SyncWithSession(sess, false, "")
	_ = sess.Close()
	if err != nil {
		return
	}
	d.learnPeer(res, sc.RemoteAddr().String())
	if d.onSync != nil {
		d.onSync(res)
	}
	if res.FastForwarded || res.MergedCommit != "" {
		d.requestDial() // our tip advanced — propagate to OTHER peers now
	}
}

// --- outbound LAN peers ---

func (d *Daemon) peerLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.PeerRefresh)
	defer t.Stop()
	d.dialKnownPeers(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.dialKnownPeers(ctx) // safety net: re-sync + find returnees
		case <-d.dialNow:
			d.dialKnownPeers(ctx) // zero-lag: fresh commit, dial NOW
		}
	}
}

// browseLoop continuously feeds the known-peer cache in the background, so
// dial rounds never block on discovery.
func (d *Daemon) browseLoop(ctx context.Context) {
	self := d.repo.Identity.HexID()
	mdns.BrowseContinuous(ctx, func(p mdns.Peer) {
		if p.PeerHex == self || p.Port == 0 {
			return
		}
		d.known.Store(p.PeerHex, p)
	})
}

// knownPeerTTL drops peers not seen recently (left the network).
const knownPeerTTL = 30 * time.Second

// dialKnownPeers dials cached, unconnected peers (instant — no discovery wait).
func (d *Daemon) dialKnownPeers(ctx context.Context) {
	d.known.Range(func(key, val any) bool {
		p := val.(mdns.Peer)
		if time.Since(p.SeenAt) > knownPeerTTL {
			d.known.Delete(key)
			return true
		}
		// Workspace scoping: never dial peers that announce a different
		// workspace. Legacy peers (no ws announced) are still dialed and
		// gated later by the sync handshake.
		if p.WS != "" && d.repo.Config.Workspace != "" && p.WS != d.repo.Config.Workspace {
			return true
		}
		if _, ok := d.peers.Load(p.PeerHex); ok {
			return true // already connected
		}
		// Paired-device key pinning: if the announced device id is in the
		// address book but the announced device key does not match the
		// pinned one, refuse to dial (possible impersonation).
		if d.book != nil && p.NodeHex != "" {
			if e := d.book.Get(p.NodeHex); e != nil && p.NodePub != "" && p.NodePub != e.Pub {
				d.event("device key mismatch for %s (%s) — not dialing", p.User, shortNodeHex(p.NodeHex))
				return true
			}
		}
		if _, dialing := d.dialMu.LoadOrStore(p.PeerHex, struct{}{}); dialing {
			return true // dial already in flight
		}
		if !d.checkPin(p.PeerHex, mustHex(p.PubKey)) {
			d.dialMu.Delete(p.PeerHex)
			return true
		}
		go func() {
			defer d.dialMu.Delete(p.PeerHex)
			d.dialPeer(ctx, p)
		}()
		return true
	})
}

func (d *Daemon) dialPeer(ctx context.Context, p mdns.Peer) {
	addr := p.Addr()
	if addr == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var expect []byte
	if pub := mustHex(p.PubKey); len(pub) == 32 {
		expect = peerIDOf(pub)
	} else if pinned := d.pinnedPub(p.PeerHex); len(pinned) == 32 {
		expect = peerIDOf(pinned)
	}
	sc, err := transport.Dial(c, addr, d.repo.Identity, d.repo.Identity.Pub, expect)
	if err != nil {
		return
	}
	peerHex := fmt.Sprintf("%x", sc.Remote.PeerID)
	if _, exists := d.peers.Load(peerHex); exists {
		_ = sc.Close() // raced another session; keep the existing one
		return
	}
	d.pin(peerHex, sc.Remote.PubKey)
	sess := mux.NewSession(sc, true)
	d.peers.Store(peerHex, &peerConn{peer: p, secure: sc, sess: sess})
	defer func() { d.peers.Delete(peerHex); _ = sess.Close() }()
	eng := lrmsync.New(d.repo)
	d.stampNode(eng)
	res, err := eng.SyncWithSession(sess, true, "")
	if err != nil {
		return
	}
	d.learnPeer(res, addr)
	if d.onSync != nil {
		d.onSync(res)
	}
	if res.FastForwarded || res.MergedCommit != "" {
		d.requestDial() // our tip advanced — propagate to OTHER peers now
	}
}

// stampNode attaches the machine's device identity to an engine so hellos
// carry it (empty = feature off).
func (d *Daemon) stampNode(eng *lrmsync.Engine) {
	if d.nodeID != nil {
		eng.NodeHex = d.nodeID.HexID()
		eng.NodePub = hex.EncodeToString(d.nodeID.Pub)
	}
}

// learnPeer refreshes the address book for a paired device we just synced
// with (learning never creates trust — only paired entries are touched).
func (d *Daemon) learnPeer(res *lrmsync.SyncResult, addr string) {
	if d.book == nil || res.RemoteNode == "" || res.RemoteNodePub == "" {
		return
	}
	if e := d.book.Get(res.RemoteNode); e == nil {
		return
	}
	var addrs []string
	if addr != "" {
		addrs = []string{addr}
	}
	d.book.Touch(res.RemoteNodePub, res.RemoteUser, addrs)
	d.event("paired device %s seen at %s", peerLabel(res), addr)
}

// peerLabel renders "user (short-id)" for events.
func peerLabel(res *lrmsync.SyncResult) string {
	if res.RemoteUser != "" {
		return res.RemoteUser
	}
	if len(res.RemoteNode) >= 8 {
		return res.RemoteNode[:8]
	}
	return res.RemoteNode
}

// shortNodeHex truncates a node id for display.
func shortNodeHex(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// --- file watcher → instant auto-versioning (Figma vibe) ---

func (d *Daemon) watchLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.scanAndCommit()
		}
	}
}

func (d *Daemon) scanAndCommit() {
	// Reload the index from disk: the CLI may have committed externally
	// while the daemon runs (daemon's in-memory copy would be stale).
	if fresh, err := staging.Load(filepath.Join(d.repo.LrmDir, "index.json")); err == nil {
		d.repo.Index = fresh
	}
	res, err := d.repo.Index.Scan(d.repo.Root, d.repo.CAS)
	if err != nil || res.Clean {
		return
	}
	// If the scanned tree already matches HEAD's tree, the content is
	// versioned (e.g. external `lrm commit` raced us) — refresh index only.
	if tip, _, herr := d.repo.HeadCommit(); herr == nil && tip != cas.Nil {
		if c, gerr := d.repo.DAG.Get(tip); gerr == nil && c.Tree == cas.Hex(res.RootHash) {
			_ = d.repo.Index.UpdateFromScan(d.repo.Root, d.repo.CAS, res.RootHash)
			_ = d.repo.Index.Save()
			return
		}
	}
	// Auto-commit the delta transparently.
	names := make([]string, 0, len(res.Changes))
	for _, ch := range res.Changes {
		names = append(names, ch.Kind+":"+ch.Path)
	}
	msg := "auto: " + strings.Join(names, ", ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	h, _, err := d.repo.Commit(msg, res.RootHash)
	if err != nil {
		return
	}
	_ = d.repo.Index.UpdateFromScan(d.repo.Root, d.repo.CAS, res.RootHash)
	_ = d.repo.Index.Save()
	if d.onChange != nil {
		d.onChange(len(res.Changes))
	}
	_ = h
	// Zero-lag propagation: fresh commit → dial cached peers immediately
	// instead of waiting for the next periodic round.
	d.requestDial()
}

// --- WAN mapping (UPnP → NAT-PMP → manual fallback) ---

func (d *Daemon) tryMapPorts() {
	d.mapMu.Lock()
	defer d.mapMu.Unlock()
	internal := localIPv4()
	desc := "LRM P2P :" + d.repo.Config.User
	// Step A: UPnP.
	if d.cfg.EnableUPnP {
		if gw, err := upnp.DiscoverGateway(3 * time.Second); err == nil {
			if err := gw.AddPortMapping(d.cfg.Port, d.cfg.Port, internal, "TCP", desc, 3600); err == nil {
				d.upnpGW, d.upnpMapped = gw, true
				return
			}
		}
	}
	// Step B: NAT-PMP.
	if d.cfg.EnableNATPCP {
		if gwIP, err := natpmp.DiscoverGateway(); err == nil {
			if res, err := natpmp.MapTCP(gwIP, uint16(d.cfg.Port), uint16(d.cfg.Port), 3600); err == nil {
				d.natGW, d.natMappedPort = gwIP, res.ExternalPort
				return
			}
		}
	}
	// Step C: manual fallback (surfaced by `lrm share`).
}

func (d *Daemon) refreshMappingsLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.tryMapPorts() // renew leases
		}
	}
}

func (d *Daemon) unmapPorts() {
	d.mapMu.Lock()
	defer d.mapMu.Unlock()
	if d.upnpMapped && d.upnpGW != nil {
		_ = d.upnpGW.DeletePortMapping(d.cfg.Port, "TCP")
		d.upnpMapped = false
	}
	if d.natGW != nil && d.natMappedPort != 0 {
		_ = natpmp.UnmapTCP(d.natGW, uint16(d.cfg.Port), d.natMappedPort)
		d.natMappedPort = 0
	}
}

// PublicIP returns the WAN address (UPnP → STUN pool).
func (d *Daemon) PublicIP() net.IP {
	d.mapMu.Lock()
	defer d.mapMu.Unlock()
	if d.upnpMapped && d.upnpGW != nil {
		if ip, err := d.upnpGW.ExternalIP(); err == nil {
			return ip
		}
	}
	if ip, err := stun.DiscoverPublicIP(nil, 4*time.Second); err == nil {
		return ip
	}
	return nil
}

// ExternalPort returns the mapped WAN port (or listen port if unmapped).
func (d *Daemon) ExternalPort() uint16 {
	d.mapMu.Lock()
	defer d.mapMu.Unlock()
	if d.natMappedPort != 0 {
		return d.natMappedPort
	}
	return uint16(d.cfg.Port)
}

func localIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					return v4.String()
				}
			}
		}
	}
	return ""
}

// --- peer key pinning (TOFU) ---

func (d *Daemon) pinsPath() string { return filepath.Join(d.repo.LrmDir, "peers.json") }

func (d *Daemon) loadPins() {
	raw, err := os.ReadFile(d.pinsPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &d.pins)
}

func (d *Daemon) savePins() {
	d.pinsMu.Lock()
	raw, _ := json.MarshalIndent(d.pins, "", "  ")
	d.pinsMu.Unlock()
	_ = os.WriteFile(d.pinsPath(), raw, 0o644)
}

func (d *Daemon) pin(peerHex string, pub []byte) {
	d.pinsMu.Lock()
	d.pins[peerHex] = fmt.Sprintf("%x", pub)
	d.pinsMu.Unlock()
	d.savePins()
}

func (d *Daemon) pinnedPub(peerHex string) []byte {
	d.pinsMu.Lock()
	defer d.pinsMu.Unlock()
	return mustHex(d.pins[peerHex])
}

// checkPin enforces pinning: first sight stores (TOFU), later mismatches fail.
func (d *Daemon) checkPin(peerHex string, pub []byte) bool {
	if len(pub) != 32 {
		return true // nothing to check (human-form key); handshake sig still verified
	}
	d.pinsMu.Lock()
	pinned, ok := d.pins[peerHex]
	d.pinsMu.Unlock()
	if !ok {
		d.pin(peerHex, pub)
		return true
	}
	return pinned == fmt.Sprintf("%x", pub)
}

func mustHex(s string) []byte {
	if len(s) != 64 {
		return nil
	}
	out := make([]byte, 32)
	for i := range out {
		var v uint
		if _, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &v); err != nil {
			return nil
		}
		out[i] = byte(v)
	}
	return out
}

func peerIDOf(pub []byte) []byte {
	// SHA-256(pub) — mirrors identity.Fingerprint without import cycle risk.
	h := sha256Sum(pub)
	return h
}

func sha256Sum(b []byte) []byte {
	// tiny local sha256 to avoid extra import surface in this file
	return sha256Of(b)
}
