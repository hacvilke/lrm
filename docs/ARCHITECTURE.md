# LRM Architecture

## Vision

LRM (Log Replication Manager) is a real-time P2P version-control and code
collaboration engine. Developers discover each other directly and stream
encrypted state changes with zero central servers, zero storage cost, and
absolute privacy.

## Design theft (the good kind)

| System | What LRM copies |
|--------|-----------------|
| **Git** | Immutable Content-Addressed Storage: blobs, trees, commits named by SHA-256. One char changes → hash mutates to the root. |
| **BitTorrent / IPFS** | Large files split into 4KB–1MB chunks; peers fetch different pieces of one history from many peers in parallel. |
| **Figma** | Transparent real-time sync: local edits are detected, diffed, and streamed to peers immediately — no manual push/pull loop. |
| **Kafka / Raft** | DAG + vector clocks + append-only replication log pin down exactly when history diverges. |

## Repository layout (`.lrm/`)

```
.lrm/
  config.json          user, port, default branch
  identity.key         Ed25519 seed (0600) — THE peer identity
  index.json           virtual staging index (path → hash/size/mtime)
  peers.json           TOFU key pins (peerHex → pubkey)
  HEAD                 ref: refs/heads/main
  refs/heads/<name>    branch tips (hex commit hash)
  objects/ab/cdef...   CAS fanout: blobs, chunk manifests, trees, commits
  objects/tmp/         streaming write staging (atomic rename into place)
  logs/replication.log append-only JSONL replication audit trail
```

## Data model

- **Blob**: raw file bytes (files ≤ 64 KiB) or one chunk (larger files).
- **Manifest** (`chunker.Manifest`): `{chunk_size, total_size, chunks[], file_hash}`
  stored as its own CAS object; trees reference it with `chunked=true`.
- **Tree** (`merkle.Tree`): sorted entries `{name, hash, mode, size, is_dir, chunked}`.
  Address = `SHA-256("lrm-tree-v1\n" + canonicalJSON)` (domain separation so
  tree addresses never collide with blob addresses).
- **Commit** (`dag.Commit`): `{tree, parents[], author, peer, timestamp, message, clock}`.
  Address = `SHA-256("lrm-commit-v1\n" + canonicalJSON)`.
- **Vector clock**: `{peerHex: counter}` — merged on every commit/sync; compared
  to classify histories as ordered vs concurrent.
- **Replication log**: `{seq, time, type, peer, commit, message, clock}` where
  type ∈ {commit, fetch, push, merge, conflict, branch}.

## Data flow: edit → peers (Figma vibe)

```
  file write (OS)
       │  poll watcher (2s; inotify/FSEvents-ready interface)
       ▼
  staging.Scan → Merkle rebuild (parallel workers, streaming hashes)
       │  diff vs index → [added|modified|deleted]
       ▼
  daemon auto-commit (message "auto: …")  — or manual `lrm commit`
       │  vector clock increment + replog append
       ▼
  for each connected peer: have/want → stream missing objects (mux)
       │  AES-GCM frames, 16 KiB plaintext each
       ▼
  peers integrate: fast-forward | 3-way merge | HEAD-peer-* branch
```

## Networking stack

```
  ┌──────────── discovery ────────────┐  ┌────────── WAN dial ──────────┐
  │ mDNS _lrm._tcp.local (5353/udp)   │  │ UPnP map (SSDP→SOAP)         │
  │ LAN broadcast announce (8413/udp) │  │ NAT-PMP map (gw:5351 raw)    │
  └────────────────┬──────────────────┘  │ STUN public IP (pool)        │
                   ▼                      │ Port Key → verified dial   │
  ┌──────────────────────────────────────────────────────────────┐
  │ transport: X25519 ephemeral + Ed25519 auth + AES-GCM frames  │
  │ mux: N streams × 1 conn (control + parallel object streams)  │
  │ sync: hello → want/have commits → want-objects → integrate   │
  └──────────────────────────────────────────────────────────────┘
```

- **LAN**: `mdns.Browse` returns peers; daemon dials new ones (TOFU pin on
  first sight, enforced afterwards), runs bidirectional sync.
- **WAN**: `lrm share` runs UPnP → NAT-PMP → manual-fallback, learns the
  public IP via STUN (or UPnP `GetExternalIPAddress`), and prints a Port Key
  embedding IP + port + Ed25519 pubkey. `lrm join` dials it; the handshake
  drops instantly on PeerID mismatch (MITM protection).
- **Hygiene**: leases are renewed every 30 min by the daemon and explicitly
  deleted on SIGINT/SIGTERM.

## Sync protocol (summary)

1. **Hello**: both sides exchange `{peer, branch, heads[]}` on a control stream.
2. **Fetch**: dialer iteratively `want`s unknown commits (`have` returns full
   commit JSONs), walking parents until the graph closes.
3. **Objects**: multi-round `want-objects` bulk transfer on fresh mux streams:
   `[64B hex][8B size][bytes…]` per object, streamed straight into CAS temps.
4. **Integrate** via LCA:
   - remote == descendant → **fast-forward** + streaming checkout;
   - disjoint or overlapping edits → **conflict branch** `HEAD-peer-<short>`
     (local tip + workdir untouched);
   - non-overlapping divergence → **3-way auto-merge** commit (2 parents,
     merged vector clock) + checkout.
5. **Push**: dialer announces its tip (`push-tip`) so the responder integrates
   symmetrically — both sides converge without a second connection.

Full wire detail: [`PROTOCOL.md`](PROTOCOL.md).
WAN traversal detail: [`WAN_PORTKEY_SPEC.md`](WAN_PORTKEY_SPEC.md).

## Concurrency & memory model

- Merkle hashing: worker pool (8), one chunk buffer per worker.
- Object transfer: 32 KiB copy buffers; mux payloads ≤ 32 KiB; secure frames
  ≤ 16 KiB plaintext. No `ReadAll` on unbounded streams anywhere in the
  hot path (only for known-small objects with explicit caps).
- Daemon loops (accept / discover-dial / watch / lease-renew) are independent
  goroutines sharing the repo; `staging.Index` and peer map are mutex-guarded.
- External CLI mutations while the daemon runs are safe: the daemon reloads
  `index.json` every tick and skips auto-commit when the tree already equals
  HEAD's tree.

## Security properties

- Long-term identity: Ed25519; PeerID = SHA-256(pubkey).
- Forward secrecy: fresh X25519 ephemeral per connection.
- Authentication: signatures over the ephemeral transcript, bound to the
  Port Key's expected PeerID (WAN) or TOFU pins (LAN).
- Transport: AES-GCM with per-direction keys and enforced monotonic nonces
  (replay protection).
- Supply chain: zero third-party dependencies — `go.mod` has no requires.
