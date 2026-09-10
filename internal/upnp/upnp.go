// Package upnp implements automated gateway port mapping via UPnP IGD.
//
// Flow: SSDP M-SEARCH (UDP 239.255.255.250:1900) → fetch device description
// XML → locate WANIPConnection/WANPPPConnection control URL → SOAP
// AddPortMapping / DeletePortMapping / GetExternalIPAddress.
package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ssdpAddr    = "239.255.255.250:1900"
	msearchWait = 3 * time.Second
	httpTimeout = 6 * time.Second
	urnIPConn   = "urn:schemas-upnp-org:service:WANIPConnection:1"
	urnPPPConn  = "urn:schemas-upnp-org:service:WANPPPConnection:1"
)

// Gateway is a discovered UPnP IGD with a usable connection service.
type Gateway struct {
	// Location is the device description URL.
	Location    string
	ControlURL  string // absolute SOAP endpoint
	ServiceType string // WANIPConnection or WANPPPConnection URN
	http        *http.Client
}

// DiscoverGateway broadcasts M-SEARCH and returns the first IGD that
// exposes a WAN connection service.
func DiscoverGateway(timeout time.Duration) (*Gateway, error) {
	if timeout <= 0 {
		timeout = msearchWait
	}
	locations, err := ssdpSearch(timeout)
	if err != nil {
		return nil, err
	}
	if len(locations) == 0 {
		return nil, fmt.Errorf("no UPnP gateways found via SSDP")
	}
	var lastErr error
	for _, loc := range locations {
		gw, err := NewGateway(loc)
		if err != nil {
			lastErr = err
			continue
		}
		return gw, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no WAN connection service found")
	}
	return nil, lastErr
}

// NewGateway fetches the device description at location and resolves the
// connection service control URL.
func NewGateway(location string) (*Gateway, error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(location)
	if err != nil {
		return nil, fmt.Errorf("fetch device desc: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	svcType, controlPath, err := parseDeviceDesc(raw)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(controlPath)
	if err != nil {
		return nil, err
	}
	return &Gateway{
		Location: location, ControlURL: base.ResolveReference(ref).String(),
		ServiceType: svcType, http: client,
	}, nil
}

// AddPortMapping asks the router to forward externalPort → internalAddr:internalPort.
func (g *Gateway) AddPortMapping(externalPort, internalPort int, internalAddr, proto, desc string, lifetimeSec int) error {
	if proto != "TCP" && proto != "UDP" {
		proto = "TCP"
	}
	args := map[string]string{
		"NewRemoteHost":             "",
		"NewExternalPort":           itoa(externalPort),
		"NewProtocol":               proto,
		"NewInternalPort":           itoa(internalPort),
		"NewInternalClient":         internalAddr,
		"NewEnabled":                "1",
		"NewPortMappingDescription": desc,
		"NewLeaseDuration":          itoa(lifetimeSec),
	}
	return g.soap("AddPortMapping", args)
}

// DeletePortMapping removes a mapping (network hygiene on exit).
func (g *Gateway) DeletePortMapping(externalPort int, proto string) error {
	if proto != "TCP" && proto != "UDP" {
		proto = "TCP"
	}
	return g.soap("DeletePortMapping", map[string]string{
		"NewRemoteHost":   "",
		"NewExternalPort": itoa(externalPort),
		"NewProtocol":     proto,
	})
}

// ExternalIP returns the gateway's WAN address.
func (g *Gateway) ExternalIP() (net.IP, error) {
	body, err := g.soapRaw("GetExternalIPAddress", nil)
	if err != nil {
		return nil, err
	}
	ip := extractTag(body, "NewExternalIPAddress")
	if ip == "" {
		return nil, fmt.Errorf("no external IP in response")
	}
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return nil, fmt.Errorf("invalid external IP %q", ip)
	}
	return parsed, nil
}

// --- SSDP ---

func ssdpSearch(timeout time.Duration) ([]string, error) {
	raddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	req := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ns=01; ns=01\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"
	if _, err := conn.WriteTo([]byte(req), raddr); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 8192)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // timeout ends the search window
		}
		loc := parseLocation(buf[:n])
		if loc != "" && !seen[loc] {
			seen[loc] = true
			out = append(out, loc)
		}
	}
	return out, nil
}

func parseLocation(resp []byte) string {
	for _, line := range strings.Split(string(resp), "\r\n") {
		if len(line) > 9 && strings.EqualFold(line[:9], "LOCATION:") {
			return strings.TrimSpace(line[9:])
		}
	}
	for _, line := range strings.Split(string(resp), "\n") {
		if len(line) > 9 && strings.EqualFold(line[:9], "LOCATION:") {
			return strings.TrimSpace(line[9:])
		}
	}
	return ""
}

// --- device description XML ---

type deviceDesc struct {
	Services []serviceDesc `xml:"device>serviceList>service"`
	Devices  []deviceDesc  `xml:"device>deviceList>device"`
}

type serviceDesc struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

func parseDeviceDesc(raw []byte) (svcType, controlURL string, err error) {
	var dd deviceDesc
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	if err := dec.Decode(&dd); err != nil {
		return "", "", fmt.Errorf("parse device desc: %w", err)
	}
	// Depth-first search for WAN connection service.
	var walk func(d deviceDesc) (string, string, bool)
	walk = func(d deviceDesc) (string, string, bool) {
		for _, s := range d.Services {
			if s.ServiceType == urnIPConn || s.ServiceType == urnPPPConn {
				return s.ServiceType, s.ControlURL, true
			}
		}
		for _, sub := range d.Devices {
			if st, cu, ok := walk(sub); ok {
				return st, cu, ok
			}
		}
		return "", "", false
	}
	// The top-level decode nests one device; also try raw scan fallback.
	if st, cu, ok := walk(dd); ok {
		return st, cu, nil
	}
	// Fallback: string scan (tolerates namespace-prefixed XML).
	s := string(raw)
	for _, urn := range []string{urnIPConn, urnPPPConn} {
		i := strings.Index(s, urn)
		if i < 0 {
			continue
		}
		window := s[i:]
		if j := strings.Index(window, "</service>"); j > 0 {
			window = window[:j]
		}
		if cu := extractTag(window, "controlURL"); cu != "" {
			return urn, cu, nil
		}
	}
	return "", "", fmt.Errorf("WANIPConnection/WANPPPConnection service not found")
}

// --- SOAP ---

func (g *Gateway) soap(action string, args map[string]string) error {
	_, err := g.soapRaw(action, args)
	return err
}

func (g *Gateway) soapRaw(action string, args map[string]string) (string, error) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?>`)
	sb.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`)
	sb.WriteString(`<s:Body>`)
	sb.WriteString(`<u:` + action + ` xmlns:u="` + g.ServiceType + `">`)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// Stable order not required; keep simple.
	for _, k := range keys {
		sb.WriteString("<" + k + ">" + xmlEscape(args[k]) + "</" + k + ">")
	}
	sb.WriteString(`</u:` + action + `>`)
	sb.WriteString(`</s:Body></s:Envelope>`)
	req, err := http.NewRequest("POST", g.ControlURL, strings.NewReader(sb.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.ServiceType+`#`+action+`"`)
	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("SOAP %s: %w", action, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	body := string(raw)
	if resp.StatusCode >= 400 || strings.Contains(body, "UPnPError") || strings.Contains(body, "s:Fault") {
		code := extractTag(body, "errorCode")
		desc := extractTag(body, "errorDescription")
		if code == "" {
			code = itoa(resp.StatusCode)
		}
		return "", fmt.Errorf("UPnP %s failed (code %s: %s)", action, code, desc)
	}
	return body, nil
}

func extractTag(body, tag string) string {
	open := "<" + tag + ">"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</"+tag+">")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
