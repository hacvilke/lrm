# APPENDIX: LRM WAN Cross-Wi-Fi Port Key Specification

Problem: connect developers on completely different Wi-Fi networks without
forcing manual home-router configuration.

Solution: an automated **UPnP / NAT-PMP Port Forwarding Engine** alongside a
**Secure Connection String (Port Key)** system — zero-configuration for most
routers, plus a simple encrypted cryptographic key developers paste to find
and connect across the globe.

## 1. How the LRM Port Key works

A Port Key is not just an IP address — it is a self-contained, secure
connection string containing everything a peer needs to verify, locate, and
encrypt data sent to another peer.

`lrm share` generates a string containing three vital pieces of information:

```
lrm_key: [Public_IP_or_DDNS]:[Mapped_External_Port]@[Peer_Public_Cryptographic_Identity_Key]
```

| Field | Purpose |
|-------|---------|
| **Network address** | External public IP of the router + the specific port LRM opened. |
| **Cryptographic ID** | Host node's public key (compact form) / SHA-256 PeerID (human form). During the handshake a signature over the ephemeral transcript is verified; if the peer cannot prove ownership of the matching private key, the connection is instantly dropped (MITM protection). |

Compact encoding (preferred): `lrm1_<base64url(version‖ipLen‖ip‖port‖pubkey)>`.
Human encoding: `lrm_key: [ip]:[port]@[peerID-hex]` (verified TOFU fallback).

Data model (`internal/portkey`):

```go
type LrmPortKey struct {
    Version    uint8
    ExternalIP net.IP
    Port       uint16
    PeerID     []byte // SHA-256 public-key fingerprint
    PubKey     ed25519.PublicKey
}
```

## 2. The Automated Port Forwarding Pipeline

```
               [ Start LRM Share ]
                          │
                          ▼
           [ Query Local Gateway via UPnP ]
           ├── Success ──> [ Map Port on Router ] ──┐
           └── Fail                                  │
                │                                    ▼
                ▼                          [ Fetch Public IP ]
     [ Query via NAT-PMP / PCP ]            (via public STUN)
           ├── Success ──> [ Map Port ] ─────────────┤
           └── Fail                                  │
                │                                    ▼
                ▼                          [ Generate LRM Key ]
     [ Fallback to Manual Port ] ────────────────────┘
```

### Step A: UPnP (Universal Plug and Play) mapping

Most consumer routers ship with UPnP enabled. LRM broadcasts an `M-SEARCH`
request over UDP to `239.255.255.250:1900`, parses the `LOCATION` device
description, locates the `WANIPConnection` (or `WANPPPConnection`) service's
`controlURL`, then sends a SOAP `AddPortMapping` request:

> "Please route external port 8443 to my local machine's internal port 8443."

Implemented in `internal/upnp` (pure stdlib: `net`, `net/http`, `encoding/xml`).

### Step B: NAT-PMP / PCP (Port Control Protocol) mapping

If UPnP is disabled (Apple routers, hardened networks), LRM falls back to
NAT-PMP: a raw 12-byte UDP packet to the gateway on port 5351.

Binary layout for NAT-PMP:

| Bytes | Field |
|-------|-------|
| Byte 0 | Version (`0x00`) |
| Byte 1 | OP code (`0x02` TCP mapping, `0x01` UDP) |
| Bytes 2–3 | Reserved (`0x0000`) |
| Bytes 4–5 | Internal port (e.g. `0x20FB` = 8443) |
| Bytes 6–7 | Requested external port |
| Bytes 8–11 | Lifetime in seconds (e.g. 3600) |

Response (16 bytes): version, opcode (`128+op`), result code, epoch,
internal port, mapped external port, lifetime. Retransmission schedule per
RFC 6886: 250ms → 500ms → 1s → 2s → 4s.

Gateway discovery order: `/proc/net/route` → `ip route` / `route -n` →
UDP-probe of common private gateways.

Implemented in `internal/natpmp` (pure stdlib UDP).

### Step C: Public IP detection

LRM learns its external WAN address from:

1. UPnP `GetExternalIPAddress()` (when Step A succeeded), else
2. Raw RFC 5389 STUN binding requests to a hardcoded pool of public,
   non-tracking servers (`stun.l.google.com:19302`, `stun.cloudflare.com:3478`,
   …), parsing `XOR-MAPPED-ADDRESS` (transaction-ID verified against spoofing).

Implemented in `internal/stun` (raw UDP, concurrent pool queries, first wins).

## 3. Feature requirements (checklist)

- [x] Automated gateway mapping via UPnP **and** NAT-PMP on `share`/daemon start.
- [x] Cryptographic verification: Port-Key dials execute an authenticated
      handshake against the embedded key; unverified handshakes close immediately.
- [x] Public IP detection via STUN pool with raw UDP binding requests.
- [x] `LrmPortKey` connection-string struct + compact/human codecs.

## 4. System guardrails

- **Non-blocking fallback**: if UPnP and NAT-PMP both fail (e.g. carrier-grade
  NAT), the system prints: *"Automatic port mapping failed. Please manually
  forward port X or use a fallback relay."* and still reports LAN addresses.
- **Port cleanup**: on SIGINT/SIGTERM the daemon sends explicit deletion
  requests (`DeletePortMapping` / lifetime-0 NAT-PMP) to tear down opened
  ports and maintain network hygiene. Leases are also renewed every 30 min
  while the daemon runs.
