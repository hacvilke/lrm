# LRQ — LRM Query language specification (v1)

LRQ (`.lrq` files) is the declarative, **read-only** twin of LRS: eight
query kinds that inspect a repo and render aligned tables. Keywords are
case-insensitive; every query ends with `;`; `#` and `//` start comments.

```sh
lrm query repo.lrq --report out.txt   # explicit form
lrm repo.lrq                          # bare form also works
```

Every run prints the tables and exports a **timestamped `.txt` report**
(`<file>_query_<stamp>.txt`), including on errors (verdict `FAIL`).
LRQ can never modify a repo — there are no write statements.

Implementation: hand-written lexer/parser + direct store reads in
`internal/lrq/` (zero dependencies).

## Query kinds

```
peers [LIMIT N]                      LAN peers (4s browse, self excluded)
status                               branch, tip, root, clean + pending changes
branches                             all branches (* marks current)
tags                                 lightweight tags → targets
commits [ON ref] [LIMIT N]           history, newest first (default: 50)
files [AT ref] [PATH LIKE "glob*"] [BIGGER|SMALLER THAN size] [LIMIT N]
show ref                             one commit, vertical field layout
find "substr" [IN ref] [LIMIT N]     filename substring search (case-insensitive)
```

`FROM` prefix is accepted (`FROM COMMITS;`) and singular table names
work (`FROM COMMIT;`). `LIMIT` range is 1–500 (default 50).

## Refs

`ref` = branch name, tag name, full hash, or unique hash prefix.
Empty ref (bare `commits;`, `files;`, `find "x";`) means the current
branch tip. Resolution order: branch → tag → hash.

## Sizes

`BIGGER THAN` / `SMALLER THAN` take bytes with `B`/`KB`/`MB`/`GB`
suffixes (case-insensitive, floats allowed): `10KB`, `1.5MB`, `2GB`.
Comparisons are strict (`bigger than 5B` excludes exactly-5-byte files).

File sizes are **measured, never trusted**: each blob is stat'ed
through the CAS, and chunked files resolve via their manifest's
`TotalSize` (stored tree sizes are unreliable for merged trees).

`PATH LIKE` uses Go `path.Match` globs (`*`, `?`, `[...]`) matched
against the full repo-relative path.

## Tables

```
== files (2 row(s)) ==
path       size  hash
notes.txt  10B   fd3a8442
todo.md    5B    2396099c
```

Empty results render `(empty)` under the header instead of a bare
grid. `show` renders `field | value` rows (hash, author, peer, time,
clock, parents, tree, message).

## Reports

Header (query file, duration), one `== title ==` block per query,
`verdict: PASS|FAIL`, and `report: <path>` trailer. Exit code `0` on
PASS, `1` on FAIL (bad ref, no repo, syntax error — the report still
lands).

## Example

`examples/repo.lrq` exercises all eight kinds against the current repo.
