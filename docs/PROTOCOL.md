# LRM Wire Protocol (v0.1)

All multi-byte integers are big-endian. All crypto uses Go stdlib primitives.

## 1. Secure channel (`internal/transport`)

TCP + 3-message SIGMA-lite handshake, then AES-GCM frames.

### Handshake

```
I → R:  ePub_I                       (32B raw X25519 ephemeral)
R → I:  ePub_R                       (32B)
        u32 len || AES-GCM(kHS, n=0, staticPub_R || sig_R)
I → R:  u32 len || AES-GCM(kHS, n=1, staticPub_I || sig_I)
```

- `shared = X25519(ePriv_I, ePub_R) = X25519(ePriv_R, ePub_I)`
- `kHS = SHA-256("lrm-hs-v1" || shared || ePub_I || ePub_R)` (AES-256)
- `sig_R = Ed25519.Sign(sk_R, ePub_I || ePub_R)`
- `sig_I = Ed25519.Sign(sk_I, ePub_R || ePub_I)`
- Verifier checks the signature AND (WAN dial) that
  `SHA-256(staticPub) == expected PeerID` from the Port Key, else the
  connection is dropped instantly.

### Session keys

```
base  = SHA-256("lrm-sess-v1" || shared || staticPub_I || staticPub_R)
kInit = SHA-256(base || "init")   # initiator → responder
kResp = SHA-256(base || "resp")   # responder → initiator
```

### Frames

```
u32 totalLen || 12B nonce || AES-GCM(key_dir, nonce, plaintext)
```

- Plaintext ≤ 16 KiB per frame; larger writes are chunked.
- Nonce = `0x00000000 || u64 counter`, strictly monotonic per direction
  (out-of-order → connection killed: replay protection).

## 2. Multiplexer (`internal/mux`)

Many logical streams over one secure connection.

```
u32 streamID || u8 flags || u32 payloadLen || payload[..]
```

- Flags: `0x01 DATA`, `0x02 FIN`, `0x04 RST`.
- Payload ≤ 32 KiB.
- Stream IDs: dialer opens odd (1,3,5…), responder opens even (2,4,6…) —
  no allocator coordination needed.
- Typical session: stream 1 = control (JSON lines), streams 3,5,7… =
  parallel object bulk transfers while graph negotiation continues.

## 3. Sync messages (`internal/sync`, control stream, JSON + `\n`)

| Type | Direction | Fields | Meaning |
|------|-----------|--------|---------|
| `hello` | both | `peer, branch, heads[], ws, extra{user,node,nodepub,oneshot?}` | advertise identity + branch tips + workspace + device; `oneshot="1"` = CLI one-shot: initiator FINs ctl after the round |
| `ping` | dialer → peer | `heads[tip]` | keepalive; carries the dialer's tip |
| `pong` | peer → dialer | `heads[tip]` | keepalive reply with the responder's tip |
| `want` | → peer | `want[commitHex…]` | request commits by hash |
| `have` | → peer | `commits[commitJSON…]` | commit bodies (small, inline) |
| `want-objects` | → peer | `objects[hex…], extra{stream}` | request blobs/trees/chunks; data follows on a fresh mux stream |
| `push-have` | → peer | `commits[…]` | unsolicited commit offer (push leg) |
| `push-tip` | → peer | `peer, heads[tip]` | "integrate this tip if you can" |
| `done` | → peer | — | fetch complete, close session |
| `error` | → peer | `error` | fatal request error |

### Workspace gate (mesh scoping)

`ws` is the 16-byte-hex workspace ID (config `workspace`):

- both sides non-empty and **different** → the sync is refused with an
  `error` message before any object moves (strangers on shared Wi-Fi
  never see each other's data);
- local empty, remote non-empty → the local repo **adopts** the remote
  workspace and persists it (the v1-Port-Key join flow);
- anything else → allowed.

`lrm init` mints a fresh workspace ID. Repos created before this field
existed derive it deterministically from their genesis commit
(`SHA-256("lrm-ws-v1" || min-root)[:16]`), so peers that already share
history keep syncing after upgrading. `lrm config workspace <hex>` is the
explicit escape hatch for deliberately merging two independently-started
histories (disjoint histories still land on conflict branches — nothing is
ever overwritten).

### Persistent sessions & keepalive

After a completed sync the dialer may keep the control stream open
(daemon mode). The responder keeps serving after `done` until the stream
is FIN'd or the connection drops; one-shot dialers FIN the stream when
their sync returns. The dialer pings every 15s (10s pong timeout); the
responder pongs with its tip, and closes the whole session when the
dialer's tip is news to it (hang-up-and-callback: both sides re-dial and
fetch). Simultaneous cross-dials (glare) converge deterministically: both
sides keep the session whose initiator has the lower PeerID.

hello extras: `user` (display name), `node` / `nodepub` (device identity
from the pairing layer, may be absent).

### Object bulk stream

Opened by the responder per `want-objects` request; one header + bytes
per object, in request order:

```
64B ascii hex (REQUESTED address — domain-separated trees keep their
               tree-address label even though bytes hash differently)
u64 size                      (0xFFFF…FFFF = "peer lacks this object")
size bytes                    (streamed, 32 KiB buffers)
```

The receiver streams bytes into `objects/tmp/` while hashing; if the content
hash differs from the requested address (tree/commit domain separation), the
bytes are filed under the requested address as well.

### Integration rules (both sides run these)

Let `base = LCA(localTip, remoteTip)`:

- `remoteTip == nil` → nothing to do.
- `localTip == nil` → adopt remote + checkout.
- `base == remoteTip` → already up to date.
- `base == localTip` → fast-forward + checkout.
- else 3-way file merge over `flatten(base/local/remote)`:
  - no overlapping paths → merge commit (parents = both tips, merged clock) + checkout;
  - overlapping paths (or disjoint histories) → `HEAD-peer-<short8>` ref =
    remote tip; workdir + local branch untouched; replog `conflict` entry.

## 4. Discovery

### LAN broadcast (primary, `internal/mdns`)

- UDP broadcast to `255.255.255.255:8413`, JSON every 2s:
  `{"v":1,"peer":hex,"pub":hex,"user":str,"port":int,"ws":hex}`.
- Listeners bind `:8413` and collect unique peers. The daemon skips
  dialing peers that announce a foreign `ws` (legacy peers without one are
  still dialed and gated by the handshake).

### mDNS (secondary)

- PTR query for `_lrm._tcp.local.` to `224.0.0.251:5353`.
- LRM TXT records: `peer=<hex> port=<n> user=<s> pub=<hex> ws=<hex>`.

## 5. Port Key (WAN dial strings, `internal/portkey`)

Compact (preferred — carries the full pubkey for MITM-proof dialing):

```
v1: "lrm1_" || base64url_nopad( u8 version=1 || u8 ipLen || ip[4|16] || u16 port || 32B ed25519 pubkey )
v2: "lrm1_" || base64url_nopad( u8 version=2 || u8 ipLen || ip[4|16] || u16 port || 32B ed25519 pubkey || 16B workspace ID )
```

v2 keys (emitted by `lrm share`) are scoped to a workspace: `join`/`clone`
creates the repo inside it, so the first sync passes the workspace gate.
v1 keys still decode (no workspace — the repo adopts it from the remote
hello).

Human (fingerprint only — TOFU warning on use):

```
lrm_key: [IP|DDNS]:[port]@[64-hex PeerID]
```

See [`WAN_PORTKEY_SPEC.md`](WAN_PORTKEY_SPEC.md) for the mapping pipeline.

## 6. Control socket (`internal/daemon`)

The daemon serves a line-JSON API on `<repo>/.lrm/daemon.sock`:

| Request | Response |
|---------|----------|
| `{"cmd":"status"}` | `{ok, user, workspace, node, uptime_sec, peers:[{user,peer,addr,state,since,rtt_ms}]}` |
| `{"cmd":"sync"}` | `{ok, triggered:true}` — immediate dial + keepalive nudge round |
| `{"cmd":"stop"}` | `{ok, stopping:true}` — graceful shutdown (port mappings torn down) |

## 7. Paired-peer relay (`internal/relay`, fam=`relay`)

A paired, online peer can carry a connection to a third device. The relay
sees only ciphertext — the inner handshake is end-to-end with the target.

```
client                       relay (fam="relay")                target
  |-- mux stream: {"fam":"relay","type":"connect",    -------------->|
  |   "extra":{target,node,ts,sig}}                    dial target (10s)
  |<------------------ {"fam":"relay","type":"open"} --|
  | ===== blind pipe; client runs the normal handshake with TARGET ====|
```

- `sig` = ed25519(device key) over `"lrm-relay-v1|<target>|<ts>"`,
  timestamp fresh ±2 min, device must be in the relay's address book
  (`lrm pair`). Unpaired devices are refused.
- The client then speaks the §1 handshake *over the pipe*
  (`HandshakeOver`) and pins the target's PeerID — the relay cannot
  MITM (it has no role in the key exchange).
- CLI: `--via H:P` on `sync` / `join` / `clone`.

## 8. TCP hole punching (`internal/punch`, fam=`punch`)

Two peers behind NATs open a direct TCP session via simultaneous open.
Signaling (a few hundred bytes) rides a paired-peer relay; data flies
direct.

**Signaling.** `lrm join <portkey> --punch --via H:P` reserves 3
`SO_REUSEPORT` listeners (the candidates) and sends
`{"fam":"punch","type":"connect","extra":{target,node,ts,sig,offer}}` to
the relay — the same auth extras as §7 plus the offer JSON:

```
{"peer":<hex PeerID>, "pub":<hex repo pubkey>, "cands":["ip:port",…],
 "ws":<workspace hex>, "user":<display name>}
```

The relay (a paired device) dials the target's daemon, delivers the
offer line, and returns the answer line — nothing else. Punch lines on
raw conns are prefixed `LRMPUNCH1` + JSON + `\n`; the daemon peeks the
first bytes of every inbound conn (preamble → punch signaling, anything
else → normal handshake via a replaying `PeekConn`).

**Candidates.** IP selection: `LRM_PUNCH_IP` override → STUN public IP
→ first LAN address. Ports: the reserved listener ports (port
preservation on the NAT is assumed; symmetric NATs fail — fall back to
`--via`).

**Roles.** Lower PeerID dials (`PunchDial`), the other accepts
(`PunchAccept`).

- *PunchDial*: spray-dial every remote candidate × every local reserved
  port (reuseport binds, IPv4); first success wins atomically, losers
  are closed, and the reserved listeners are torn down (closing them
  resets the silent conns the peer's hole-openers parked there). A
  drain goroutine also accepts-and-discards those parked conns while
  spraying — otherwise their 4-tuples stay allocated (CLOSE_WAIT) and
  every spray dial fails to bind.
- *PunchAccept*: (a) accept the initiator's dials on the reserved
  listeners, validating each concurrently — in practice by running the
  real §1 responder handshake; dead conns EOF instantly, only the
  winner carries bytes; (b) hole-openers dial the peer's candidates to
  pin the NAT mappings, peek 500 ms for data: silent conns (they
  reached a listener, not a dial socket) are closed to free the tuple,
  data-bearing conns are TCP-simultaneous-open links to the initiator's
  live dial and join the validator pool.

The surviving connection runs the normal handshake → mux → sync, direct
and end-to-end. A one-shot CLI responder advertises `oneshot` in its
hello so the initiating daemon FINs the control stream after the sync
round instead of holding a keepalive session no one is left to answer.
