// Package mdns implements Zero-Config Peer Discovery.
//
// Two mechanisms, fastest-wins:
//  1. LRM LAN broadcast (UDP 255.255.255.255:8413): JSON announcements.
//     Works on virtually all home/office Wi-Fi with zero setup.
//  2. Best-effort mDNS query/response for _lrm._tcp.local on
//     224.0.0.251:5353 (multicast DNS), for networks where broadcast is
//     filtered but multicast is routed.
//
// Both carry: PeerID hex, TCP port, user name, protocol version.
package mdns

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// ServiceName is the mDNS service type.
const ServiceName = "_lrm._tcp.local."

// BroadcastPort is the LAN broadcast discovery port.
const BroadcastPort = 8413

// MDNSPort is the standard multicast DNS port.
const MDNSPort = 5353

// MDNSGroup is the IPv4 mDNS multicast group.
const MDNSGroup = "224.0.0.251"

// Peer is a discovered developer node.
type Peer struct {
	PeerHex string   `json:"peer"`
	PubKey  string   `json:"pub,omitempty"` // hex ed25519 pub (for TOFU verify)
	User    string   `json:"user"`
	Port    int      `json:"port"`
	WS      string   `json:"ws,omitempty"`   // workspace id hex (mesh scoping)
	NodeHex string   `json:"node,omitempty"` // device node PeerID (pairing layer)
	NodePub string   `json:"nodepub,omitempty"`
	Addrs   []string `json:"addrs,omitempty"`
	SeenAt  time.Time
	Source  string // "broadcast" | "mdns"
}

// Addr returns the first dialable address.
func (p *Peer) Addr() string {
	if len(p.Addrs) > 0 {
		return net.JoinHostPort(p.Addrs[0], itoa(p.Port))
	}
	return ""
}

// Announcement is the broadcast payload.
type Announcement struct {
	Version int    `json:"v"`
	Peer    string `json:"peer"`
	Pub     string `json:"pub"`
	User    string `json:"user"`
	Port    int    `json:"port"`
	WS      string `json:"ws,omitempty"`   // workspace id hex
	Node    string `json:"node,omitempty"` // device node PeerID hex
	NodePub string `json:"nodepub,omitempty"`
}

// Advertiser periodically announces this node on the LAN.
type Advertiser struct {
	ann    Announcement
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// StartAdvertiser begins LAN announcements every interval until Stop.
// wsHex is the workspace ID this node's repo belongs to (may be empty for
// legacy repos — announced without a workspace). nodeHex/nodePubHex are the
// machine's device identity (pairing layer; may be empty).
func StartAdvertiser(peerHex, pubHex, nodeHex, nodePubHex, wsHex, user string, port int, interval time.Duration) *Advertiser {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	a := &Advertiser{
		ann: Announcement{Version: 1, Peer: peerHex, Pub: pubHex, User: user, Port: port,
			WS: wsHex, Node: nodeHex, NodePub: nodePubHex},
		stopCh: make(chan struct{}),
	}
	a.wg.Add(1)
	go a.loop(interval)
	return a
}

func (a *Advertiser) loop(interval time.Duration) {
	defer a.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	_ = a.broadcast() // announce immediately
	for {
		select {
		case <-a.stopCh:
			return
		case <-t.C:
			_ = a.broadcast()
		}
	}
}

func (a *Advertiser) broadcast() error {
	raw, _ := json.Marshal(a.ann)
	raddr := &net.UDPAddr{IP: net.IPv4bcast, Port: BroadcastPort}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(raw)
	return err
}

// Stop halts announcements.
func (a *Advertiser) Stop() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	a.wg.Wait()
}

// Browse listens for announcements for duration, returning unique peers.
func Browse(ctx context.Context, duration time.Duration) ([]Peer, error) {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	peers := map[string]*Peer{}
	var mu sync.Mutex
	collect := func(p Peer) {
		mu.Lock()
		defer mu.Unlock()
		if ex, ok := peers[p.PeerHex]; ok {
			ex.SeenAt = p.SeenAt
			// Merge addrs.
			for _, ad := range p.Addrs {
				found := false
				for _, e := range ex.Addrs {
					if e == ad {
						found = true
						break
					}
				}
				if !found {
					ex.Addrs = append(ex.Addrs, ad)
				}
			}
			return
		}
		cp := p
		peers[p.PeerHex] = &cp
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		listenBroadcast(ctx, collect)
	}()
	go func() {
		defer wg.Done()
		queryMDNS(ctx, collect)
	}()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		out = append(out, *p)
	}
	return out, nil
}

// BrowseContinuous streams peers until ctx is done.
func BrowseContinuous(ctx context.Context, cb func(Peer)) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		listenBroadcast(ctx, cb)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			queryMDNS(c, cb)
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	wg.Wait()
}

func listenBroadcast(ctx context.Context, cb func(Peer)) {
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: BroadcastPort}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(256 * 1024)
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var ann Announcement
		if err := json.Unmarshal(buf[:n], &ann); err != nil || ann.Peer == "" {
			continue
		}
		host, _, _ := net.SplitHostPort(raddr.String())
		if host == "" {
			host = raddr.IP.String()
		}
		cb(Peer{
			PeerHex: ann.Peer, PubKey: ann.Pub, User: ann.User, Port: ann.Port, WS: ann.WS,
			NodeHex: ann.Node, NodePub: ann.NodePub,
			Addrs: []string{host}, SeenAt: time.Now(), Source: "broadcast",
		})
	}
}

// --- minimal mDNS ---

// queryMDNS sends a PTR query for ServiceName and parses any SRV/TXT/A
// answers (best-effort; broadcast is the reliable path).
func queryMDNS(ctx context.Context, cb func(Peer)) {
	gaddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", MDNSGroup, MDNSPort))
	if err != nil {
		return
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return
	}
	defer conn.Close()
	q := buildPTRQuery(ServiceName)
	if _, err := conn.WriteTo(q, gaddr); err != nil {
		return
	}
	buf := make([]byte, 9000)
	deadline := time.Now().Add(4 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if time.Now().After(deadline) {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			continue
		}
		if peers := parseMDNSResponse(buf[:n]); len(peers) > 0 {
			host, _, _ := net.SplitHostPort(addr.String())
			for _, p := range peers {
				if host != "" {
					p.Addrs = []string{host}
				}
				p.SeenAt = time.Now()
				p.Source = "mdns"
				cb(p)
			}
		}
	}
}

func buildPTRQuery(name string) []byte {
	// Header: ID=0, flags=0, QD=1, AN=NS=AR=0
	pkt := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	pkt = append(pkt, encodeName(name)...)
	// QTYPE=PTR(12), QCLASS=IN(1)
	pkt = append(pkt, 0, 12, 0, 1)
	return pkt
}

func encodeName(name string) []byte {
	var out []byte
	for _, part := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(part)))
		out = append(out, part...)
	}
	out = append(out, 0)
	return out
}

// parseMDNSResponse extracts LRM TXT records (peer=...,port=...,user=...).
// Returns peers found in the packet (best-effort parse, skips unknowns).
func parseMDNSResponse(pkt []byte) []Peer {
	if len(pkt) < 12 {
		return nil
	}
	qd := int(pkt[4])<<8 | int(pkt[5])
	an := int(pkt[6])<<8 | int(pkt[7])
	ns := int(pkt[8])<<8 | int(pkt[9])
	ar := int(pkt[10])<<8 | int(pkt[11])
	off := 12
	skipName := func() bool {
		for {
			if off >= len(pkt) {
				return false
			}
			b := pkt[off]
			if b == 0 {
				off++
				return true
			}
			if b&0xC0 == 0xC0 {
				off += 2
				return true
			}
			off += 1 + int(b)
		}
	}
	// Skip questions.
	for i := 0; i < qd; i++ {
		if !skipName() {
			return nil
		}
		off += 4
		if off > len(pkt) {
			return nil
		}
	}
	var peers []Peer
	// Scan answers + authority + additional for TXT records.
	for i := 0; i < an+ns+ar; i++ {
		if !skipName() {
			return peers
		}
		if off+10 > len(pkt) {
			return peers
		}
		typ := int(pkt[off])<<8 | int(pkt[off+1])
		// class(2) ttl(4)
		rdlen := int(pkt[off+8])<<8 | int(pkt[off+9])
		off += 10
		if off+rdlen > len(pkt) {
			return peers
		}
		rdata := pkt[off : off+rdlen]
		off += rdlen
		if typ == 16 { // TXT
			if p := parseTXT(rdata); p != nil {
				peers = append(peers, *p)
			}
		}
	}
	return peers
}

func parseTXT(rdata []byte) *Peer {
	kv := map[string]string{}
	off := 0
	for off < len(rdata) {
		l := int(rdata[off])
		off++
		if off+l > len(rdata) {
			break
		}
		part := string(rdata[off : off+l])
		off += l
		if eq := strings.Index(part, "="); eq > 0 {
			kv[part[:eq]] = part[eq+1:]
		}
	}
	peer, ok := kv["peer"]
	if !ok || peer == "" {
		return nil
	}
	p := &Peer{PeerHex: peer, PubKey: kv["pub"], User: kv["user"], WS: kv["ws"],
		NodeHex: kv["node"], NodePub: kv["nodepub"]}
	if pt := kv["port"]; pt != "" {
		var n int
		_, _ = fmt.Sscanf(pt, "%d", &n)
		p.Port = n
	}
	return p
}

// LocalAddrs returns non-loopback IPv4 addresses for announcements.
func LocalAddrs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
