# LRM Quickstart — workplace & hobby

Ten minutes from install to a syncing mesh. This guide is the practical
companion to the [feature README](../README.md); the wire details live in
[PROTOCOL.md](PROTOCOL.md), the automation recipes in
[AUTOMATION.md](AUTOMATION.md).

## The mental model

Every machine holds the **full repo** — commits, history graph,
content-addressed storage. The network is just peers finding each other:

```
LAN (mDNS, zero-config)  →  WAN direct (Port Key)  →  hole-punch
→  paired-peer relay (ciphertext only)  →  resume where you left off
```

No server. No account. No hosting bill. The daemon watches your folder:
save a file → auto-committed → pushed to online peers in ~2s.

## Day 1: same office / same LAN

```sh
# maya starts the project
lrm init --user maya
lrm daemon

# everyone else, one time each
lrm init --user joe
lrm config workspace <maya's workspace id>   # she runs: lrm config workspace
lrm daemon
```

That's the whole setup. mDNS finds peers on the shared network, and from
then on it's save → sync all day. `lrm status` shows who's live and their
latency; strangers on the same Wi-Fi (different workspace) never see your
data — that's an enforced gate, not a promise.

## Remote teammate / different network

```sh
# on any machine reachable from the internet (or port-forwarded)
lrm share                    # prints a Port Key: lrm1_...

# teammate, first time (creates the repo inside the shared workspace)
lrm join lrm1_... --init

# later syncs — pick whichever path the network allows:
lrm sync --peer maya.example.com:8443              # direct
lrm sync --peer 10.0.0.2:8443 --via buddy:8443     # relayed via a paired peer
lrm join lrm1_... --punch --via buddy:8443         # NAT hole-punch, data direct
```

`--via` and `--punch --via` need a **paired** in-between device
(`lrm pair` on both, invite codes over any channel you trust). The relay
sees only ciphertext; punched connections carry data directly. If both
NATs are too unfriendly, the command says so and suggests the relay
fallback.

## Handing someone a file (no repo gymnastics)

```sh
lrm send report.pdf --peer maya.example.com:8443
lrm send dataset.zip --peer 10.0.0.2:8443 --via buddy:8443   # relayed
```

The file lands in the peer's `inbox/` — hash-verified, workspace-gated
(strangers are refused) — and their daemon auto-commits it. Received
files never silently overwrite anything: a name collision becomes
`report-1.pdf`.

## Keeping an eye on it

```sh
lrm watch        # live stream of mesh/sync/send events from your daemon
lrm status       # presence table, uptime, workspace
```

## Slow links

```sh
lrm sync --peer maya.example.com:8443 --bwlimit 512KB   # don't eat the upload
LRM_BWLIMIT=2MB lrm daemon                              # cap daemon transfers
```

Interruptions cost you almost nothing: transfers are chunk-granular
(64 KiB blocks), so a re-sync fetches only what's missing.

## Workplace fit

**Where lrm wins:**
- **Zero infrastructure** — air-gapped labs, ships, restricted networks:
  same commands, no central host to stand up or secure.
- **Binary/large files are first-class** — 64 KiB chunking + dedup +
  resume. No LFS, no size limits enforced by a vendor.
- **Conflicts never eat work** — concurrent edits land on a
  `HEAD-peer-xxxx` branch; both versions survive; you merge when ready.
- **Auditability** — every script run exports a timestamped `.txt`
  report you can keep in the repo itself.

**Where git+hub still wins:** pull requests, code review, CI on a
central remote, issue tracking. lrm replaces the *hub*, not the review
culture. Best fit: docs/config repos, design assets, small teams that
sit together anyway, regulated or offline environments.

## Hobby fit

- **Notes/knowledge vault** across laptop + desktop + a Raspberry Pi
  (every peer is a full replica — the Pi is an automatic backup).
- **Weekend project with a friend** — no org, no permissions dance:
  share a Port Key over Signal, done.
- **Game saves, assets, datasets between machines** — appended files
  only move their new blocks.
- **Uptime checks without a service** —
  `lrm run check_site.lr -- https://your-sideproject.dev`.

## Honest caveats (v0.1)

- WAN paths (punch/relay) work but assume reasonably friendly NATs;
  the relay fallback covers the rest.
- Desktop OSes only (Linux/macOS/Windows) — no phone client.
- One workspace per repo; cross-workspace sharing is `lrm send` only.
- Peer-exchange gossip (learning addresses through friends) is designed
  but not shipped — pairing is deliberately manual today.

## Command cheat sheet

| Task | Command |
|------|---------|
| Start | `lrm init --user NAME` then `lrm daemon` |
| Join a friend | `lrm join lrm1_... --init` |
| Sync now | `lrm sync` (LAN) / `lrm sync --peer H:P` |
| Via a friend | add `--via H:P` (pair first) |
| Through NAT | `lrm join lrm1_... --punch --via H:P` |
| Give a file | `lrm send FILE --peer H:P` |
| Watch live | `lrm watch` |
| Who's online | `lrm status` |
| History | `lrm log --graph` |
| Automate | `lrm run check.lr` / `lrm query repo.lrq` |
