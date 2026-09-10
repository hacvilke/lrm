# The Mesh Is the Product — LRM Network-First Direction

Status: proposal · Sep 2026

## 1. Where the effort went

Current non-test line counts:

| Layer | Lines | Test lines |
|---|---|---|
| Mesh / network (transport, mux, mdns, stun, upnp, natpmp, portkey, daemon, identity) | **2,772** | **268** |
| History engine (store, cas, dag, merkle, sync, staging, lca, vectorclock, diff, chunker, replog, patch) | 3,970 | — |
| Git-compat command surface (`internal/gitcompat` + `internal/cli/gitcompat.go`) | **~4,478** | **1,544** |
| LRS / LRQ languages | 4,018 | — |

Five of the last commits are "git-reverse" rounds. The git-compat surface is now
**bigger than the entire network layer**, and it has 6× the test coverage. The
codebase is healthy (builds clean on go1.27, all tests green) — but the layer
that makes LRM *LRM* is the thinnest, least-tested one.

**Rule going forward: a feature only goes in if it makes two machines talk to
each other better.** Ways to inspect local history don't qualify. The file layer
is a tenant of the network, not the product.

## 2. Audit — what the mesh can and cannot do today

What is real and good:

- SIGMA-lite handshake (X25519 + Ed25519 + AES-GCM), per-direction session keys,
  monotonic-nonce replay protection (`internal/transport`)
- Multi-stream mux over one connection (`internal/mux`)
- LAN discovery: UDP broadcast :8413 + mDNS `_lrm._tcp` (`internal/mdns`)
- WAN reachability: UPnP (SSDP→SOAP), NAT-PMP/PCP, STUN public-IP discovery
- Port Keys — compact, MITM-proof dial strings (`internal/portkey`)
- Sync engine: hello/want/have negotiation, parallel bulk streams, LCA merge,
  vector clocks, conflict branching (`internal/sync`)

What is missing — and it is all networking, none of it is file-layer:

1. **No workspace identity on the wire.** The LAN announcement is
   `{peer, pub, user, port}` (`internal/mdns/mdns.go:37`) and the sync hello is
   `{peer, branch, heads}` — neither says *which project* this node hosts.
   Consequence: two strangers on the same café Wi-Fi both running `lrm daemon`
   handshake, negotiate, and their repos are disjoint — so
   `integrate()` stores the stranger's tip as a `HEAD-peer-<short>` conflict ref
   and logs a `conflict` replog entry (`internal/sync/sync.go:426`, and
   `docs/PROTOCOL.md` "Integration rules"). The mesh will sync *anyone*.
   This is the project's #1 networking bug and it exists because the network
   never grew a "workspace" concept.
2. **Identity is per-repo.** `identity.key` and `peers.json` (trust pins) live
   inside each repo's `.lrm/` (`internal/store/store.go:86`,
   `internal/daemon/daemon.go:469`). Every repo is a different PeerID, so your
   laptop is a stranger to your desktop on every new project, and every
   workspace needs a fresh Port Key. There is no device identity, no address
   book, no pairing.
3. **Sessions are one-shot.** `dialPeer` runs one `SyncWithSession` and closes
   (`internal/daemon/daemon.go:279-305`); inbound is the same. There is no
   keepalive, no ping, no presence — "connected" only exists mid-sync. The mesh
   is a dial→sync→close poll loop every 2–10 s.
4. **The protocol only knows repo objects.** Eight message types, all sync
   (`docs/PROTOCOL.md` §3). No versioned envelope, no events, nothing else can
   ride the wire. This is the structural reason the product drifts toward
   command parity: the network cannot carry anything but commits.
5. **No relay, no hole-punching.** The CLI tells users to "use a fallback relay"
   (`internal/cli/cli.go:1085,1102`) — no relay exists. All transport is TCP.
   Behind CGNAT (very common), UPnP/NAT-PMP fail and WAN is simply dead.
6. **Port Keys embed one IP:port.** DHCP renews break them; there are no
   candidate addresses and no dial-by-PeerID.
7. **No peer exchange.** Peers are learned only via LAN broadcast or a manually
   pasted Port Key. Nobody tells anybody who else is in the workspace.
8. **The daemon is a foreground process.** No detach, no control socket, so
   `lrm peers` / `lrm status` cannot ask the running node anything.
9. **Transfers don't resume and aren't rate-limited.** Chunking and parallelism
   exist; an interrupted 2 GB blob restarts from zero.

## 3. The native model — LRM's own concepts

Four first-class nouns, none of them borrowed:

**Node** — one keypair per *machine*, not per repo (`~/.lrm/node.key`).
PeerID = SHA-256(pubkey) as today. Everything the machine does on the mesh is
signed by its node key. A node hosts many workspaces.

**Workspace** — a folder + its history, identified by a workspace ID
(derived at `init` from founder node ID + name, so it's stable and offline).
Every announcement, every hello, every join carries it. The mesh syncs
same-workspace peers *only* — this closes the stranger hole and makes
multi-project nodes coherent.

**Pairing** — the trust primitive, at node level, stored in a machine-wide
address book (`~/.lrm/peers.json`): peerID → verified pubkey, known addresses,
last seen, relay reachability. Pair once (QR / invite string / tap on LAN);
trust crosses all workspaces.

**Session** — a persistent, keepalive'd, muxed connection. A session stays up
until it drops; presence is the session table (route, RTT, since). Sync becomes
an event-driven message family *inside* sessions instead of a dial-per-cycle.

On top of those, one horizontal change:

**Message envelopes.** Every mux control frame becomes
`{ver, family, type, reqID, payload}` with negotiated protocol versions.
Sync is family 1. Families that follow naturally: `event` (peer joined,
workspace changed, snapshot made), `transfer` (send a file to a peer directly,
no workspace required), `relay` (signaling + forward), `control` (ask a node
what it hosts). This is what keeps LRM from ever being "just a VCS transport".

And one reachability ladder, tried in order for any peer:

```
LAN mDNS  →  WAN direct (STUN)  →  UDP hole-punch  →  paired-peer relay
```

The relay hop is the important one: **any online paired peer can relay for an
unreachable one**. No central server ever — which is what makes "no GitHub"
actually true on CGNAT networks, where it is currently false.

## 4. Native command surface

```
Node        lrm up / lrm down            start/stop the resident node
            lrm status                   presence table: peer, route, rtt, since

Peers       lrm pair                     pair this machine with another
            lrm peers                    live sessions + known peers
            lrm ping @peer               RTT + route used
            lrm trust / untrust @peer

Workspace   lrm open [dir]               join a folder into the mesh
            lrm workspaces               what this node hosts / shares
            lrm invite                   workspace join key (no IP inside)

Data        lrm send @peer <path>        direct transfer, no repo needed
            lrm sync                     reconcile now (redundant once live)

History     lrm save -m "…"              snapshot (commit stays as alias)
            lrm history / log            what happened
            lrm restore / diff / branch / merge   (already built — freeze)
```

The git-compat commands keep working but stop growing. No round 7.

## 5. Build order — infra before features

**P0 — small diffs, big fixes (days, not weeks)**

1. Workspace ID: generate at `init`, carry in mdns announcement + `hello`,
   gate sync on match. Closes the stranger hole. Touches `mdns`, `sync`,
   `daemon`, `portkey` (join keys carry workspace ID).
2. Node identity + address book: promote `identity.key`/`peers.json` to
   `~/.lrm/`, keep per-repo config only for workspace metadata. `lrm pair`
   minimal version: invite string over any channel, verify, pin.

**P1 — the mesh becomes real**

3. Persistent sessions: keep connections up, ping/keepalive, drop on timeout;
   presence table powering `lrm peers` / `lrm status`.
4. Message envelopes + version negotiation; move sync inside unchanged
   (family 1). Test harness for the mesh packages (they are at 268 test lines).
5. Daemonize: `lrm up` backgrounds the node; unix-socket control API so every
   CLI command talks to the live node instead of opening its own connections.

**P2 — WAN that always works**

6. UDP hole-punching using the existing STUN client (ephemeral port pairs,
   simultaneous open). First non-TCP path.
7. Paired-peer relay: dial-by-PeerID routed through an online peer.
8. Transfer resume + bandwidth caps on bulk streams.

**P3 — the network earns new abilities**

9. Peer exchange (address-book gossip between paired peers).
10. Event family → `lrm watch` output, notifications, dashboard on the control
    socket. `lrm send` (direct file transfer) once envelopes + sessions exist.

## 6. Non-goals

- More git command parity. What exists ships; the surface is frozen.
- Anything central: no accounts, no hosted relay, no telemetry. Relays are
  paired peers or nothing.
- A second UI before the control socket exists.
