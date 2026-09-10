// Package daemon runs the LRM background engine: file watching with
// instant auto-versioning, LAN peer discovery + secure sync, and WAN
// port mapping with hygiene cleanup on exit.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/natpmp"
	"github.com/lrm-project/lrm/internal/node"
	"github.com/lrm-project/lrm/internal/relay"
	"github.com/lrm-project/lrm/internal/staging"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/stun"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
	"github.com/lrm-project/lrm/internal/upnp"
)

// Config tunes the daemon.
type Config struct {
	Port           int
	WatchEvery     time.Duration
	PeerRefresh    time.Duration
	KeepaliveEvery time.Duration // ping cadence on live sessions
	PingTimeout    time.Duration // pong wait before declaring the peer lost
	EnableUPnP     bool
	EnableNATPCP   bool
}

// DefaultConfig returns defaults.
func DefaultConfig(port int) Config {
	if port == 0 {
		port = store.DefaultPort
	}
	return Config{Port: port, WatchEvery: 2 * time.Second, PeerRefresh: 10 * time.Second,
		KeepaliveEvery: 15 * time.Second, PingTimeout: 10 * time.Second,
		EnableUPnP: true, EnableNATPCP: true}
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
	// Control socket (live presence for the CLI) + lifecycle.
	started time.Time
	stopFn  func()
	ctrlMu  sync.Mutex
	ctrlLn  net.Listener
	// statics: explicitly configured peers (lrm daemon --peer host:port),
	// dialed every round in addition to discovered LAN peers.
	statics sync.Map // "host:port" -> mdns.Peer
}

// peerConn is one live peer session (presence entry).
type peerConn struct {
	peer   mdns.Peer
	secure *transport.SecureConn
	sess   *mux.Session
	// presence fields
	peerIDhex  string    // remote repo PeerID hex
	addr       string    // remote dial address
	remoteUser string    // learned from sync hello
	since      time.Time // session established
	lastPing   time.Time // last successful keepalive
	lastRTT    time.Duration
	state      string // "syncing" | "live"
	initiator  bool   // we drive keepalive when true
	resync     chan struct{}
}

// nudge requests an immediate keepalive/resync round (non-blocking).
func (pc *peerConn) nudge() {
	select {
	case pc.resync <- struct{}{}:
	default:
	}
}

// initiatorPeerID returns the PeerID of whichever peer OPENED the session
// (input to the glare tie-break).
func (pc *peerConn) initiatorPeerID(myHex string) string {
	if pc.initiator {
		return myHex
	}
	return pc.peerIDhex
}

// OnSync sets a callback for completed syncs (may be nil).
func (d *Daemon) OnSync(fn func(*lrmsync.SyncResult)) { d.onSync = fn }

// OnChange sets a callback for auto-commits (n = files changed).
func (d *Daemon) OnChange(fn func(int)) { d.onChange = fn }

// OnEvent sets a callback for mesh events (may be nil).
func (d *Daemon) OnEvent(fn func(string)) { d.onEvent = fn }

// SetStopFunc registers how the daemon should stop itself (used by the
// control socket's "stop" command).
func (d *Daemon) SetStopFunc(fn func()) { d.stopFn = fn }

// AddStaticPeer registers a peer to dial continuously (never expires).
func (d *Daemon) AddStaticPeer(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || portStr == "" {
		return fmt.Errorf("peer address must be host:port, got %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("bad port in %q", addr)
	}
	d.statics.Store(addr, mdns.Peer{Addrs: []string{host}, Port: port, SeenAt: time.Now(), Source: "static"})
	return nil
}

// Uptime returns how long the daemon has been running.
func (d *Daemon) Uptime() time.Duration { return time.Since(d.started) }

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

	d.started = time.Now()
	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); d.acceptLoop(ctx) }()
	go func() { defer wg.Done(); d.browseLoop(ctx) }()
	go func() { defer wg.Done(); d.peerLoop(ctx) }()
	go func() { defer wg.Done(); d.watchLoop(ctx) }()
	go func() { defer wg.Done(); d.refreshMappingsLoop(ctx) }()
	go func() { defer wg.Done(); d.controlLoop(ctx) }()

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
		d.ctrlMu.Lock()
		if d.ctrlLn != nil {
			_ = d.ctrlLn.Close()
		}
		d.ctrlLn = nil
		d.ctrlMu.Unlock()
		_ = os.Remove(d.controlPath())
		d.unmapPorts()
		_ = d.repo.Close()
	})
}

// controlPath is the unix socket the live daemon serves presence on.
func (d *Daemon) controlPath() string {
	return filepath.Join(d.repo.LrmDir, "daemon.sock")
}

// dropPeer removes a presence entry only if it still points at pc —
// a replaced session must never evict its successor.
func (d *Daemon) dropPeer(pc *peerConn) {
	if cur, ok := d.peers.Load(pc.peerIDhex); ok && cur.(*peerConn) == pc {
		d.peers.Delete(pc.peerIDhex)
	}
}

// shouldReplace is the glare tie-break: a challenger session (opened by
// newInitiatorHex) takes over only when its initiator's PeerID is strictly
// lower than the existing session's. Both peers evaluate the identical
// comparison, so simultaneous cross-dials converge on ONE connection
// instead of each side dropping the other's session as a duplicate.
func (d *Daemon) shouldReplace(existing *peerConn, newInitiatorHex string) bool {
	return newInitiatorHex < existing.initiatorPeerID(d.repo.Identity.HexID())
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
	if existing, exists := d.peers.Load(peerHex); exists {
		if !d.shouldReplace(existing.(*peerConn), peerHex) {
			_ = sess.Close() // duplicate session; keep the existing one
			return
		}
		// Challenger wins the tie-break: retire the old session. Its
		// goroutines wind down without touching our new entry (dropPeer).
		old := existing.(*peerConn)
		_ = old.sess.Close()
		d.peers.Delete(peerHex)
	}
	pc := &peerConn{
		peer:   mdns.Peer{PeerHex: peerHex, SeenAt: time.Now(), Source: "inbound"},
		secure: sc, sess: sess,
		peerIDhex: peerHex, addr: sc.RemoteAddr().String(),
		since: time.Now(), state: "live", resync: make(chan struct{}, 1),
	}
	d.peers.Store(peerHex, pc)
	defer d.dropPeer(pc)
	// Family dispatch: a relay session never sends a sync hello — its
	// first message is a fam=relay connect. Everything else is a sync.
	ctl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	_ = sc.SetDeadline(time.Now().Add(30 * time.Second)) // first msg must arrive promptly
	first, err := lrmsync.ReadMsg(ctl)
	if err != nil {
		return
	}
	_ = sc.SetDeadline(time.Time{})
	if first.Fam == relay.Fam {
		d.handleRelay(ctl, first)
		return
	}
	eng := lrmsync.New(d.repo)
	eng.AdvertiseFams = []string{relay.Fam}
	eng.OnHello = func(user, node string) {
		if user != "" {
			pc.remoteUser = user
		}
	}
	d.stampNode(eng)
	res, err := eng.ServeInbound(ctl, sess, first)
	_ = sess.Close()
	if err != nil {
		return
	}
	pc.remoteUser = res.RemoteUser
	d.learnPeer(res, pc.addr)
	if d.onSync != nil {
		d.onSync(res)
	}
	if res.PeerHasNews {
		d.event("peer %s advertised newer history — re-dialing", peerLabel(res))
		d.requestDial() // fetch what they have
		return
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
// dial rounds never block on discovery. A NEWLY seen peer triggers an
// immediate dial round — peers join the mesh within one announcement
// (~2s) instead of waiting for the refresh tick.
func (d *Daemon) browseLoop(ctx context.Context) {
	self := d.repo.Identity.HexID()
	mdns.BrowseContinuous(ctx, func(p mdns.Peer) {
		if p.PeerHex == self || p.Port == 0 {
			return
		}
		_, knownBefore := d.known.Load(p.PeerHex)
		d.known.Store(p.PeerHex, p)
		if !knownBefore {
			d.requestDial() // zero-lag join: dial now, not at the next tick
		}
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
		if existing, ok := d.peers.Load(p.PeerHex); ok {
			// Already connected: nudge the live session instead of
			// skipping — the ping round propagates tip changes both ways.
			existing.(*peerConn).nudge()
			return true
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
	// Static peers (--peer): dial any not already connected by address.
	d.statics.Range(func(key, val any) bool {
		addr := key.(string)
		p := val.(mdns.Peer)
		connected := false
		d.peers.Range(func(_, v any) bool {
			if v.(*peerConn).addr == addr {
				connected = true
				return false
			}
			return true
		})
		if connected {
			return true
		}
		muKey := "static:" + addr
		if _, dialing := d.dialMu.LoadOrStore(muKey, struct{}{}); dialing {
			return true
		}
		go func() {
			defer d.dialMu.Delete(muKey)
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
	if existing, exists := d.peers.Load(peerHex); exists {
		if !d.shouldReplace(existing.(*peerConn), d.repo.Identity.HexID()) {
			_ = sc.Close() // raced another session; keep the existing one
			return
		}
		old := existing.(*peerConn)
		_ = old.sess.Close()
		d.peers.Delete(peerHex)
	}
	d.pin(peerHex, sc.Remote.PubKey)
	sess := mux.NewSession(sc, true)
	pc := &peerConn{
		peer: p, secure: sc, sess: sess,
		peerIDhex: peerHex, addr: addr,
		since: time.Now(), state: "syncing", initiator: true,
		resync: make(chan struct{}, 1),
	}
	d.peers.Store(peerHex, pc)
	defer func() { d.dropPeer(pc); _ = sess.Close() }()
	eng := lrmsync.New(d.repo)
	eng.KeepCtl = true // persistent session: keepalive rides the ctl stream
	eng.AdvertiseFams = []string{relay.Fam}
	eng.OnHello = func(user, node string) {
		if user != "" {
			pc.remoteUser = user
		}
	}
	d.stampNode(eng)
	res, err := eng.SyncWithSession(sess, true, "")
	if err != nil {
		return
	}
	pc.remoteUser = res.RemoteUser
	pc.state = "live"
	d.learnPeer(res, addr)
	if d.onSync != nil {
		d.onSync(res)
	}
	if res.FastForwarded || res.MergedCommit != "" {
		d.requestDial() // our tip advanced — propagate to OTHER peers now
	}
	d.keepaliveLoop(ctx, pc, eng)
}

// keepaliveLoop keeps a dialer session alive: ping/pong liveness with RTT
// presence, tip-divergence detection both ways, and resync nudges. Any
// exit tears the session down and triggers a fresh dial round.
func (d *Daemon) keepaliveLoop(ctx context.Context, pc *peerConn, eng *lrmsync.Engine) {
	label := pc.remoteUser
	if label == "" {
		label = shortNodeHex(pc.peerIDhex)
	}
	d.event("peer %s live (%s)", label, pc.addr)
	ticker := time.NewTicker(d.cfg.KeepaliveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-pc.resync:
		}
		start := time.Now()
		tip, err := eng.PingRound(d.cfg.PingTimeout)
		if err != nil {
			if ctx.Err() == nil {
				d.event("peer %s lost (%v) — will re-dial", label, err)
				d.dropPeer(pc)
				_ = pc.sess.Close()
				d.requestDial()
			}
			return
		}
		pc.lastPing = time.Now()
		pc.lastRTT = time.Since(start)
		if tip != "" {
			if h, perr := cas.ParseHex(tip); perr == nil && !d.repo.DAG.Has(h) {
				d.event("peer %s has newer history — resyncing", label)
				d.dropPeer(pc)
				_ = pc.sess.Close()
				d.requestDial()
				return
			}
		}
	}
}

// PeerInfo is a live-session snapshot (presence table row).
type PeerInfo struct {
	User  string    `json:"user"`
	Peer  string    `json:"peer"`
	Addr  string    `json:"addr"`
	WS    string    `json:"ws,omitempty"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
	RTTms int64     `json:"rtt_ms"`
}

// Presence returns the current live-session table.
func (d *Daemon) Presence() []PeerInfo {
	out := []PeerInfo{}
	d.peers.Range(func(_, v any) bool {
		pc := v.(*peerConn)
		ws := pc.peer.WS
		out = append(out, PeerInfo{
			User: pc.remoteUser, Peer: pc.peerIDhex, Addr: pc.addr, WS: ws,
			State: pc.state, Since: pc.since, RTTms: pc.lastRTT.Milliseconds(),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		return out[i].Peer < out[j].Peer
	})
	return out
}

// --- control socket: live presence for the CLI ---

// controlLoop serves a tiny line-JSON API on .lrm/daemon.sock:
//
//	{"cmd":"status"} → presence table + uptime
//	{"cmd":"sync"}   → trigger a dial/nudge round now
//	{"cmd":"stop"}   → graceful shutdown
func (d *Daemon) controlLoop(ctx context.Context) {
	path := d.controlPath()
	// Refuse to shadow a live daemon; clean up a stale socket.
	if conn, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		_ = conn.Close()
		d.event("control: another daemon already serves %s", path)
		return
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		d.event("control: unix socket unavailable (%v)", err)
		return
	}
	d.ctrlMu.Lock()
	d.ctrlLn = ln
	d.ctrlMu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleControl(conn)
		}
	}()
	<-ctx.Done()
	_ = ln.Close()
}

// handleControl serves one control connection.
func (d *Daemon) handleControl(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	rd := bufio.NewReader(conn)
	line, err := rd.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		writeControlJSON(conn, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	switch req.Cmd {
	case "status":
		node := ""
		if d.nodeID != nil {
			node = d.nodeID.HexID()
		}
		writeControlJSON(conn, map[string]any{
			"ok": true, "user": d.repo.Config.User, "workspace": d.repo.Config.Workspace,
			"node": node, "uptime_sec": int64(d.Uptime().Seconds()), "peers": d.Presence(),
		})
	case "sync":
		d.requestDial()
		d.peers.Range(func(_, v any) bool {
			v.(*peerConn).nudge()
			return true
		})
		writeControlJSON(conn, map[string]any{"ok": true, "triggered": true})
	case "stop":
		if d.stopFn == nil {
			writeControlJSON(conn, map[string]any{"ok": false, "error": "no stop function"})
			return
		}
		writeControlJSON(conn, map[string]any{"ok": true, "stopping": true})
		go d.stopFn()
	default:
		writeControlJSON(conn, map[string]any{"ok": false, "error": "unknown cmd"})
	}
}

func writeControlJSON(conn net.Conn, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// handleRelay serves a fam=relay connect: verify the requesting device is
// paired and the request is freshly signed, dial the requested target,
// and pipe bytes blind. The daemon advertises the "relay" family in its
// hello, so peers can discover the capability before asking.
func (d *Daemon) handleRelay(ctl *mux.Stream, m lrmsync.Msg) {
	target, err := relay.VerifyRequest(m, d.book)
	if err != nil {
		d.event("relay refused: %v", err)
		_ = lrmsync.WriteMsg(ctl, lrmsync.Msg{Fam: relay.Fam, Type: "error", Error: err.Error()})
		return
	}
	dst, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		d.event("relay: target %s unreachable (%v)", target, err)
		_ = lrmsync.WriteMsg(ctl, lrmsync.Msg{Fam: relay.Fam, Type: "error", Error: "target unreachable: " + err.Error()})
		return
	}
	if err := lrmsync.WriteMsg(ctl, lrmsync.Msg{Fam: relay.Fam, Type: "open"}); err != nil {
		_ = dst.Close()
		return
	}
	d.event("relaying for %s -> %s", shortNodeHex(m.Extra["node"]), target)
	_ = relay.Pipe(dst, ctl)
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
