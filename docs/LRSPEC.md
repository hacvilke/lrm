# LRS — LRM Script language specification (v1)

LRS (`.lr` files) is the imperative runtime language for automating LRM
sharing and testing site/network behavior. Rust-flavored feel, easy syntax:

```sh
lrm run check.lr --report out.txt -- arg1 arg2   # explicit form
lrm check.lr                                     # bare form also works
```

Every run exports a **timestamped `.txt` report**: printed output,
timestamped logs, network events, assertion results, and — on failure —
the exact script error with line/col. Parse errors and runtime errors
also produce reports (verdict `FAIL`).

Implementation: tree-walk interpreter in `internal/lr/` (zero
dependencies, stdlib only). Scripts are network/repo-bound, so v1
optimizes for correctness and clear errors, not JIT speed.

## 1. Lexical rules

- Comments: `//` to end of line, or `/* ... */` blocks.
- Strings: `"..."` with `\"`, `\\`, `\n`, `\t` escapes.
- Numbers: one `num` type (float64); `18443`, `1.5` both num.
- Identifiers: `[A-Za-z_][A-Za-z0-9_]*`.
- Statement terminators: newline or `;` (both accepted, freely mixed).

## 2. Types

| Type | Literals | Notes |
|------|----------|-------|
| `null` | `null` | missing map keys yield `null` |
| `bool` | `true`, `false` | |
| `num` | `42`, `1.5` | prints as int when integral |
| `string` | `"hi"` | immutable, rune-indexed by `len()` |
| `list` | `[1, "a", true]` | heterogeneous, 0-indexed |
| `map` | `{"k": 1}` or `{k: 1}` | string keys; `m.k` ≡ `m["k"]` |
| `fn` | `fn(a) { return a; }` | first-class, closures capture |

**Truthiness:** only `null` and `false` are falsy. `0`, `""`, `[]`
are all truthy — one rule, no surprises.

## 3. Operators (precedence, high → low)

1. Unary: `-x`, `not x`
2. `*`, `/`, `%`
3. `+`, `-` (`+` concatenates when either side is a string)
4. `<`, `<=`, `>`, `>=`
5. `==`, `!=` (deep for lists/maps)
6. `in` (`"a" in ["a"]`, `"k" in {k:1}`, `"sub" in "string"`)
7. `and`
8. `or`
9. `=`: assignment (also `+=`, `-=`, `*=`, `/=`, `%=`)

`and`/`or` short-circuit and return the deciding operand's value.
Symbolic aliases `&&`, `||`, `!` work identically (`!x` ≡ `not x`).

## 4. Statements

```js
let x = 1;                 // declaration (also plain x = 1 after)
const PORT = 18443;        // constant (reassign = error)
if x > 1 { ... } else { ... }
while x < 10 { x += 1; }
for i in [1, 2, 3] { ... } // also over strings (chars) and maps (keys)
for k in {a: 1} { ... }
break;  continue;
fn add(a, b) { return a + b; }
return value;              // top-level return ends the program
```

`for` over a map iterates sorted keys. `break`/`continue` only inside
loops (parse-agnostic runtime error otherwise). Max call depth 500;
infinite loops are stopped by the run timeout (default 5 min) — and
blocking builtins (`sleep_ms`, network probes, `lrm_*` dials) cap their
own waits to the same deadline, so a single long call cannot outlive it.

## 5. Builtins (pure)

`print(...)`, `log(...)` (timestamped), `len(x)`, `str(x)`, `int(x)`,
`type(x)`, `split(s, sep)`, `join(list, sep)`, `contains(hay, needle)`,
`keys(map)`, `trim(s)`, `upper(s)`, `lower(s)`, `sleep_ms(n)`,
`now_ms()`, `args()` (argv after `--`).

## 6. Assertions: soft `assert`, hard `fail`

```js
assert(tcp.ok, "daemon reachable");  // records PASS/FAIL, CONTINUES
fail("aborting now");                // stops the script immediately
```

One run collects *every* assertion outcome; the verdict is `FAIL` if
any failed or any runtime error occurred. `assert` returns the bool so
`if assert(...) { }` also works.

## 7. Files (sandboxed to the run directory)

```js
write_file("sub/note.txt", "hi");  // {ok:true, bytes:N}
let r = read_file("sub/note.txt"); // {ok:true, text:"hi"}
file_exists("sub/note.txt");       // bool
```

Paths outside the sandbox (`../x`, `/etc/...`) are **blocked** and
return `{ok:false, error:...}` — never an exception. `read_file` caps
at 4 MiB (large files stay in the streaming engine, not in scripts).

## 8. Network probes (all logged as NET events in the report)

```js
tcp_connect("127.0.0.1", 18443, 2000);  // (host, port, timeout_ms)
dns_lookup("example.com");              // {ok, ips:[...], latency_ms}
http_get("https://example.com", 10000); // {ok, status, body, latency_ms}
stun_ip();                              // {ok, ip} — public IP via STUN
```

`tcp_connect` returns `{ok, latency_ms}` (latency present even on
failure). `http_get` caps bodies at 1 MiB.

### 8.1 Batch probes — many endpoints at once

The probes above run one at a time, because LRS evaluates sequentially.
For anything more than a handful of endpoints the cost is dominated by
waiting on the network, so these builtins fan the I/O out across a worker
pool inside Go and return results **in input order**:

```js
dns_lookup_all(hosts [, workers]);
// {ok, results:[{host, ok, ips, latency_ms}], count, failed, wall_ms}

tcp_scan(hosts, ports [, timeout_ms [, workers]]);
// probes every host x port combination
// {ok, results:[{host, port, ok, latency_ms}], open:[...],
//  open_count, closed_count, count, wall_ms}

http_get_all(urls [, timeout_ms [, workers]]);
// {ok, results:[{url, ok, status, bytes, latency_ms}], count, failed, wall_ms}

sort_by_latency(results);   // any list of result maps, fastest first
```

The script itself stays single-threaded — there is no `parallel for`, and
no interpreter state is shared across goroutines. That is deliberate: the
evaluator shares globals, the recorder and the output writer, and `Env` is
a plain map, so running loop bodies concurrently would be a data race and
the obvious accumulator pattern (`down = down + 1`) is exactly what would
break. Locking every variable access would serialise evaluation again and
buy nothing, because evaluation was never the slow part.

Workers default to 32 and are capped at 256. Both are bounded by the
script's overall timeout, and `http_get_all` caps each body at 1 MiB so a
large batch cannot exhaust memory.

The difference on work dominated by timeouts:

```js
// eight unreachable hosts, 1s timeout each
for h in dead { tcp_connect(h, 445, 1000); }   // 8003 ms
tcp_scan(dead, [445], 1000);                   // 1001 ms
```

Example — sweep a subnet and rank what answered:

```js
let hosts = ["10.0.0.1","10.0.0.2","10.0.0.3","10.0.0.4"];
let r = tcp_scan(hosts, [22, 80, 443, 8787], 1500);
print(str(r.open_count) + " open of " + str(r.count) + " in " + str(r.wall_ms) + "ms");
for o in sort_by_latency(r.open) {
  print("  " + o.host + ":" + str(o.port) + "  " + str(o.latency_ms) + "ms");
}
assert(r.open_count > 0, "something is listening on this subnet");
```

Every probe is still recorded individually as a NET event, so the exported
report contains the full per-endpoint trail plus a summary line.

## 9. LRM repo builtins (repo containing the run directory)

```js
lrm_status();          // {ok, branch, tip, root, clean, port, changes:[{kind,path}]}
lrm_commit("msg");     // {ok, hash} — scan→commit→index→save, like the CLI
lrm_log(10);           // {ok, commits:[{hash,author,time,message}]}
lrm_peers();           // {ok, peers:[{user,addr,id,source}]} — 4s LAN browse
lrm_share(18443, true);// {ok, key, human, wan, mapped, listening}
lrm_join("lrm1_...");  // {ok, fetched, pushed, message} — verified dial+sync
lrm_sync("");          // {ok, fetched, pushed, notes} — "" = discover LAN
lrm_sync("10.0.0.2:18443");
```

All mirror the CLI flows (same dial/sync/mapping code paths) and fail
gracefully as `{ok:false, error:...}` outside a repo or offline.

## 10. Reports

Default path: `<script>_report_<YYYYMMDD_HHMMSS>.txt` next to the
script (`--report` overrides). Contents: header (script, args, start,
duration), timestamped `INFO`/`OUT`/`LOG`/`NET`/`ASSERT`/`ERROR` lines,
assert totals, and `verdict: PASS|FAIL`.

Exit code: `0` on PASS, `1` on FAIL.

## 11. Examples

- `examples/check_site.lr` — probe a URL (DNS + HTTP + body match).
- `examples/share_team.lr` — commit work, mint a Port Key for a teammate.
