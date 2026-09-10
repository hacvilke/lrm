# LRM — Log Replication Manager

**Hyper-fast, real-time, peer-to-peer version control. No GitHub. No servers. No storage bills.**

LRM lets developers open a repo on their machines, auto-discover teammates over the
network, and collaborate on code in real time — with zero central authority, zero lag
(state streams directly peer-to-peer), and absolute privacy (everything is
end-to-end encrypted and content-addressed).

```
Developer A  ◄─── direct P2P (LAN mdns / WAN Port Key) ───►  Developer B
   .lrm/objects (SHA-256 CAS)         encrypted mux streams        .lrm/objects (SHA-256 CAS)
```

## 60-second quickstart

```bash
# Build
make build          # produces ./lrm (pure Go stdlib, no dependencies)

# Alice starts a project
mkdir demo && cd demo
lrm init --user alice
echo "hello LRM" > hello.txt
lrm commit -m "first commit"

# Same Wi-Fi: Bob finds Alice automatically
lrm peers                    # → alice @ 192.168.1.5:8443  ws:1a2b3c4d (same)
lrm daemon                   # background real-time sync (both sides)

# Pair your machines once — trust crosses all workspaces:
lrm pair                     # print this device's invite
lrm pair <invite> laptop     # on the other machine; paste its invite back

# Different Wi-Fi / across the globe: Alice shares a Port Key
lrm share
# → lrm1_AQQiUwEBIPvCWoLq6VlKc14ZBsidRqO_hvaGtYYQZa8mLBVepV0yrw

# Bob pastes it — router ports auto-open via UPnP/NAT-PMP, STUN finds the
# public IP, and a verified encrypted channel syncs history instantly:
lrm join lrm1_AQQiUwEBIPvCWoLq6VlKc14ZBsidRqO_hvaGtYYQZa8mLBVepV0yrw
```

## Feature matrix

| Tier | Feature | Status |
|------|---------|--------|
| **1 — Local engine** | Instant Merkle trees (parallel, streaming, single root hash) | ✅ |
| | Immutable SHA-256 Content-Addressed Storage (Git-style fanout) | ✅ |
| | In-Memory Virtual Staging (change watcher + instant delta diffs) | ✅ |
| | File chunking 4KB–1MB (64 KiB default) + manifests (BitTorrent/IPFS model) | ✅ |
| | Line diffs / unified patches, binary detection | ✅ |
| **2 — P2P mesh** | Zero-config LAN discovery (mDNS `_lrm._tcp` + UDP broadcast) | ✅ |
| | Workspace identity — mesh scoping: strangers on shared Wi-Fi never sync (legacy repos derive it from genesis; v2 Port Keys carry it) | ✅ |
| | Device identity + pairing — one node key per machine (`~/.lrm/`), address book, self-verifying invites (`lrm pair`) | ✅ |
| | Authenticated encryption (X25519 + Ed25519 + AES-GCM, MITM-drop) | ✅ |
| | Streaming multiplexer (N concurrent streams / 1 connection) | ✅ |
| | UPnP auto port mapping (SSDP → SOAP) | ✅ |
| | NAT-PMP / PCP fallback (raw 12-byte UDP to gateway:5351) | ✅ |
| | STUN public-IP discovery (hardcoded non-tracking pool) | ✅ |
| | Secure Port Key connection strings (compact + human forms) | ✅ |
| | Port hygiene (mapping teardown on SIGINT/SIGTERM) | ✅ |
| **3 — Sync engine** | Commit DAG + vector clocks (concurrency tracking) | ✅ |
| | LCA / merge-base resolver (optimal minimal-fetch) | ✅ |
| | Have/want graph negotiation + parallel object fetch | ✅ |
| | Fast-forward, clean 3-way auto-merge | ✅ |
| | Automated conflict branching (`HEAD-peer-<short>`, never overwrites) | ✅ |
| | Replication log (append-only JSONL audit trail) | ✅ |
| | Real-time daemon (watch → auto-commit → instant dial, ~2s peer latency) | ✅ |
| | Persistent sessions — keepalive ping/pong, RTT presence table, deterministic glare tie-break, live control socket | ✅ |
| **2.7 — WAN ladder** | TCP simultaneous-open hole punching — direct P2P through NATs, signaling relayed by a paired peer (`lrm join KEY --punch --via H:P`) | ✅ |
| | Paired-peer relay — dial any device through an online paired peer (`--via H:P`, end-to-end encrypted, relay sees only ciphertext) | ✅ |
| | Chunk-granular transfer resume — interrupted transfers re-fetch only missing 64 KiB blocks | ✅ |
| | Direct file handoff — `lrm send FILE --peer H:P` (workspace-gated, SHA-256-verified, lands in `inbox/`, auto-committed) | ✅ |
| | Bandwidth caps — `--bwlimit 512KB` on sync/join, `LRM_BWLIMIT` for the daemon | ✅ |
| | Live event stream — `lrm watch` (mesh/sync/send events over the control socket) | ✅ |
| **5 — Git-reverse++** | `blame`, `cherry-pick`, `grep`, `config`, `clean`, `describe`, `amend`, `checkout -b` | ✅ |
| | `bisect` (DAG-correct), `ref-log`, `archive`, `shortlog`, `mv`, `~`/`^` ref suffixes | ✅ |
| | `rebase` (linear, scratch-branch, continue/abort), `notes`, `.lrmignore`, `stash show/drop/clear`, `log --grep/--author`, GC honors ref-logs | ✅ |
| | `branch -d/-D`, `stash apply`, `rev-parse`, `merge-base`, `cherry`, `check-ignore` | ✅ |
| **4 — Scripting** | LRS runtime language (`.lr`): sharing + site/network tests, `.txt` reports | ✅ |
| | LRQ query language (`.lrq`): 8 read-only repo queries, `.txt` reports | ✅ |
| | `lrm run` / `lrm query` + bare `lrm file.lr` dispatch, tag delete | ✅ |
| **2.5 — Readability** | Unified patches in `diff`/`show` (binary/large-file safe, 1 MiB cap) | ✅ |
| | ASCII DAG `log --graph` + `log --oneline` | ✅ |
| | `share` warns when no local listener serves the port | ✅ |

## Command reference

| Command | What it does |
|---------|--------------|
| `lrm init [--user N] [--port P] [path]` | create a repo (identity + CAS + refs + log) |
| `lrm status` | branch, tip, root hash, pending changes + live presence table (via daemon control socket) |
| `lrm commit -m MSG` | version the current Merkle root |
| `lrm log [--limit N] [--graph] [--oneline] [--grep S] [--author A]` | history with vector clocks (graph = ASCII DAG; grep/author filter all modes) |
| `lrm show [HASH]` | commit detail + file list + unified patches |
| `lrm diff [H1 [H2]] [--stat]` | unified patches, or file list with `--stat` |
| `lrm branch [--list] [NAME]` | list / create branches |
| `lrm branch -d\|-D NAME...` | delete branches (safe refuses unmerged) |
| `lrm checkout <BRANCH>` | switch branch (rewrites workdir, streaming) |
| `lrm merge <BRANCH\|HASH>` | fast-forward or 3-way merge |
| `lrm cat-file <HASH>` | print a CAS object |
| `lrm replog [--last N]` | replication audit trail |
| `lrm peers [--timeout S]` | discover LAN developers (workspace-tagged) |
| `lrm share [--port P] [--no-upnp] [--no-natpmp]` | map router port, print Port Key |
| `lrm join <PORTKEY> [--init]` | verified dial + full history sync |
| `lrm sync [--peer H:P]` | sync with LAN peers (or one peer) |
| `lrm daemon [--port P] [--peer H:P]` | real-time engine: watch + announce + sync + keepalive (+ a continuously-dialed static peer) |
| `lrm pair [INVITE] [NAME]` | device pairing — prints your invite, or pairs with the invite's device (works outside any repo) |
| `lrm devices` | list the paired-device address book |
| `lrm unpair <ID>` | remove a paired device (id or unique short prefix) |
| `lrm add [paths...]` | confirm auto-tracked paths (the `add` ritual, reversed away) |
| `lrm push [--peer H:P]` | sync history out to peers (force-push is impossible by design) |
| `lrm pull` / `lrm fetch` | sync in from peers (bidirectional in one session) |
| `lrm clone <PORTKEY> [dir]` | verified dial + full history + checkout |
| `lrm stash [push\|pop\|apply\|list\|show\|drop\|clear]` | shelf / restore / inspect workdir deltas |
| `lrm reset [--soft\|--mixed\|--hard] <H>` | move branch ref (± index ± workdir) |
| `lrm fsck` | verify CAS reachability from all refs |
| `lrm gc [--dry-run]` | prune unreachable objects |
| `lrm remote -v` | list LIVE peers (there is nothing to configure) |
| `lrm blame <FILE> [REF]` | per-line authorship (first-parent walk) |
| `lrm cherry-pick <REF>` | replay a commit onto this branch (file-level 3-way) |
| `lrm grep [-i] [-l] <PAT> [REF]` | literal-substring search in workdir or history |
| `lrm config [--list] [user\|port\|workspace [V]]` | view / change identity settings (workspace = mesh scoping escape hatch) |
| `lrm clean [-n] [-f] [-d] [-x]` | list / delete untracked files (dry run by default; `-x` includes ignored) |
| `lrm describe [REF]` | nearest tag (`v1-3-g<short>`) |
| `lrm commit --amend [-m MSG]` | fold workdir state into the tip commit |
| `lrm checkout -b NAME` | create a branch and switch to it |
| `lrm bisect start BAD [GOOD]` + `good\|bad\|skip` / `run CMD` / `reset` / `log` | binary-search history for the first bad commit |
| `lrm ref-log [BRANCH] [--last N]` | branch-tip move history (recover amended tips) |
| `lrm archive [REF] -o FILE [--format F]` | export snapshot (tar\|tar.gz\|zip, streaming) |
| `lrm shortlog [REF] [--limit N]` | commit counts per author |
| `lrm mv <SRC> <DST>` | rename a workdir file |
| `lrm rebase <UP> [--continue\|--abort]` | replay branch commits onto upstream (linear only, atomic move) |
| `lrm notes add\|show\|list\|remove <REF>` | local-only commit annotations (shown by `show`) |
| `lrm rev-parse [--short] [--abbrev-ref] <REF>` | print resolved commit hash(es) |
| `lrm merge-base <A> <B>` | best common ancestor of two refs |
| `lrm cherry <UP> [HEAD]` | branch-unique commits (`+` missing, `-` applied) |
| `lrm check-ignore [-v] <PATH>...` | test `.lrmignore` rules |
| `lrm tag [NAME [HASH]]` | list / create lightweight tags |
| `lrm tag -d NAME` | delete a tag |
| `lrm run <S.lr> [--report P] [--timeout D] [-- args]` | run an LRS script, export `.txt` report |
| `lrm query <Q.lrq> [--report P]` | run LRQ repo queries, export `.txt` report |
| `lrm <file.lr \| file.lrq>` | bare form: run script / queries directly |

Git users: see [`docs/GIT_REVERSE_MAP.md`](docs/GIT_REVERSE_MAP.md) — every git
workflow reversed into its LRM equivalent (`push`→`share`/`sync`,
`pull`→`sync`, `clone`→`join`, …), plus the `lrm git-*` compat layer.

## Architecture

- [`docs/QUICKSTART.md`](docs/QUICKSTART.md) — workplace & hobby setup in ten minutes
- [`docs/AUTOMATION.md`](docs/AUTOMATION.md) — LRS/LRQ recipes: share→join roundtrips, health checks, convergence proofs
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system design, repo layout, data flow
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md) — wire protocol (handshake, mux frames, sync messages)
- [`docs/WAN_PORTKEY_SPEC.md`](docs/WAN_PORTKEY_SPEC.md) — cross-Wi-Fi Port Key + UPnP/NAT-PMP/STUN spec
- [`docs/LRSPEC.md`](docs/LRSPEC.md) — LRS script language (types, builtins, reports)
- [`docs/LRQSPEC.md`](docs/LRQSPEC.md) — LRQ query language (8 query kinds, refs, tables)

## Engineering guardrails (enforced)

- **No central servers.** Discovery is mDNS/LAN broadcast; WAN uses Port Keys +
  public DHT-style STUN for address learning only. No AWS/Supabase/databases.
- **No large files in memory.** All hashing, chunking, transfer, and checkout
  stream with bounded (16–64 KiB) buffers. A 10 GB file syncs with flat RAM.
- **No framework overhead.** Pure Go standard library, zero dependencies —
  execution stays close to OS primitives (UDP/TCP sockets, file I/O).

## Layout

```
cmd/lrm/            CLI entrypoint
internal/
  identity/         Ed25519 peer identities (PeerID = SHA-256(pubkey))
  cas/              content-addressed object store (streaming put/get)
  chunker/          4KB–1MB file chunking + manifests + reassembly
  merkle/           parallel Merkle tree builder + differ
  diff/             line diffs, unified patches
  staging/          virtual staging index + change watcher
  store/            .lrm repo layout, refs, commits
  dag/              commit DAG (content-addressed, vector-clocked)
  vectorclock/      vector clocks (happened-before / concurrent)
  lca/              lowest-common-ancestor / merge-base resolver
  replog/           append-only replication log (JSONL)
  mdns/             LAN discovery (mDNS + broadcast)
  stun/             STUN binding requests (public IP)
  upnp/             UPnP IGD mapping (SSDP + SOAP)
  natpmp/           NAT-PMP/PCP mapping (raw UDP)
  portkey/          Port Key encode/decode (compact + human)
  transport/        authenticated-encryption channel (X25519/Ed25519/AES-GCM)
  mux/              streaming multiplexer (many streams, one conn)
  sync/             have/want sync, merge, conflict branches, checkout
  daemon/           background engine (watch + announce + map + instant sync)
  patch/            unified-patch builder (bounded, binary/large aware)
  gitcompat/        git-reverse layer: stash, reset, fsck, gc, tag, add
  cli/              command implementations (incl. push/pull/clone/remote)
  lr/               LRS interpreter: lexer, parser, eval, stdlib, reports
  lrq/              LRQ engine: query parser, tables, repo reads
docs/               architecture, protocol, WAN spec, git map, LR/LRQ specs
examples/           check_site.lr, share_team.lr, repo.lrq
```

## Testing

```bash
make test        # unit + integration (sync, conflicts, merges over mux)
make test-race   # with the race detector
./lrm peers      # live LAN check (needs a teammate running lrm daemon)
```

## Roadmap

- [ ] STUN/ICE UDP hole punching for symmetric NATs (TCP mapping covers most routers today)
- [ ] DHT peer routing (Kademlia) for global discovery without key exchange
- [ ] Delta-compressed object transfer (xdelta) for huge binaries
- [ ] FUSE workdir overlay for instant multi-GB checkouts
- [ ] `lrm web` — local DAG visualizer
