# Git → LRM Reverse Map

LRM stands where git stands — but every central-server assumption is reversed
into a peer-to-peer equivalent. This is the complete translation table.

## Command map

| You used to (git, central) | You now (LRM, P2P) | Notes |
|----------------------------|--------------------|-------|
| `git init` | `lrm init --user NAME` | also mints your Ed25519 peer identity |
| `git add <files>` | *(nothing — automatic)* | the virtual staging index tracks all files transparently; `lrm status` shows the delta. `lrm add` exists as a compat alias (validates paths). |
| `git commit -m MSG` | `lrm commit -m MSG` | commits the Merkle root + vector clock; replog entry appended |
| `git status` | `lrm status` | branch, tip, root hash, instant delta list |
| `git log` | `lrm log` | adds vector clocks + peer authors |
| `git show` | `lrm show [HASH]` | commit + full file manifest |
| `git diff` | `lrm diff [H1 [H2]]` | workdir-vs-tip or tip-vs-tip |
| `git branch` / `git branch N` | `lrm branch [--list] [N]` | branches are local refs, synced as tips |
| `git checkout B` / `git switch B` | `lrm checkout B` | streaming workdir rewrite |
| `git merge B` | `lrm merge B` | fast-forward or 3-way; true conflicts stay local until you resolve |
| `git stash` | `lrm stash` | shelves workdir delta onto `refs/shelves/*` (compat layer) |
| `git remote add origin URL` | *(nothing — no remotes)* | peers are discovered (LAN) or dialed (Port Key). No URLs to manage. |
| `git push origin main` | `lrm share` then peer `lrm join`s — or just `lrm sync` / `lrm push` | pushing = making your history available; with the daemon running it happens continuously |
| `git pull` / `git fetch` | `lrm sync [--peer H:P]` / `lrm pull` | bidirectional: fetch + integrate + push-back in one session |
| `git clone URL dir` | `lrm clone <PORTKEY> <dir>` (= `join --init`) | verified dial + full history + checkout |
| `git push --force` | *(impossible by design)* | history is a content-addressed DAG; concurrent tips become `HEAD-peer-*` branches, never overwritten |
| merge conflicts (`<<<<<<<`) | `HEAD-peer-<short8>` branches | no conflict markers injected into your files — the peer's tip is preserved on a named branch, your workdir untouched |
| `git cat-file -p H` | `lrm cat-file H` | prints blobs / manifests / trees / commits |
| `git fsck` | `lrm fsck` | verifies CAS reachability from all refs (compat layer) |
| `git gc` | `lrm gc` | drops objects unreachable from refs + replog window (compat layer) |
| Pull Requests / forks | `lrm share` + `lrm join` | the "PR" is a live P2P session: fetch their tip, `lrm merge HEAD-peer-*`, they sync back |
| GitHub Actions | *(your machines)* | hooks run locally: `.lrm/hooks/` (roadmap) |

## Concept map

| Git concept | LRM reversal |
|-------------|--------------|
| Central remote (`origin`) | No origin. Every peer is a full remote; discovery replaces URLs. |
| Push / pull cycle | Continuous background sync (`lrm daemon`): edits stream in ~seconds. |
| SHA-1 object ids | SHA-256 everywhere, with domain-separated tree/commit addresses. |
| Index / staging area | In-memory virtual staging: always on, millisecond diffs, no `git add` ritual. |
| Reflog | Replication log (`.lrm/logs/replication.log`): every commit/fetch/merge/conflict with vector clocks. |
| Forced updates | Structurally impossible: divergent tips coexist as branches until merged. |
| LFS / large files | Built in: automatic 64 KiB chunking + manifests + parallel P2P fetch. |

## Compat layer (`lrm git-*` + aliases)

For muscle memory, LRM ships git-spelled aliases implemented in
`internal/gitcompat` (Phase 2): `add`, `push`, `pull`, `fetch`, `clone`,
`stash`, `stash pop`, `reset --soft`, `fsck`, `gc`, `remote -v` (lists
*peers*, read-only — there are no remotes to configure).

```bash
lrm push        # → announce + sync to all connected peers
lrm pull        # → sync from all connected peers (== lrm sync)
lrm clone <PORTKEY> ./dir
lrm stash && lrm stash pop
```
