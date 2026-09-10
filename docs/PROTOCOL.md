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
| `hello` | both | `peer, branch, heads[], ws, extra{user}` | advertise identity + branch tips + workspace |
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
