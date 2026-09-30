# LRM Security Model

LRM is a peer-to-peer version control system: repositories move directly
between machines, with no central server in the middle. That removes the
"trust GitHub" story and replaces it with "trust nobody by default, and
prove everything at the edges." This document states what LRM guarantees,
what it deliberately does not, and where the boundaries are.

## Threat model

An attacker is assumed to:

- sit on the same LAN and send/receive arbitrary UDP (mDNS) and TCP,
- know any peer's IP address and port (they are announced on the LAN),
- have full control of a *different* LRM installation, its keys, and its
  workspace ID if they were ever given it,
- be able to send malformed, oversized, or hostile protocol traffic,
- be able to author hostile repositories (trees, commits, objects).

An attacker is **not** assumed to:

- know your workspace ID, or hold your private keys,
- have local code execution on a machine in your address book.

## Guarantees

**1. The workspace is the gate.**
Every repository carries a workspace ID — 16 random bytes, minted at
`lrm init`, carried inside Port Keys, never derived from anything guessable.
The sync handshake compares it: two nodes with **non-empty and different**
workspace IDs refuse each other before any object moves
(`workspace mismatch (…) — sync refused, no objects exchanged`). Joining a
workspace is the act of receiving its ID (a Port Key, a pairing flow, or a
config import), which makes the ID a bearer secret: treat it like a shared
password for the project, not like a username.

Discovered peers from *other* workspaces are skipped before dialing and
after handshake; mDNS can only ever *list* peers, it can never authorize
one.

**2. Devices, not just peers.**
`lrm pair` establishes a machine identity (device node key) and an address
book entry; relay and hole-punch signaling between devices is signature
checked, so an online third device cannot redirect your connection or
forge a punch offer. A pairing is per device, and revocable.

**3. Traffic is confidential and authenticated.**
Every connection performs a SIGMA-lite handshake over X25519 (ECDH) bound
to static Ed25519 identities, and all post-handshake traffic is AES-GCM
frames with per-direction keys. Length fields are bounded
(32 KiB per mux frame). Object transfers re-hash content while streaming,
so a malicious relay can neither read nor alter what it forwards.

**4. Hostile content cannot write outside the repository.**
Object addresses are domain-separated (`lrm-tree-v1` / `lrm-commit-v1`
prefixes), so a blob can never be reinterpreted as a tree or commit. Every
peer-supplied tree path is validated before it can touch the filesystem
(`CleanTreePath`: no absolute paths, no `..`, no empty segments, no NUL or
backslash, no control or bidi-override characters) and a tree with even one
hostile entry is refused whole — checkout, and the tree builder used during
merges, both gate on it.

**5. Object bytes must match their address.**
When a peer hands over bytes for a tree or commit address, LRM re-hashes
the stream and refuses anything that does not hash to the address the
repository asked for (`possible poisoning — object dropped`). A peer
chooses both the bytes and the address it claims they belong to; the
repository only trusts its own arithmetic.

**6. The dashboard is a window, not a door.**
`lrm dashboard` serves a read-only local view. It binds `127.0.0.1` by
default, makes no network calls, and the file browser is confined to the
repository root: traversal, absolute paths, `.lrm` (where `identity.key`
lives), and symlinks that leave the repo are all refused. Downloads are
served as opaque attachments, so a synced `.html` file can never execute on
the dashboard's origin, and every peer-supplied name is HTML-escaped.
Binding it beyond localhost is possible (`--host 0.0.0.0`) and warns you:
that exposes your working tree to whoever can reach the port.

**7. Private keys stay private.**
`identity.key` is created `0600` and is never sent, logged, or written into
a repository. The daemon's control socket is `0600` for the same reason.

## Known limits — read these

- **Repositories are not encrypted at rest.** LRM protects the network
  path and the local views. Anyone who can read the files on your disk
  (backups, shared accounts, a stolen laptop) can read your history,
  exactly as with git.
- **The workspace ID is shared by everyone in the workspace.** Anyone who
  learns it can sync the whole project. Rotating it means re-pairing.
- **mDNS announces your presence.** It advertises user name, peer ID,
  public key, port, and workspace participation to the local network. Use
  explicit addresses if you want no announcements.
- **A forged keypair is a new device, not your device.** Someone can
  generate keys and announce a fresh identity; they cannot impersonate an
  identity already in your address book, and relay/punch signaling for
  paired devices is signature checked.
- **UPnP/NAT-PMP open your listening port** on the router. Descriptions
  published to the router are fixed strings. Disable with
  `lrm daemon --no-upnp` / `--no-natpmp`.
- **Port Keys are bearer tokens.** Whoever holds one can use it while it
  is valid; share them over a trusted channel.

## Findings from the round-6 red-team pass

Found by attacking the product (not by reading code), fixed, and covered
by tests that fail on the old behavior:

| Finding | Impact | Fix / test |
| --- | --- | --- |
| `Checkout` joined peer-supplied tree paths directly under the working dir | A hostile tree could write **outside the repo** (`../../.ssh/authorized_keys`) | All paths validated before any write; hostile trees refused whole — `internal/sync/security_test.go` |
| `copyBlobToAddress` filed received bytes at the address the peer named, without re-hashing | **CAS poisoning**: a peer decides what "the tree of commit X" is | Streaming re-hash against blob/tree/commit addresses; mismatch dropped + reported |
| `BuildTreeFromMap` recursed forever on a leading-slash key | A hostile tree could hang a merge (DoS) | Keys must be clean tree paths — `TestBuildTreeRefusesHostileKeys` |
| Tree paths accepted control/bidi characters | Spoofed file names in listings and the GUI | Rejected by `CleanTreePath` — `internal/merkle/path_test.go` |
| Control socket created with default permissions | Other local accounts could query presence and nudge syncs | Socket chmod `0600` |
| Dashboard browsing by construction read arbitrary paths | Local page could expose `~/.ssh`, `identity.key` | Root confinement + `.lrm` refusal + symlink escape checks + opaque downloads — `internal/dash/dash_test.go` |

## Verifying a workspace

```sh
lrm status            # live presence: who is connected right now
lrm dashboard         # local window: peers, syncs, activity, files
lrm fsck              # every reachable object present?
```

Security-relevant tests live next to the code they protect:
`internal/merkle/path_test.go`, `internal/sync/security_test.go`,
`internal/dash/dash_test.go`.

## Reporting

If you find a way to sync into a workspace you were not given, to write
outside a repository, or to read files a view should not reach, treat it
as critical: open an issue with a repro, or contact the maintainer
privately first if the impact is broad.
