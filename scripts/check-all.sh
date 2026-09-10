#!/bin/bash
# check-all.sh — full LRM CLI surface sweep.
#
# Exercises EVERY command (core, mesh, device, git-compat, language)
# against real repos and a live daemon, printing PASS/FAIL per check.
# Exits 1 if anything failed. No external network required (LRM_SHARE_IP
# pins the key to loopback; STUN-dependent behavior is a soft check).
#
# Usage:  scripts/check-all.sh [path-to-lrm]     (default: builds ./lrm)
# Env:    KEEP=1  keep the scratch dir for debugging

set -u
LRM="${1:-}"
if [ -z "$LRM" ]; then
	echo "usage: $0 [path-to-lrm-binary]" >&2
	exit 2
fi
LRM="$(cd "$(dirname "$LRM")" && pwd)/$(basename "$LRM")"

PASS=0; FAIL=0; FAILED=""
SCRATCH="$(mktemp -d /tmp/lrm-checkall.XXXXXX)"
PORT=$(( (RANDOM % 200) + 8900 ))           # daemon port, unique-ish per run
cleanup() { [ "${KEEP:-0}" = "1" ] || rm -rf "$SCRATCH"; }
trap cleanup EXIT

ok()   { PASS=$((PASS+1)); echo "  ok   $1"; }
bad()  { FAIL=$((FAIL+1)); FAILED="$FAILED $1"; echo "  FAIL $1  -- $2"; }
# ck NAME EXPECT_SUBSTRING CMD...  (pass if rc==0 and output contains substring)
ck() {
	local name="$1" want="$2"; shift 2
	local out; out="$("$@" 2>&1)"; local rc=$?
	if [ $rc -ne 0 ]; then bad "$name" "rc=$rc: $(echo "$out" | tail -1)"
	elif [ -n "$want" ] && ! echo "$out" | grep -q "$want"; then bad "$name" "missing '$want' in: $(echo "$out" | tail -2)"
	else ok "$name"; fi
}
# ckfail NAME EXPECT_SUBSTRING CMD... (pass if rc!=0 and output mentions why)
ckfail() {
	local name="$1" want="$2"; shift 2
	local out; out="$("$@" 2>&1)"; local rc=$?
	if [ $rc -eq 0 ]; then bad "$name" "expected failure, got success"
	elif [ -n "$want" ] && ! echo "$out" | grep -q "$want"; then bad "$name" "missing '$want' in: $(echo "$out" | tail -2)"
	else ok "$name"; fi
}

echo "== LRM CLI surface sweep ($LRM, scratch $SCRATCH, port $PORT) =="

# ---------------------------------------------------------------- core ---
echo "-- core --"
cd "$SCRATCH"
export LRM_HOME="$SCRATCH/home"                    # one machine, one device id
mkdir proj && cd proj
ck "init"          "initialized"            "$LRM" init --user tester
ck "config set"    "port"                   "$LRM" config port "$PORT"
ck "config get"    "$PORT"                  "$LRM" config port
ck "config list"   "tester"                 "$LRM" config --list
echo "hello world" > a.txt
ck "status dirty"  "a.txt"                  "$LRM" status
ck "add"           "staged\|tracked\|added\|scanned" "$LRM" add -A
ck "commit"        "committed\|tip"         "$LRM" commit -m "first"
ck "status clean"  "clean"                  "$LRM" status
ck "log"           "first"                  "$LRM" log
ck "log --oneline" "first"                  "$LRM" log --oneline
ck "log --graph"   "\*"                     "$LRM" log --graph
ck "log --grep"    "first"                  "$LRM" log --grep first
ck "log --author"  "tester"                 "$LRM" log --author tester
TIP="$($LRM rev-parse --short HEAD 2>/dev/null || $LRM rev-parse HEAD)"
ck "rev-parse"     ""                       "$LRM" rev-parse HEAD
ck "rev-parse --short" ""                   "$LRM" rev-parse --short HEAD
ck "rev-parse --abbrev-ref" "main"          "$LRM" rev-parse --abbrev-ref HEAD
ck "show"          "hello world"            "$LRM" show
echo "hello v2" > a.txt
ck "diff"          "hello v2\|+hello"       "$LRM" diff
ck "commit --amend" "amended"               "$LRM" commit --amend -m "first (amended)"
ck "cat-file"      ""                       "$LRM" cat-file "$($LRM rev-parse HEAD)"
ck "replog"        ""                       "$LRM" replog
ck "blame"         "tester\|a.txt"          "$LRM" blame a.txt
ck "tag create"    "v1"                     "$LRM" tag v1
ck "describe"      "v1"                     "$LRM" describe
ck "shortlog"      "tester"                 "$LRM" shortlog
ck "grep"          "hello"                  "$LRM" grep hello
ck "grep -i -l"    "a.txt"                  "$LRM" grep -i -l HELLO
ck "version"       ""                       "$LRM" version
ck "help"          "usage\|Usage"           "$LRM" help

# ------------------------------------------------------------ git-compat ---
echo "-- git-compat --"
ck "tag list"      "v1"                     "$LRM" tag
ck "tag delete"    "deleted tag v1"         "$LRM" tag -d v1
ck "checkout -b"   "feature"                "$LRM" checkout -b feature
echo "feature work" > b.txt
ck "commit on branch" ""                    "$LRM" commit -m "feature work"
ck "branch list"   "feature"                "$LRM" branch
"$LRM" checkout main >/dev/null 2>&1
ck "merge cmd"     "fast-forward\|merged\|up to date" "$LRM" merge feature
ck "merge-base"    ""                       "$LRM" merge-base main feature
ck "cherry"        ""                       "$LRM" cherry main feature
"$LRM" checkout -b pickme >/dev/null 2>&1
echo pickme > pick.txt && "$LRM" commit -m "pick me" >/dev/null 2>&1
PICK="$($LRM rev-parse HEAD)"
"$LRM" checkout main >/dev/null 2>&1
ck "cherry-pick"   "cherry-pick\|picked\|applied" "$LRM" cherry-pick "$PICK"
ck "mv"            "renamed\|moved\|b2"     "$LRM" mv b.txt b2.txt
ck "commit mv"     ""                       "$LRM" commit -m "moved b"
printf 'build/\nsecret\n' > .lrmignore
ck "check-ignore"  "build/"                 "$LRM" check-ignore -v build/x.txt
ck "notes add"     ""                       "$LRM" notes add HEAD -m "reviewed"
ck "notes show"    "reviewed"               "$LRM" notes show HEAD
ck "notes list"    "reviewed"               "$LRM" notes list
ck "notes remove"  ""                       "$LRM" notes remove HEAD
ck "ref-log"       ""                       "$LRM" ref-log
ck "stash push"    "shelved\|stash"         sh -c "cd '$SCRATCH/proj' && echo wip > a.txt && '$LRM' stash push -m wip"
ck "stash list"    "wip"                    "$LRM" stash list
ck "stash show"    ""                       "$LRM" stash show
ck "stash apply"   "wip"                    "$LRM" stash apply
ck "stash drop"    ""                       "$LRM" stash drop
ck "stash clear"   ""                       "$LRM" stash clear
mkdir -p sub && echo junk > sub/junk.txt
ck "clean -n"      "junk\|would"            "$LRM" clean -n
ck "clean -f"      ""                       "$LRM" clean -f -d
ck "reset --hard"  "reset\|HEAD"            sh -c "cd '$SCRATCH/proj' && echo x >> a.txt && '$LRM' reset --hard HEAD"
ck "archive tar"   ""                       "$LRM" archive HEAD -o "$SCRATCH/a.tar" --format tar
ck "archive zip"   ""                       "$LRM" archive HEAD -o "$SCRATCH/a.zip" --format zip
[ -s "$SCRATCH/a.tar" ] && ok "archive tar bytes" || bad "archive tar bytes" "empty"
[ -s "$SCRATCH/a.zip" ] && ok "archive zip bytes" || bad "archive zip bytes" "empty"
# rebase: branch off, commit, rebase onto main
"$LRM" checkout -b rb main >/dev/null 2>&1
echo rb > rb.txt && "$LRM" commit -m "rb work" >/dev/null 2>&1
ck "rebase"        "rebas\|applied\|fast\|up to date" "$LRM" rebase main
"$LRM" checkout main >/dev/null 2>&1
# bisect: two commits, bad tip, good base
"$LRM" bisect reset >/dev/null 2>&1
echo bis > bis.txt && "$LRM" commit -m "good commit" >/dev/null 2>&1
GOOD="$($LRM rev-parse HEAD)"
echo broken > bis.txt && "$LRM" commit -m "bad commit" >/dev/null 2>&1
ck "bisect start"  "bad commit\|bisect"      "$LRM" bisect start HEAD "$GOOD"
ck "bisect log"    ""                       "$LRM" bisect log
ck "bisect reset"  ""                       "$LRM" bisect reset
ck "fsck"          ""                       "$LRM" fsck
ck "gc"            ""                       "$LRM" gc

# -------------------------------------------------------------- device ---
echo "-- device --"
cd "$SCRATCH"
INV="$("$LRM" pair 2>/dev/null | grep 'lrmpair1_' | tr -d ' ')"
[ -n "$INV" ] && ok "pair invite generated" || bad "pair invite generated" "no invite"
mkdir -p "$SCRATCH/home2"
ck "pair accept"   "paired"                 env LRM_HOME="$SCRATCH/home2" "$LRM" pair "$INV" buddy
ck "devices"       "buddy"                  env LRM_HOME="$SCRATCH/home2" "$LRM" devices
BUDDY_ID="$(LRM_HOME="$SCRATCH/home2" "$LRM" devices 2>/dev/null | grep 'buddy' | grep -oE '[0-9a-f]{8}' | head -1)"
ck "unpair"        "unpaired\|removed"      env LRM_HOME="$SCRATCH/home2" "$LRM" unpair "$BUDDY_ID"

# ----------------------------------------------------------------- mesh ---
echo "-- mesh --"
mkdir -p "$SCRATCH/carol" && cd "$SCRATCH/carol"
ck "init carol"    "initialized"            "$LRM" init --user carol
ck "carol port"    "$PORT"                  "$LRM" config port "$PORT"
echo "mesh data" > m.txt
"$LRM" commit -m "carol 1" >/dev/null 2>&1
( LRM_HOME="$SCRATCH/home" timeout 40 "$LRM" daemon --port "$PORT" >/tmp/checkall-daemon.log 2>&1 & )
sleep 2
ck "status daemon" "daemon: live"           "$LRM" status
KEY="$(LRM_SHARE_IP=127.0.0.1 "$LRM" share --no-upnp --no-natpmp 2>/dev/null | grep -oE 'lrm1_[A-Za-z0-9_-]+' | head -1)"
[ -n "$KEY" ] && ok "share (LRM_SHARE_IP)" || bad "share (LRM_SHARE_IP)" "no key"
mkdir -p "$SCRATCH/alfa" && cd "$SCRATCH/alfa"
ck "join --init"   "adopted\|sync complete" "$LRM" join "$KEY" --init
ck "join materialized" "mesh data"          cat "$SCRATCH/alfa/m.txt"
ck "sync --peer"   "fetched\|up to date"    "$LRM" sync --peer "127.0.0.1:$PORT"
ck "pull alias"    "fetched\|up to date"    "$LRM" pull --peer "127.0.0.1:$PORT"
ck "push alias"    "pushed\|up to date"     "$LRM" push --peer "127.0.0.1:$PORT"
ck "clone"         "cloned\|sync complete\|adopted" env -C "$SCRATCH" "$LRM" clone "$KEY" clonecopy
[ -f "$SCRATCH/clonecopy/m.txt" ] && ok "clone materialized" || bad "clone materialized" "no m.txt"
head -c 50000 /dev/urandom > "$SCRATCH/alfa/pic.bin"
ck "send"          "sent"                   "$LRM" send pic.bin --peer "127.0.0.1:$PORT"
sleep 3
cmp -s "$SCRATCH/alfa/pic.bin" "$SCRATCH/carol/inbox/pic.bin" && ok "send bytes identical" || bad "send bytes identical" "inbox mismatch"
( cd "$SCRATCH/carol" && timeout 5 "$LRM" watch >/tmp/checkall-watch.log 2>&1 & )
sleep 1
echo "watch test" > "$SCRATCH/carol/w.txt"
sleep 3
grep -q "watching daemon events" /tmp/checkall-watch.log && ok "watch stream" || bad "watch stream" "$(cat /tmp/checkall-watch.log | head -2)"
ckfail "send refused (stranger ws)" "workspace mismatch" sh -c "cd '$SCRATCH' && mkdir -p mallory && cd mallory && LRM_HOME='$SCRATCH/home' '$LRM' init --user mallory >/dev/null 2>&1; echo hi > evil.txt; '$LRM' send evil.txt --peer 127.0.0.1:$PORT"

# ------------------------------------------------------------ language ---
echo "-- language --"
mkdir -p "$SCRATCH/lang" && cd "$SCRATCH/lang"
ck "init lang"     "initialized"            "$LRM" init --user scripter
cat > check.lr <<'EOF'
let s = lrm_status();
assert(s.ok, "in repo");
let c = lrm_commit("via script");
assert(c.ok, "commit");
write_file("note.txt", "automation works");
assert(read_file("note.txt").ok, "files");
let t = tcp_connect("127.0.0.1", PORTNUM, 2000);
assert(t.ok, "daemon reachable from script");
print("lang ok: commit " + c.hash);
EOF
sed -i "s/PORTNUM/$PORT/" check.lr
ck "run LRS"       "lang ok\|verdict"       "$LRM" run check.lr
ck "LRS report"    ""                       sh -c "ls check_report_*.txt | head -1 | xargs grep -l 'verdict: PASS'"
printf 'status;\ncommits on "main" limit 3;\nfiles at "main";\n' > q.lrq
ck "query LRQ"     "commits"                "$LRM" query q.lrq
ck "bare .lr"      "lang ok"                "$LRM" check.lr
ck "bare .lrq"     "commits"                "$LRM" q.lrq

echo
echo "== sweep: $PASS passed, $FAIL failed =="
[ $FAIL -gt 0 ] && { echo "failed:$FAILED"; exit 1; }
echo "ALL GREEN"
