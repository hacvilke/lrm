# LRM Automation — LRS scripts & LRQ queries in practice

LRS (`.lr`) is the imperative runtime language for automating sharing and
testing network behavior; LRQ (`.lrq`) is its read-only declarative twin
for asking a repo questions. Full syntax: [LRSPEC.md](LRSPEC.md) /
[LRQSPEC.md](LRQSPEC.md). This page is the cookbook — the things you'd
actually type.

Every run exports a **timestamped `.txt` report** (output, logs, network
events, assertion results, verdict). Keep the reports in the repo and the
audit trail syncs with everything else.

```sh
lrm run check.lr                       # bare form works too
lrm run check.lr --report out.txt -- arg1 arg2
lrm query repo.lrq
```

## Recipe 1: is my friend's machine up?

```js
// ping.lr — probe a teammate's daemon before a long sync
let t = tcp_connect("10.0.0.4", 8443, 2000);
assert(t.ok, "maya's daemon reachable");
let st = stun_ip();
print("we are reachable at " + str(st.ip) + " — share the key from there");
```

## Recipe 2: the full share → join roundtrip, scripted

The most useful automation: one script mints access for a teammate.

```js
// share_team.lr — run inside a repo served by `lrm daemon`
let s = lrm_status();                 // {ok, branch, tip, root, clean, port, ...}
if s.clean == false {
  let c = lrm_commit("wip");          // commit pending work first
  assert(c.ok, "commit ok");
}
let key = lrm_share(s.port, true);    // STUN + UPnP; s.port is the repo's port
assert(key.ok and key.listening, "key minted, daemon listening");
print("KEY: " + key.key);             // send this over any channel you trust
```

On the teammate's machine, the matching join:

```js
// join.lr — lrm run join.lr -- lrm1_...
let j = lrm_join(args()[0]);
assert(j.ok, "join ok: " + str(j.message));
assert(contains(read_file("README.md").text, "lrm"), "repo materialized");
print("fetched " + str(j.fetched) + " object(s)");
```

## Recipe 3: sync with everyone, verify convergence

```js
// rounds.lr — two rounds against every LAN peer
for i in [1, 2] {
  let s = lrm_sync("");               // "" = discover LAN peers
  if s.ok {
    print("round " + str(i) + ": fetched=" + str(s.fetched) + " pushed=" + str(s.pushed));
    sleep_ms(1000);
  } else {
    print("round " + str(i) + " failed: " + str(s.error));
  }
}
let s2 = lrm_sync("");
assert(s2.ok and s2.fetched == 0, "converged — nothing left to fetch");
```

Against a specific peer, pass the address: `lrm_sync("10.0.0.4:8443")`.

## Recipe 4: site / service health checks (with a friend's node)

```js
// check_site.lr — lrm run check_site.lr -- https://our-sideproject.dev
let r = http_get(args()[0], 10000);
assert(r.ok and r.status == 200, "site up");
assert(r.latency_ms < 800, "fast enough: " + str(r.latency_ms) + "ms");
let d = dns_lookup("our-sideproject.dev");
assert(d.ok and len(d.ips) > 0, "dns resolves");
```

Run it from two different machines (yours and a teammate's) and you have
a two-vantage-point uptime check whose reports live in the repo.

## Recipe 5: watch a sync land in real time

On one terminal: `lrm watch`. On another, trigger a round:
`lrm run rounds.lr` — the watch terminal streams the mesh events
(`peer X live`, `send: received ...`, `[watch] auto-committed ...`) as
they happen. Great for demos and for debugging "why isn't it syncing".

## LRQ: asking the repo questions

```sh
lrm query status.lrq
```

```sql
status;                          -- branch, tip, root, clean
commits on "main" limit 10;      -- recent history, newest first
commits on "main" by "maya";     -- whose work landed
files at "main" bigger than 1MB; -- the heavy stuff
files at "main" path like "*.md" limit 20;
find "invoice" in "main";        -- FILENAME search (case-insensitive)
show main;                       -- full tip commit: hash, author, clock, tree
peers;                           -- LAN presence at query time
```

Handy one-off (no file needed for quick looks — write the .lrq once,
query forever):

```sql
-- deadline.lrq: what changed this week and by whom?
commits on "main" limit 50;
files at "main" bigger than 100KB;
```

The rendered table lands in the terminal AND in a `.txt` report — paste
that into the team chat when someone asks "what's in the repo right now?".

## Rules of thumb

- **Scripts fail gracefully**: every network/repo builtin returns
  `{ok:false, error}` instead of crashing — wrap in `assert()` and the
  report carries the verdict.
- **`args()`** is how scripts take parameters (`lrm run x.lr -- arg`),
  so one script serves many peers/URLs.
- **Reports are the product**: the point of LRS is the auditable `.txt`
  trail — schedule runs, keep reports, diff them over time.
- **Timeouts are real**: `--timeout 30s` bounds the whole run, blocking
  calls included.
- Scripts are **sandboxed** to the run directory — a script can't read
  `../../etc/passwd`, by design.
