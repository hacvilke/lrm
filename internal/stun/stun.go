// Package stun discovers the node's external WAN address by querying
// public STUN servers with raw UDP binding requests (RFC 5389).
//
// No central LRM servers are used — only a hardcoded pool of generic,
// non-tracking public STUN endpoints.
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// DefaultServers is the hardcoded pool of public STUN endpoints.
var DefaultServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun2.l.google.com:19302",
	"stun.cloudflare.com:3478",
	"stun.sipgate.net:3478",
}

const (
	msgBindingRequest  uint16 = 0x0001
	msgBindingResponse uint16 = 0x0101
	attrMappedAddress  uint16 = 0x0001
	attrXorMapped      uint16 = 0x0020
	magicCookie        uint32 = 0x2112A442
)

// DiscoverPublicIP queries the server pool concurrently and returns the
// first successfully resolved external IP.
func DiscoverPublicIP(servers []string, timeout time.Duration) (net.IP, error) {
	if len(servers) == 0 {
		servers = DefaultServers
	}
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	type res struct {
		ip  net.IP
		err error
	}
	ch := make(chan res, len(servers))
	for _, srv := range servers {
		go func(s string) {
			ip, err := Query(s, timeout)
			ch <- res{ip, err}
		}(srv)
	}
	var lastErr error
	for range servers {
		r := <-ch
		if r.err == nil && r.ip != nil {
			return r.ip, nil
		}
		lastErr = r.err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no STUN servers reachable")
	}
	return nil, lastErr
}

// Query sends a single binding request to addr (host:port) and parses the
// XOR-MAPPED-ADDRESS / MAPPED-ADDRESS response.
func Query(addr string, timeout time.Duration) (net.IP, error) {
	ip, _, err := QueryEndpoint(addr, timeout)
	return ip, err
}

// QueryEndpoint is Query but also returns the mapped PORT — the reflexive
// endpoint (ip:port) a hole-punch candidate needs.
func QueryEndpoint(addr string, timeout time.Duration) (net.IP, int, error) {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, 0, err
	}
	txn := make([]byte, 12)
	if _, err := rand.Read(txn); err != nil {
		return nil, 0, err
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], msgBindingRequest)
	binary.BigEndian.PutUint16(req[2:4], 0) // no attributes
	binary.BigEndian.PutUint32(req[4:8], magicCookie)
	copy(req[8:20], txn)
	if _, err := conn.Write(req); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, 0, fmt.Errorf("stun read %s: %w", addr, err)
	}
	return parseResponse(buf[:n], txn)
}

func parseResponse(p []byte, txn []byte) (net.IP, int, error) {
	if len(p) < 20 {
		return nil, 0, fmt.Errorf("short STUN response (%d bytes)", len(p))
	}
	msgType := binary.BigEndian.Uint16(p[0:2])
	if msgType != msgBindingResponse {
		return nil, 0, fmt.Errorf("unexpected STUN type 0x%04x", msgType)
	}
	if binary.BigEndian.Uint32(p[4:8]) != magicCookie {
		return nil, 0, fmt.Errorf("bad STUN magic cookie")
	}
	// Transaction ID must match (prevents off-path spoofing).
	for i := 0; i < 12; i++ {
		if p[8+i] != txn[i] {
			return nil, 0, fmt.Errorf("STUN transaction mismatch")
		}
	}
	off := 20
	for off+4 <= len(p) {
		attrType := binary.BigEndian.Uint16(p[off : off+2])
		attrLen := int(binary.BigEndian.Uint16(p[off+2 : off+4]))
		val := p[off+4:]
		if len(val) < attrLen {
			return nil, 0, fmt.Errorf("truncated STUN attribute")
		}
		val = val[:attrLen]
		switch attrType {
		case attrXorMapped:
			if ip, port, err := parseXorMapped(val, p[4:8], p[8:20]); err == nil {
				return ip, port, nil
			}
		case attrMappedAddress:
			if ip, port, err := parseMapped(val); err == nil {
				return ip, port, nil
			}
		}
		// Attributes are 32-bit padded.
		off += 4 + (attrLen+3)/4*4
	}
	return nil, 0, fmt.Errorf("no mapped address in STUN response")
}

func parseXorMapped(val, cookie, txn []byte) (net.IP, int, error) {
	if len(val) < 8 {
		return nil, 0, fmt.Errorf("short XOR-MAPPED-ADDRESS")
	}
	family := val[1]
	xPort := binary.BigEndian.Uint16(val[2:4])
	port := int(xPort ^ binary.BigEndian.Uint16(cookie[0:2]))
	if family == 0x01 { // IPv4
		if len(val) < 8 {
			return nil, 0, fmt.Errorf("short IPv4 address")
		}
		ip := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			ip[i] = val[4+i] ^ cookie[i]
		}
		return ip, port, nil
	}
	if family == 0x02 { // IPv6
		if len(val) < 20 {
			return nil, 0, fmt.Errorf("short IPv6 address")
		}
		mask := append(append([]byte{}, cookie...), txn...)
		ip := make(net.IP, 16)
		for i := 0; i < 16; i++ {
			ip[i] = val[4+i] ^ mask[i]
		}
		return ip, port, nil
	}
	return nil, 0, fmt.Errorf("unknown address family %d", family)
}

func parseMapped(val []byte) (net.IP, int, error) {
	if len(val) < 8 {
		return nil, 0, fmt.Errorf("short MAPPED-ADDRESS")
	}
	family := val[1]
	port := int(binary.BigEndian.Uint16(val[2:4]))
	if family == 0x01 {
		return net.IP(val[4:8]), port, nil
	}
	if family == 0x02 && len(val) >= 20 {
		return net.IP(val[4:20]), port, nil
	}
	return nil, 0, fmt.Errorf("unknown family %d", family)
}
