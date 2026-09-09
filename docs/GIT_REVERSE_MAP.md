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
| `git blame F` | `lrm blame F [REF]` | per-line authorship; follows the first-parent chain through merges |
| `git cherry-pick H` | `lrm cherry-pick REF` | file-level 3-way replay; conflicts abort cleanly, workdir untouched |
| `git grep PAT` | `lrm grep [-i] [-l] PAT [REF]` | literal substring (no regex dialect); workdir or any ref; binary/large skipped |
| `git config user.name` | `lrm config user [NAME]` | view/set author name; `port` likewise; peer id is immutable |
| `git clean -fd` | `lrm clean [-f] [-d] [-x]` | dry run by default — nothing deletes without `-f`; `-x` includes ignored paths |
| `git describe` | `lrm describe [REF]` | nearest reachable tag (`v1-3-g<short>`); BFS over all parents |
| `git commit --amend` | `lrm commit --amend [-m MSG]` | folds workdir into tip (same parents; old tip unreferenced) |
| `git checkout -b N` | `lrm checkout -b N` | create + switch in one step |
| `git bisect` | `lrm bisect start BAD [GOOD]`, `good/bad/skip`, `run CMD`, `reset`, `log` | DAG-correct suspect-set narrowing; rides a scratch branch (no detached HEAD); exit 125 = skip |
| `git reflog` | `lrm ref-log [BRANCH]` | per-branch tip-move history (`reflog` stays the replication log); recovers amended/reset-away tips |
| `git archive` | `lrm archive [REF] -o FILE` | tar/tar.gz/zip, fully streaming, commit-timestamped entries |
| `git shortlog -sn` | `lrm shortlog [REF]` | commit counts per author |
| `git mv A B` | `lrm mv A B` | workdir rename (tracking follows automatically); refuses escapes/overwrites |
| `HEAD~N`, `REV^N` | same spellings | suffixes work in `show`/`diff`/`reset`/`describe`/`archive`/`blame`/`cherry-pick` — anywhere a ref resolves |
| `git rebase B` (+ `--continue`/`--abort`) | `lrm rebase B` (+ `--continue`/`--abort`) | linear-only replay on a scratch branch: the branch moves once, at the end (abort = pure restore); original messages kept; merge commits refuse; no-op when upstream is already contained |
| `git notes add/show/list` | `lrm notes add/show/list/remove REF` | local-only annotations (`refs/notes/<hex>`, never synced); `show` displays them |
| `git log --grep/--author` | `lrm log --grep S --author A` | case-insensitive substring filters in all three modes (full/`--oneline`/`--graph`) |
| `.gitignore` | `.lrmignore` | basename / `*` / anchored / `dir/` / `!` rules; honored by commit builds, `grep`, and `clean` (`-x` overrides); `.lrm`/`.git` always excluded |
| `git stash show/drop/clear` | `lrm stash show [--name-only]` / `drop [N]` / `clear` | show = parent-vs-shelf unified diff; drop/pop compact shelf numbers; `pop` still restores newest by default |
| `git gc` keeping reflog tips | `lrm gc` | ref-log old/new tips are reachability roots: amended/reset-away commits survive collection |
| `git branch -d/-D` | `lrm branch -d/-D NAME...` | safe delete refuses unmerged tips and the current branch; the ref-log keeps a deletion entry so the tip stays recoverable |
| `git stash apply` | `lrm stash apply [N]` | restore a shelf without dropping it (pop's non-destructive sibling) |
| `git rev-parse` | `lrm rev-parse [--short] [--abbrev-ref] REF...` | print resolved hashes; `--abbrev-ref` prints the branch name when one points at the tip |
| `git merge-base A B` | `lrm merge-base A B` | best common ancestor (the LCA resolver, scriptable) |
| `git cherry` | `lrm cherry UPSTREAM [HEAD]` | branch-unique commits oldest-first: `+` missing upstream, `-` already applied (content+subject equivalence) |
| `git check-ignore` | `lrm check-ignore [-v] PATH...` | test `.lrmignore` rules; `-v` prints `.lrmignore:LINE:PATTERN` per hit |
| `git cat-file -p H` | `lrm cat-file H` | prints blobs / manifests / trees / commits |
| `git fsck` | `lrm fsck` | verifies CAS reachability from all refs (compat layer) |
| `git gc` | `lrm gc` | drops objects unreachable from refs + ref-log tips (compat layer) |
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
