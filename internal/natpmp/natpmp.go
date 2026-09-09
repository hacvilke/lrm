// Package natpmp implements NAT-PMP / PCP port mapping (RFC 6886).
//
// Binary request layout (12 bytes) for TCP mapping:
//
//	Byte 0:    Version (0x00)
//	Byte 1:    OP (0x02 = TCP map, 0x01 = UDP map)
//	Bytes 2-3: Reserved (0x0000)
//	Bytes 4-5: Internal port
//	Bytes 6-7: Requested external port (0 = router picks)
//	Bytes 8-11: Lifetime in seconds
//
// Sent as a raw UDP packet to the gateway on port 5351.
package natpmp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	version     = 0x00
	opUDPLower  = 0x01
	opTCPMap    = 0x02
	serverPort  = 5351
	defaultLife = 3600
)

// Result is a successful mapping response.
type Result struct {
	ExternalPort uint16
	InternalPort uint16
	Lifetime     uint32
	Epoch        uint32
	Gateway      net.IP
}

// MapTCP requests a TCP mapping via NAT-PMP. external=0 lets the router pick.
func MapTCP(gateway net.IP, internal, external uint16, lifetime uint32) (*Result, error) {
	return mapPort(gateway, opTCPMap, internal, external, lifetime)
}

// MapUDP requests a UDP mapping via NAT-PMP.
func MapUDP(gateway net.IP, internal, external uint16, lifetime uint32) (*Result, error) {
	return mapPort(gateway, opUDPLower, internal, external, lifetime)
}

// UnmapTCP deletes a TCP mapping (lifetime=0).
func UnmapTCP(gateway net.IP, internal, external uint16) error {
	_, err := mapPort(gateway, opTCPMap, internal, external, 0)
	return err
}

func mapPort(gateway net.IP, op byte, internal, external uint16, lifetime uint32) (*Result, error) {
	if lifetime == 0 && external == 0 {
		// Deletion requires explicit ports; nothing to do.
		return &Result{Gateway: gateway}, nil
	}
	if lifetime == 0 {
		// keep as deletion request
	} else if lifetime < 60 {
		lifetime = defaultLife
	}
	req := make([]byte, 12)
	req[0] = version
	req[1] = op
	// bytes 2-3 reserved
	binary.BigEndian.PutUint16(req[4:6], internal)
	binary.BigEndian.PutUint16(req[6:8], external)
	binary.BigEndian.PutUint32(req[8:12], lifetime)

	raddr := &net.UDPAddr{IP: gateway, Port: serverPort}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("nat-pmp dial %s: %w", gateway, err)
	}
	defer conn.Close()

	// RFC 6886 retransmission: 250ms, 500ms, 1s, 2s, 4s... up to ~9s.
	delays := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		wait := 4 * time.Second
		if attempt < len(delays) {
			wait = delays[attempt]
		}
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		if err != nil {
			lastErr = err
			continue // retry
		}
		return parseResponse(buf[:n], op, gateway)
	}
	return nil, fmt.Errorf("nat-pmp: no response from %s: %v", gateway, lastErr)
}

func parseResponse(p []byte, wantOp byte, gw net.IP) (*Result, error) {
	if len(p) < 16 {
		return nil, fmt.Errorf("short NAT-PMP response (%d bytes)", len(p))
	}
	if p[0] != version {
		return nil, fmt.Errorf("bad NAT-PMP version %d", p[0])
	}
	if p[1] != 128+wantOp {
		return nil, fmt.Errorf("unexpected NAT-PMP opcode %d", p[1])
	}
	code := binary.BigEndian.Uint16(p[2:4])
	if code != 0 {
		return nil, fmt.Errorf("NAT-PMP error code %d (%s)", code, resultString(code))
	}
	return &Result{
		Epoch:        binary.BigEndian.Uint32(p[4:8]),
		InternalPort: binary.BigEndian.Uint16(p[8:10]),
		ExternalPort: binary.BigEndian.Uint16(p[10:12]),
		Lifetime:     binary.BigEndian.Uint32(p[12:16]),
		Gateway:      gw,
	}, nil
}

func resultString(code uint16) string {
	switch code {
	case 0:
		return "success"
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized / refused"
	case 3:
		return "network failure"
	case 4:
		return "out of resources"
	case 5:
		return "unsupported opcode"
	default:
		return "unknown"
	}
}

// DiscoverGateway returns the default route's gateway IP using (in order):
// /proc/net/route (Linux), `ip route`, `route -n`, common private fallback.
func DiscoverGateway() (net.IP, error) {
	if gw := fromProcRoute(); gw != nil {
		return gw, nil
	}
	if gw := fromIPRoute(); gw != nil {
		return gw, nil
	}
	// Last resort: probe likely candidates by UDP reachability on 5351.
	for _, c := range []string{"192.168.1.1", "192.168.0.1", "10.0.0.1", "172.16.0.1"} {
		ip := net.ParseIP(c)
		if reachable(ip) {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("could not determine default gateway")
}

func fromProcRoute() net.IP {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		if fields[1] != "00000000" { // destination 0.0.0.0
			continue
		}
		gwHex := fields[2]
		if len(gwHex) != 8 {
			continue
		}
		// Little-endian hex.
		var b [4]byte
		for i := 0; i < 4; i++ {
			var v uint
			_, _ = fmt.Sscanf(gwHex[i*2:i*2+2], "%02x", &v)
			b[i] = byte(v)
		}
		// /proc route gateway is little-endian: bytes are already network order reversed.
		return net.IPv4(b[3], b[2], b[1], b[0])
	}
	return nil
}

func fromIPRoute() net.IP {
	for _, argv := range [][]string{{"ip", "route", "show", "default"}, {"route", "-n"}} {
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if (f == "via" || f == "gateway") && i+1 < len(fields) {
					if ip := net.ParseIP(fields[i+1]); ip != nil {
						return ip
					}
				}
			}
			// `route -n` format: Destination Gateway ... last line 0.0.0.0 <gw>
			if len(fields) >= 2 && fields[0] == "0.0.0.0" {
				if ip := net.ParseIP(fields[1]); ip != nil {
					return ip
				}
			}
		}
	}
	return nil
}

func reachable(ip net.IP) bool {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip.String(), "5351"), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
