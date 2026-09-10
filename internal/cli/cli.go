// Package cli implements the `lrm` command-line interface.
package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/daemon"
	"github.com/lrm-project/lrm/internal/gitcompat"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/natpmp"
	"github.com/lrm-project/lrm/internal/node"
	"github.com/lrm-project/lrm/internal/patch"
	"github.com/lrm-project/lrm/internal/portkey"
	"github.com/lrm-project/lrm/internal/relay"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/stun"
	"github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
	"github.com/lrm-project/lrm/internal/upnp"
)

// Run dispatches argv (without program name). Returns exit code.
func Run(argv []string) int {
	if len(argv) == 0 {
		usage()
		return 2
	}
	cmd, args := argv[0], argv[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "status", "st":
		err = cmdStatus(args)
	case "commit", "ci":
		err = cmdCommit(args)
	case "log":
		err = cmdLog(args)
	case "show":
		err = cmdShow(args)
	case "diff":
		err = cmdDiff(args)
	case "branch", "br":
		err = cmdBranch(args)
	case "checkout", "co":
		err = cmdCheckout(args)
	case "merge":
		err = cmdMerge(args)
	case "peers":
		err = cmdPeers(args)
	case "share":
		err = cmdShare(args)
	case "join":
		err = cmdJoin(args)
	case "sync":
		err = cmdSync(args)
	case "daemon", "d":
		err = cmdDaemon(args)
	case "pair":
		err = cmdPair(args)
	case "devices":
		err = cmdDevices(args)
	case "unpair":
		err = cmdUnpair(args)
	case "cat-file", "cat":
		err = cmdCatFile(args)
	case "replog":
		err = cmdReplog(args)
	case "add":
		err = cmdAdd(args)
	case "push":
		err = cmdPush(args)
	case "pull", "fetch":
		err = cmdPull(args)
	case "clone":
		err = cmdClone(args)
	case "stash":
		err = cmdStash(args)
	case "notes":
		err = cmdNotes(args)
	case "reset":
		err = cmdReset(args)
	case "fsck":
		err = cmdFsck(args)
	case "gc":
		err = cmdGC(args)
	case "remote":
		err = cmdRemote(args)
	case "tag":
		err = cmdTag(args)
	case "blame":
		err = cmdBlame(args)
	case "cherry-pick", "pick":
		err = cmdPick(args)
	case "rebase":
		err = cmdRebase(args)
	case "grep":
		err = cmdGrep(args)
	case "config":
		err = cmdConfig(args)
	case "clean":
		err = cmdClean(args)
	case "describe":
		err = cmdDescribe(args)
	case "rev-parse":
		err = cmdRevParse(args)
	case "merge-base":
		err = cmdMergeBase(args)
	case "cherry":
		err = cmdCherry(args)
	case "check-ignore":
		err = cmdCheckIgnore(args)
	case "bisect":
		err = cmdBisect(args)
	case "ref-log":
		err = cmdRefLog(args)
	case "archive":
		err = cmdArchive(args)
	case "shortlog":
		err = cmdShortlog(args)
	case "mv":
		err = cmdMv(args)
	case "run":
		err = cmdRun(args)
	case "query", "q":
		err = cmdQuery(args)
	case "version", "-V", "--version":
		fmt.Println("lrm version 0.3.0")
	case "help", "-h", "--help":
		usage()
	default:
		// Bare script dispatch: `lrm check.lr` / `lrm repo.lrq`.
		switch {
		case strings.HasSuffix(cmd, ".lr"):
			err = cmdRun(append([]string{cmd}, args...))
		case strings.HasSuffix(cmd, ".lrq"):
			err = cmdQuery(append([]string{cmd}, args...))
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
			usage()
			return 2
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Println(`LRM — Log Replication Manager (P2P version control)

Usage: lrm <command> [options]

Local engine:
  init [path] [--user NAME] [--port PORT]   create a new repo
  status                                    show branch, tip, pending changes
  commit -m MSG                             version current workspace state
  log [--limit N] [--graph] [--oneline]     show history (graph = ASCII DAG)
  log --grep STR --author STR               filter history by message/author
  show [REF]                                show a commit + its patches (REF=head/branch/tag/hash)
  diff [REF1 [REF2]] [--stat]               unified patches (or file list)
  branch [--list] [NAME]                    list / create branches
  branch -d|-D NAME...                     delete branches (-D = unmerged too)
  checkout <BRANCH>                         switch branch (updates workdir)
  merge <BRANCH|HASH>                       merge into current branch
  cat-file <HASH>                           print a CAS object (small)
  replog [--last N]                         show replication log

P2P mesh:
  peers [--timeout SEC]                     discover LAN developers
  share [--port PORT] [--no-upnp] [--no-natpmp] [--lifetime SEC]
                                            map router port + print Port Key
  join <PORTKEY> [--init]                   connect via Port Key + sync
  sync [--peer HOST:PORT]                   sync with LAN peers (or one peer)
  daemon [--port PORT]                      run background engine (watch+sync)

Git-reverse compat (git spellings, P2P standing):
  add [paths...]                            confirm auto-tracked paths (no-op ritual)
  push [--peer HOST:PORT]                   sync out to peers (no force-push exists)
  pull | fetch [--peer HOST:PORT]            sync in from peers (bidir in one session)
  clone <PORTKEY> [dir]                     verified dial + full history + checkout
  stash [push|pop|apply|list|show|drop|clear]  shelf / restore workdir deltas
  reset [--soft|--mixed|--hard] <ref>      move branch ref (± index ± workdir)
  fsck                                      verify CAS reachability from all refs
  gc [--dry-run]                            prune unreachable objects
  remote -v                                 list LIVE peers (nothing to configure)
  tag [NAME [HASH]]                         list / create lightweight tags
  tag -d NAME                               delete a tag
  blame <FILE> [REF]                        line-by-line authorship (first-parent walk)
  cherry-pick <REF>                         replay a commit onto this branch (file-level 3-way)
  rebase <UPSTREAM> [--continue|--abort]    replay branch commits onto upstream (linear only)
  grep [-i] [-l] <PATTERN> [REF]            literal-substring search (workdir or history)
  config [--list] [user|port [VALUE]]       view / change identity settings
  clean [-n] [-f] [-d] [-x]                 list / delete untracked files (dry run by default)
  describe [REF]                            nearest tag name (<tag>[-N-g<short>])
  commit --amend [-m MSG]                   fold workdir state into the tip commit
  checkout -b NAME                          create a branch and switch to it
  bisect start <BAD> [GOOD...]              binary-search history (good|bad|skip|run|reset|log)
  ref-log [BRANCH] [--last N]               branch-tip move history (vs replog: replication log)
  archive [REF] -o FILE [--format F]        export snapshot (tar|tar.gz|zip, streaming)
  shortlog [REF] [--limit N]                commit counts per author
  mv <SRC> <DST>                            rename a workdir file (tracking is automatic)
  notes add|show|list|remove <REF>           local-only commit annotations (shown by show)
  rev-parse [--short] [--abbrev-ref] <REF>  print resolved commit hash(es)
  merge-base <A> <B>                       best common ancestor of two refs
  cherry <UPSTREAM> [HEAD]                 branch-unique commits (+ missing, - applied)
  check-ignore [-v] <PATH>...              test .lrmignore rules (-v shows the rule)

Scripting (LRS runtime + LRQ queries):
  run <SCRIPT.lr> [--report PATH] [--timeout 30s] [-- args...]
                                            execute a script, export .txt report
  query <QUERIES.lrq> [--report PATH]       run read-only repo queries
  <file.lr> | <file.lrq>                    bare form: lrm check.lr

Examples:
  lrm init --user alice
  lrm commit -m "first commit"
  lrm share                                  # prints a key for your teammate
  lrm join lrm1_...                          # teammate pastes your key
  lrm daemon                                 # real-time background sync`)
}

// --- helpers ---

func openRepo() (*store.Repo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return store.Open(cwd)
}

// resolveCommitRef maps HEAD/branch/tag/hash-prefix to a commit hash.
// Trailing ~N (Nth first-parent ancestor) and ^N (Nth parent) suffixes
// apply in order, git-style: HEAD~3, main^2, v1~1^2.
func resolveCommitRef(r *store.Repo, ref string) (cas.Hash, error) {
	base, suffixes := splitRefSuffix(ref)
	h, err := resolveCommitBase(r, base)
	if err != nil {
		return cas.Nil, err
	}
	for _, s := range suffixes {
		c, err := r.DAG.Get(h)
		if err != nil {
			return cas.Nil, err
		}
		switch s.op {
		case '~':
			for i := 0; i < s.n; i++ {
				if len(c.Parents) == 0 {
					return cas.Nil, fmt.Errorf("ref %q: no first parent %d step(s) back", ref, s.n)
				}
				h, err = cas.ParseHex(c.Parents[0])
				if err != nil {
					return cas.Nil, err
				}
				c, err = r.DAG.Get(h)
				if err != nil {
					return cas.Nil, err
				}
			}
		case '^':
			if s.n == 0 {
				break // rev^0 is rev itself (git parity)
			}
			if s.n < 1 || s.n > len(c.Parents) {
				return cas.Nil, fmt.Errorf("ref %q: parent ^%d out of range (%d parent(s))", ref, s.n, len(c.Parents))
			}
			h, err = cas.ParseHex(c.Parents[s.n-1])
			if err != nil {
				return cas.Nil, err
			}
		}
	}
	return h, nil
}

// refSuffix is one parsed ~N/^N step.
type refSuffix struct {
	op byte
	n  int
}

// splitRefSuffix splits "main~2^1" into ("main", [~2, ^1]).
// A bare ~ or ^ means 1. Stops at the first non-suffix char from the right.
func splitRefSuffix(ref string) (string, []refSuffix) {
	var suffixes []refSuffix
	rest := ref
	for len(rest) > 0 {
		// Peel [digits] then [~^] from the right.
		i := len(rest)
		for i > 0 && rest[i-1] >= '0' && rest[i-1] <= '9' {
			i--
		}
		digits := rest[i:]
		if i == 0 || (rest[i-1] != '~' && rest[i-1] != '^') {
			break
		}
		op := rest[i-1]
		rest = rest[:i-1]
		n := 1
		if digits != "" {
			fmt.Sscanf(digits, "%d", &n)
			if n < 0 {
				n = 0
			}
		}
		suffixes = append([]refSuffix{{op: op, n: n}}, suffixes...)
	}
	if rest == "" {
		rest = "HEAD" // "~3" alone means HEAD~3
	}
	return rest, suffixes
}

func resolveCommitBase(r *store.Repo, ref string) (cas.Hash, error) {
	if ref == "" || ref == "HEAD" {
		h, _, err := r.HeadCommit()
		if err != nil {
			return cas.Nil, err
		}
		if h == cas.Nil {
			return cas.Nil, fmt.Errorf("no commits yet")
		}
		return h, nil
	}
	if hexStr, _ := r.GetRef(ref); hexStr != "" {
		return cas.ParseHex(hexStr)
	}
	if hexStr, _ := r.GetTag(ref); hexStr != "" {
		return cas.ParseHex(hexStr)
	}
	h, err := r.CAS.Parse(ref)
	if err != nil {
		return cas.Nil, fmt.Errorf("unknown ref %q (no branch/tag/hash)", ref)
	}
	if !r.DAG.Has(h) {
		return cas.Nil, fmt.Errorf("ref %q is not a commit", ref)
	}
	return h, nil
}

func flagVal(args []string, names ...string) (string, []string) {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				val := args[i+1] // capture BEFORE splicing (append may clobber backing array)
				return val, spliceOut(args, i, i+2)
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"="), spliceOut(args, i, i+1)
			}
		}
	}
	return "", args
}

func hasFlag(args []string, names ...string) (bool, []string) {
	for i, a := range args {
		for _, n := range names {
			if a == n {
				return true, spliceOut(args, i, i+1)
			}
		}
	}
	return false, args
}

// spliceOut returns args minus [lo,hi), allocating a fresh slice so the
// caller's backing array is never mutated.
func spliceOut(args []string, lo, hi int) []string {
	out := make([]string, 0, len(args)-(hi-lo))
	out = append(out, args[:lo]...)
	out = append(out, args[hi:]...)
	return out
}

// --- local commands ---

func cmdInit(args []string) error {
	user, args := flagVal(args, "--user", "-u")
	portStr, args := flagVal(args, "--port", "-p")
	port := store.DefaultPort
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil {
			port = n
		}
	}
	path := "."
	if len(args) > 0 {
		path = args[0]
	}
	abs, _ := filepath.Abs(path)
	r, err := store.Init(abs, user, port)
	if err != nil {
		return err
	}
	defer r.Close()
	fmt.Printf("initialized empty LRM repo in %s/.lrm\n", abs)
	fmt.Printf("peer id: %s (short %s)\n", r.Identity.HexID(), r.Identity.ShortID())
	return nil
}

// cmdPair manages device pairing (works outside any repo):
//
//	lrm pair                    → print this machine's invite
//	lrm pair <invite> [name]    → pair with the machine behind the invite
func cmdPair(args []string) error {
	id, err := node.LoadOrCreateIdentity()
	if err != nil {
		return fmt.Errorf("device identity: %w", err)
	}
	book, err := node.LoadBook()
	if err != nil {
		return fmt.Errorf("address book: %w", err)
	}
	if len(args) == 0 {
		fmt.Printf("this device:  node %s\n", id.ShortID())
		fmt.Printf("paired:       %d device(s)\n", book.Count())
		fmt.Println()
		fmt.Println("Your pairing invite (send it over any channel you trust):")
		fmt.Println()
		fmt.Printf("  %s\n", node.Invite(id))
		fmt.Println()
		fmt.Println("On the OTHER machine run:  lrm pair <invite>")
		fmt.Println("then paste ITS invite back here the same way.")
		return nil
	}
	pub, err := node.VerifyInvite(args[0])
	if err != nil {
		return fmt.Errorf("bad invite: %w", err)
	}
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	peerID, err := book.Pair(pub, name)
	if err != nil {
		return err
	}
	fmt.Printf("paired ✓ device %s", peerID[:8])
	if name != "" {
		fmt.Printf(" (%s)", name)
	}
	fmt.Println()
	fmt.Println()
	fmt.Println("Complete the handshake — give THIS invite to the other machine:")
	fmt.Println()
	fmt.Printf("  %s\n", node.Invite(id))
	fmt.Println()
	fmt.Println("(run `lrm pair <invite above>` there; `lrm devices` lists everyone)")
	return nil
}

// cmdDevices lists the paired-device address book.
func cmdDevices(args []string) error {
	_ = args
	book, err := node.LoadBook()
	if err != nil {
		return err
	}
	entries := book.List()
	if id, err := node.LoadOrCreateIdentity(); err == nil {
		fmt.Printf("this device: node %s\n", id.ShortID())
	}
	if len(entries) == 0 {
		fmt.Println("no paired devices (see `lrm pair`)")
		return nil
	}
	fmt.Printf("%d paired device(s)\n", len(entries))
	for _, e := range entries {
		user := e.User
		if user == "" {
			user = "-"
		}
		last := "never"
		if !e.LastSeen.IsZero() {
			last = e.LastSeen.Format("2006-01-02 15:04")
		}
		addrs := "-"
		if len(e.Addrs) > 0 {
			addrs = strings.Join(e.Addrs, ", ")
		}
		fmt.Printf("  %-12s %s  last seen: %s  at: %s\n", user, e.PeerID()[:8], last, addrs)
	}
	return nil
}

// cmdUnpair removes a paired device by PeerID (or unique short prefix).
func cmdUnpair(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: lrm unpair <peer-id-prefix>")
	}
	book, err := node.LoadBook()
	if err != nil {
		return err
	}
	if err := book.Unpair(args[0]); err != nil {
		return err
	}
	fmt.Println("unpaired ✓")
	return nil
}

func cmdStatus(args []string) error {
	_ = args
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	br, _ := r.HeadBranch()
	tipHex, _ := r.GetRef(br)
	fmt.Printf("branch: %s\n", br)
	if tipHex == "" {
		fmt.Println("tip:    (unborn — no commits yet)")
	} else {
		fmt.Printf("tip:    %s\n", tipHex[:12])
	}
	fmt.Printf("peer:   %s (%s)\n", r.Identity.ShortID(), r.Config.User)
	if r.Config.Workspace != "" {
		fmt.Printf("workspace: %s\n", r.Config.Workspace)
	} else {
		fmt.Println("workspace: (unset — legacy repo; derived at first sync)")
	}
	if id, err := node.LoadOrCreateIdentity(); err == nil {
		if book, err := node.LoadBook(); err == nil {
			fmt.Printf("device: node %s (%d paired)\n", id.ShortID(), book.Count())
		}
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return err
	}
	fmt.Printf("root:   %s\n", cas.Hex(res.RootHash)[:12])
	if res.Clean {
		fmt.Println("status: clean (no changes)")
		printLiveDaemon(r.LrmDir)
		return nil
	}
	fmt.Printf("status: %d change(s):\n", len(res.Changes))
	for _, ch := range res.Changes {
		fmt.Printf("  %-8s %s\n", ch.Kind, ch.Path)
	}
	return nil
}

// daemonStatus holds the live-daemon presence snapshot.
type daemonStatus struct {
	OK        bool         `json:"ok"`
	User      string       `json:"user"`
	Workspace string       `json:"workspace"`
	Node      string       `json:"node"`
	UptimeSec int64        `json:"uptime_sec"`
	Peers     []daemonPeer `json:"peers"`
}

type daemonPeer struct {
	User  string `json:"user"`
	Peer  string `json:"peer"`
	Addr  string `json:"addr"`
	WS    string `json:"ws"`
	State string `json:"state"`
	Since string `json:"since"`
	RTTms int64  `json:"rtt_ms"`
}

// queryDaemon asks the running daemon over .lrm/daemon.sock.
func queryDaemon(lrmDir, cmd string) *daemonStatus {
	path := filepath.Join(lrmDir, "daemon.sock")
	conn, err := net.DialTimeout("unix", path, 700*time.Millisecond)
	if err != nil {
		return nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := conn.Write([]byte(`{"cmd":"` + cmd + `"}` + "\n")); err != nil {
		return nil
	}
	raw, err := io_readLine(conn)
	if err != nil {
		return nil
	}
	var st daemonStatus
	if err := json.Unmarshal(raw, &st); err != nil || !st.OK {
		return nil
	}
	return &st
}

func io_readLine(conn net.Conn) ([]byte, error) {
	var buf []byte
	b := make([]byte, 1)
	for len(buf) < 1<<20 {
		n, err := conn.Read(b)
		if n > 0 {
			buf = append(buf, b[:n]...)
			if b[n-1] == '\n' {
				return buf, nil
			}
		}
		if err != nil {
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
	}
	return buf, nil
}

// printLiveDaemon renders the live presence table for `lrm status`.
func printLiveDaemon(lrmDir string) {
	st := queryDaemon(lrmDir, "status")
	if st == nil {
		fmt.Println("daemon: not running (run `lrm daemon` to go live)")
		return
	}
	ups := st.UptimeSec
	upStr := fmt.Sprintf("%dd%02dh%02dm", ups/86400, ups%86400/3600, ups%3600/60)
	fmt.Printf("daemon: live (uptime %s)\n", upStr)
	if len(st.Peers) == 0 {
		fmt.Println("  no peers connected")
		return
	}
	fmt.Printf("  %-10s %-22s %-8s %6s  %s\n", "peer", "addr", "state", "rtt", "since")
	for _, p := range st.Peers {
		user := p.User
		if user == "" {
			if len(p.Peer) >= 8 {
				user = p.Peer[:8]
			}
		}
		rtt := "-"
		if p.RTTms > 0 {
			rtt = fmt.Sprintf("%dms", p.RTTms)
		}
		since := p.Since
		if len(since) >= 19 {
			since = since[11:19] // HH:MM:SS of the RFC3339 timestamp
		}
		fmt.Printf("  %-10s %-22s %-8s %6s  %s\n", user, p.Addr, p.State, rtt, since)
	}
}

func cmdCommit(args []string) error {
	msg, args := flagVal(args, "-m", "--message")
	amend, args := hasFlag(args, "--amend")
	_ = args
	if msg == "" && !amend {
		return fmt.Errorf("commit message required: lrm commit -m \"msg\"")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if amend {
		h, err := gitcompat.Amend(r, msg)
		if err != nil {
			return err
		}
		fmt.Printf("amended tip → %s\n", cas.Hex(h)[:12])
		return nil
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return err
	}
	if res.Clean {
		fmt.Println("nothing to commit (working dir clean)")
		return nil
	}
	h, c, err := r.Commit(msg, res.RootHash)
	if err != nil {
		return err
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	_ = r.Index.Save()
	fmt.Printf("committed %s (%d file(s), clock %s)\n", cas.Hex(h)[:12], len(res.Changes), c.Clock.String())
	for _, ch := range res.Changes {
		fmt.Printf("  %-8s %s\n", ch.Kind, ch.Path)
	}
	return nil
}

func cmdLog(args []string) error {
	graph, args := hasFlag(args, "--graph")
	oneline, args := hasFlag(args, "--oneline")
	grepStr, args := flagVal(args, "--grep")
	authorStr, args := flagVal(args, "--author")
	limitStr, _ := flagVal(args, "--limit", "-n")
	grepNeedle := strings.ToLower(grepStr)
	authorNeedle := strings.ToLower(authorStr)
	matchLog := func(msg, author string) bool {
		return matchLogFilter(msg, author, grepNeedle, authorNeedle)
	}
	limit := 20
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	tip, br, err := r.HeadCommit()
	if err != nil {
		return err
	}
	if tip == cas.Nil {
		fmt.Println("(no commits yet)")
		return nil
	}
	if graph || oneline {
		order, lookup, err := GraphOrder(r, tip, limit)
		if err != nil {
			return err
		}
		if oneline && !graph {
			for _, h := range order {
				msg, author := "(missing)", ""
				if c := lookup[h]; c != nil {
					msg, author = firstLine(c.Message), c.Author
				}
				if !matchLog(msg, author) {
					continue
				}
				fmt.Printf("%s %s\n", cas.Short(h), msg)
			}
			return nil
		}
		if grepStr != "" || authorStr != "" {
			kept := make([]cas.Hash, 0, len(order))
			for _, h := range order {
				msg, author := "(missing)", ""
				if c := lookup[h]; c != nil {
					msg, author = firstLine(c.Message), c.Author
				}
				if matchLog(msg, author) {
					kept = append(kept, h)
				}
			}
			order = kept
		}
		fmt.Printf("history of %s:\n", br)
		for _, row := range RenderGraph(order, lookup) {
			fmt.Println(row)
		}
		return nil
	}
	hashes, commits, err := r.DAG.WalkTipOrder(tip, limit)
	if err != nil {
		return err
	}
	fmt.Printf("history of %s:\n", br)
	for i, c := range commits {
		if !matchLog(c.Message, c.Author) {
			continue
		}
		fmt.Printf("\ncommit %s\n", cas.Hex(hashes[i]))
		fmt.Printf("Author: %s (peer %s)\n", c.Author, shortHex(c.PeerHex))
		fmt.Printf("Date:   %s\n", time.Unix(0, c.Timestamp).Format(time.RFC3339))
		fmt.Printf("Clock:  %s\n", c.Clock.String())
		if len(c.Parents) > 0 {
			fmt.Printf("Parents: %s\n", strings.Join(shortList(c.Parents), " "))
		}
		fmt.Printf("\n    %s\n", c.Message)
	}
	return nil
}

func cmdShow(args []string) error {
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	var h cas.Hash
	if len(args) > 0 {
		hh, err := resolveCommitRef(r, args[0])
		if err != nil {
			return err
		}
		h = hh
	} else {
		tip, _, err := r.HeadCommit()
		if err != nil {
			return err
		}
		if tip == cas.Nil {
			return fmt.Errorf("no commits yet")
		}
		h = tip
	}
	c, err := r.DAG.Get(h)
	if err != nil {
		return err
	}
	fmt.Printf("commit %s\n", cas.Hex(h))
	fmt.Printf("Author: %s (peer %s)\n", c.Author, shortHex(c.PeerHex))
	fmt.Printf("Date:   %s\n", time.Unix(0, c.Timestamp).Format(time.RFC3339))
	fmt.Printf("Tree:   %s\n", c.Tree)
	fmt.Printf("Message: %s\n", c.Message)
	if note, err := gitcompat.NoteShow(r, h); err == nil {
		fmt.Printf("Notes:\n    %s\n", strings.ReplaceAll(note, "\n", "\n    "))
	}
	flat := map[string]string{}
	th, _ := cas.ParseHex(c.Tree)
	_ = merkle.Flatten(r.CAS, th, "", flat)
	fmt.Printf("Files (%d):\n", len(flat))
	paths := sortedKeys(flat)
	for _, p := range paths {
		fmt.Printf("  %s  %s\n", flat[p][:8], p)
	}
	// Patches vs first parent (git-show style).
	var oldRoot cas.Hash // Nil = initial commit
	switch len(c.Parents) {
	case 0:
		fmt.Println("\n(initial commit)")
	case 1:
		ph, err := cas.ParseHex(c.Parents[0])
		if err != nil {
			return nil
		}
		pc, err := r.DAG.Get(ph)
		if err != nil {
			fmt.Printf("\n(parent %s not fetched yet — patches unavailable)\n", shortHex(c.Parents[0]))
			return nil
		}
		oldRoot, _ = cas.ParseHex(pc.Tree)
	default:
		fmt.Printf("\n(merge of %d parents — use `lrm diff <parent> %s` for patches)\n", len(c.Parents), cas.Short(h))
		return nil
	}
	fps, err := patch.Build(r, oldRoot, th, 0)
	if err != nil {
		return err
	}
	for _, fp := range fps {
		switch fp.Kind {
		case "binary", "large":
			fmt.Printf("\n--- a/%s\n+++ b/%s\n(%s: %s)\n", fp.Path, fp.Path, fp.Kind, fp.Note)
		default:
			fmt.Printf("\n%s", fp.Patch.Unified())
		}
	}
	return nil
}

func cmdDiff(args []string) error {
	statOnly, args := hasFlag(args, "--stat")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	var oldRoot, newRoot cas.Hash
	switch len(args) {
	case 0:
		// working dir vs tip
		tip, _, _ := r.HeadCommit()
		if tip != cas.Nil {
			c, _ := r.DAG.Get(tip)
			oldRoot, _ = cas.ParseHex(c.Tree)
		}
		res, err := r.Index.Scan(r.Root, r.CAS)
		if err != nil {
			return err
		}
		newRoot = res.RootHash
	case 1:
		h, err := resolveCommitRef(r, args[0])
		if err != nil {
			return err
		}
		c, err := r.DAG.Get(h)
		if err != nil {
			return err
		}
		oldRoot, _ = cas.ParseHex(c.Tree)
		res, err := r.Index.Scan(r.Root, r.CAS)
		if err != nil {
			return err
		}
		newRoot = res.RootHash
	default:
		h1, err := resolveCommitRef(r, args[0])
		if err != nil {
			return err
		}
		h2, err := resolveCommitRef(r, args[1])
		if err != nil {
			return err
		}
		c1, err := r.DAG.Get(h1)
		if err != nil {
			return err
		}
		c2, err := r.DAG.Get(h2)
		if err != nil {
			return err
		}
		oldRoot, _ = cas.ParseHex(c1.Tree)
		newRoot, _ = cas.ParseHex(c2.Tree)
	}
	if statOnly {
		changes, err := merkle.Diff(r.CAS, oldRoot, newRoot)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			fmt.Println("no differences")
			return nil
		}
		for _, ch := range changes {
			fmt.Printf("%-8s %s\n", ch.Kind, ch.Path)
		}
		return nil
	}
	fps, err := patch.Build(r, oldRoot, newRoot, 0)
	if err != nil {
		return err
	}
	if len(fps) == 0 {
		fmt.Println("no differences")
		return nil
	}
	for _, fp := range fps {
		switch fp.Kind {
		case "binary", "large":
			fmt.Printf("diff --lrm a/%s b/%s\n", fp.Path, fp.Path)
			fmt.Printf("(%s: %s)\n", fp.Kind, fp.Note)
		default:
			fmt.Printf("diff --lrm a/%s b/%s\n", fp.Path, fp.Path)
			fmt.Printf("%s", fp.Patch.Unified())
		}
	}
	return nil
}

func cmdBranch(args []string) error {
	list, args := hasFlag(args, "--list", "-l")
	delSafe, args := hasFlag(args, "-d", "--delete")
	delForce, args := hasFlag(args, "-D")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if delSafe || delForce {
		if len(args) == 0 {
			return fmt.Errorf("usage: lrm branch -d|-D <name>...")
		}
		var failed int
		var firstErr error
		for _, name := range args {
			res, err := gitcompat.DeleteBranch(r, name, delForce)
			if err != nil {
				if len(args) > 1 {
					fmt.Printf("error: %v\n", err)
				} else {
					firstErr = err
				}
				failed++
				continue
			}
			was := "(was unborn)"
			if res.Tip != "" {
				was = fmt.Sprintf("(was %s)", res.Tip[:12])
			}
			fmt.Printf("deleted branch %s %s\n", name, was)
		}
		if failed > 0 {
			if firstErr != nil {
				return firstErr
			}
			return fmt.Errorf("could not delete %d branch(es)", failed)
		}
		return nil
	}
	cur, _ := r.HeadBranch()
	if list || len(args) == 0 {
		branches, _ := r.ListBranches()
		if len(branches) == 0 {
			fmt.Println("(no branches yet — commit first)")
			return nil
		}
		for _, b := range branches {
			mark := " "
			if b == cur {
				mark = "*"
			}
			tip, _ := r.GetRef(b)
			short := "(unborn)"
			if len(tip) >= 12 {
				short = tip[:12]
			}
			fmt.Printf("%s %-24s %s\n", mark, b, short)
		}
		return nil
	}
	name := args[0]
	if name == "" {
		return fmt.Errorf("branch name required")
	}
	tipHex, _ := r.GetRef(cur)
	oldHex, _ := r.GetRef(name)
	if err := r.SetRef(name, tipHex); err != nil {
		return err
	}
	r.AppendReflog(name, oldHex, tipHex, "branch", "created at "+shortOrUnborn(tipHex))
	fmt.Printf("created branch %s at %s\n", name, shortOrUnborn(tipHex))
	return nil
}

func cmdCheckout(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm checkout [-b] <branch>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if args[0] == "-b" {
		if len(args) != 2 || args[1] == "" {
			return fmt.Errorf("usage: lrm checkout -b <new-branch>")
		}
		name := args[1]
		if tipHex, _ := r.GetRef(name); tipHex != "" {
			return fmt.Errorf("branch %q already exists", name)
		}
		if branches, _ := r.ListBranches(); branches != nil {
			for _, b := range branches {
				if b == name {
					return fmt.Errorf("branch %q already exists", name)
				}
			}
		}
		curTip, _, err := r.HeadCommit()
		if err != nil {
			return err
		}
		if curTip != cas.Nil {
			if err := r.SetRef(name, cas.Hex(curTip)); err != nil {
				return err
			}
			r.AppendReflog(name, "", cas.Hex(curTip), "branch", "created via checkout -b")
		}
		if err := r.SetHeadBranch(name); err != nil {
			return err
		}
		fmt.Printf("created and switched to branch %s\n", name)
		return nil
	}
	name := args[0]
	tipHex, _ := r.GetRef(name)
	// Allow checkout of unborn branch only if it exists or tip empty.
	branches, _ := r.ListBranches()
	exists := false
	for _, b := range branches {
		if b == name {
			exists = true
		}
	}
	if !exists && tipHex == "" {
		// Check it's not a commit hash.
		if h, err := r.CAS.Parse(name); err == nil && r.DAG.Has(h) {
			return fmt.Errorf("detached checkout not supported in sprint build; create a branch first")
		}
		return fmt.Errorf("branch %q does not exist", name)
	}
	if err := r.SetHeadBranch(name); err != nil {
		return err
	}
	if tipHex != "" {
		h, _ := cas.ParseHex(tipHex)
		if err := sync.Checkout(r, h); err != nil {
			return err
		}
	}
	fmt.Printf("switched to branch %s\n", name)
	return nil
}

func cmdMerge(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm merge <branch|hash>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	target := args[0]
	h, err := resolveCommitRef(r, target)
	if err != nil {
		return err
	}
	res, err := sync.MergeLocal(r, h, target)
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	if res.MergedCommit != "" {
		fmt.Printf("merge commit: %s\n", res.MergedCommit[:12])
	}
	return nil
}

func cmdCatFile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm cat-file <hash>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	h, err := r.CAS.Parse(args[0])
	if err != nil {
		return err
	}
	raw, err := r.CAS.GetBytes(h, 4<<20)
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return fmt.Errorf("object too large to print (%d bytes)", len(raw))
	}
	fmt.Printf("%s", raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func cmdReplog(args []string) error {
	nStr, _ := flagVal(args, "--last", "-n")
	n := 20
	if nStr != "" {
		if v, err := strconv.Atoi(nStr); err == nil && v > 0 {
			n = v
		}
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	ents, err := r.Replog.ReadLast(n)
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		fmt.Println("(empty replication log)")
		return nil
	}
	for _, e := range ents {
		fmt.Printf("#%-4d [%s] %s peer=%s commit=%s %s\n",
			e.Seq, time.Unix(0, e.Time).Format(time.RFC3339), e.Type, shortHex(e.PeerHex), shortHex(e.Commit), e.Message)
	}
	return nil
}

// --- P2P commands ---

func cmdPeers(args []string) error {
	tStr, _ := flagVal(args, "--timeout", "-t")
	secs := 5
	if tStr != "" {
		if n, err := strconv.Atoi(tStr); err == nil && n > 0 {
			secs = n
		}
	}
	fmt.Printf("discovering LAN peers for %ds...\n", secs)
	if r, err := openRepo(); err == nil {
		if st := queryDaemon(r.LrmDir, "status"); st != nil {
			fmt.Printf("daemon: live — %d peer(s) connected\n", len(st.Peers))
		}
		_ = r.Close()
	}
	ctx := context.Background()
	peers, err := mdns.Browse(ctx, time.Duration(secs)*time.Second)
	if err != nil {
		return err
	}
	// Dedupe self if in a repo.
	self := ""
	localWS := ""
	if r, err := openRepo(); err == nil {
		self = r.Identity.HexID()
		localWS = r.Config.Workspace
		_ = r.Close()
	}
	// Tag peers with their workspace so foreign projects on shared Wi-Fi
	// are visible at a glance.
	var book *node.Book
	if b, err := node.LoadBook(); err == nil {
		book = b
	}
	shown := 0
	for _, p := range peers {
		if p.PeerHex == self {
			continue
		}
		tag := "-"
		switch {
		case p.WS == "":
			tag = "legacy"
		case p.WS == localWS:
			tag = "same"
		default:
			tag = "other"
		}
		dev := ""
		if book != nil && p.NodeHex != "" && book.Get(p.NodeHex) != nil {
			dev = " dev✓"
		}
		fmt.Printf("  %-8s %-16s ws:%-8s %s%s (%s)\n", p.User, p.Addr(), wsTag(p.WS), tag, dev, p.Source)
		shown++
	}
	if shown == 0 {
		fmt.Println("no peers found (are teammates running `lrm daemon` on this Wi-Fi?)")
	}
	return nil
}

// wsTag shortens a workspace id for display.
func wsTag(ws string) string {
	if len(ws) >= 8 {
		return ws[:8]
	}
	return ws
}

// cmdShare implements the Automated Port Forwarding Pipeline:
//
//	[Start] → UPnP → (fail) NAT-PMP/PCP → (fail) manual fallback
//	→ Fetch Public IP (STUN) → Generate LRM Key
func cmdShare(args []string) error {
	portStr, args := flagVal(args, "--port", "-p")
	lifeStr, args := flagVal(args, "--lifetime")
	noUPnP, args := hasFlag(args, "--no-upnp")
	noPMP, _ := hasFlag(args, "--no-natpmp", "--no-pmp")
	_ = args
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	port := r.Config.Port
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil {
			port = n
		}
	}
	lifetime := 3600
	if lifeStr != "" {
		if n, err := strconv.Atoi(lifeStr); err == nil && n >= 60 {
			lifetime = n
		}
	}
	fmt.Printf("LRM share — opening WAN port %d (user %s, peer %s)\n", port, r.Config.User, r.Identity.ShortID())
	// Probe early (cheap): a Port Key is useless if nobody accepts the dial.
	served := isListening(port)

	mappedPort := uint16(port)
	var publicIP []byte
	var cleanup []func()
	defer func() {
		// NOTE: share keeps mappings alive (lease-based). Cleanup funcs run
		// only if we fail before printing the key.
		_ = cleanup
	}()

	// Step A: UPnP.
	upnpOK := false
	var gw *upnp.Gateway
	if !noUPnP {
		fmt.Print("[1/3] Query Local Gateway via UPnP... ")
		g, err := upnp.DiscoverGateway(4 * time.Second)
		if err != nil {
			fmt.Printf("FAIL (%v)\n", shortErr(err))
		} else if err := g.AddPortMapping(port, port, lanIP(), "TCP", "LRM P2P :"+r.Config.User, lifetime); err != nil {
			fmt.Printf("FAIL (%v)\n", shortErr(err))
		} else {
			fmt.Println("OK (port mapped on router)")
			upnpOK = true
			gw = g
			cleanup = append(cleanup, func() { _ = g.DeletePortMapping(port, "TCP") })
		}
	} else {
		fmt.Println("[1/3] UPnP skipped (--no-upnp)")
	}
	// Step B: NAT-PMP / PCP.
	pmpOK := false
	var pmpGW []byte
	var pmpExt uint16
	if !upnpOK && !noPMP {
		fmt.Print("[2/3] Query via NAT-PMP / PCP... ")
		gwIP, err := natpmp.DiscoverGateway()
		if err != nil {
			fmt.Printf("FAIL (%v)\n", shortErr(err))
		} else if res, err := natpmp.MapTCP(gwIP, uint16(port), uint16(port), uint32(lifetime)); err != nil {
			fmt.Printf("FAIL (%v)\n", shortErr(err))
		} else {
			fmt.Printf("OK (external port %d via %s)\n", res.ExternalPort, gwIP.String())
			pmpOK = true
			pmpGW = gwIP
			pmpExt = res.ExternalPort
			mappedPort = res.ExternalPort
		}
	} else if upnpOK {
		fmt.Println("[2/3] NAT-PMP skipped (UPnP succeeded)")
	} else {
		fmt.Println("[2/3] NAT-PMP skipped (--no-natpmp)")
	}
	if !upnpOK && !pmpOK {
		fmt.Println("[2/3] Fallback to Manual Port — automatic mapping failed.")
		fmt.Printf("      Please manually forward port %d on your router, or use a relay.\n", port)
	}
	_ = pmpGW
	_ = pmpExt
	// Step C: public IP.
	fmt.Print("[3/3] Fetch Public IP (STUN)... ")
	if upnpOK && gw != nil {
		if ip, err := gw.ExternalIP(); err == nil && ip != nil {
			publicIP = ip
			fmt.Printf("OK (%s via UPnP)\n", ip.String())
		}
	}
	if publicIP == nil {
		ip, err := stun.DiscoverPublicIP(nil, 5*time.Second)
		if err != nil || ip == nil {
			fmt.Printf("FAIL (%v)\n", shortErr(err))
			fmt.Println()
			fmt.Printf("Automatic port mapping failed. Please manually forward port %d or use a fallback relay.\n", port)
			fmt.Printf("Your LAN addresses: %s (share these + port for same-WiFi join)\n", strings.Join(mdns.LocalAddrs(), ", "))
			if !served {
				printServeWarning(port)
			}
			return fmt.Errorf("could not determine public IP; no Port Key generated")
		}
		publicIP = ip
		fmt.Printf("OK (%s via STUN)\n", ip.String())
	}
	// Generate key.
	key := portkey.Generate(r.Identity.Pub, publicIP, mappedPort, wsBytes(r.Config.Workspace))
	fmt.Println()
	fmt.Println("Share this Port Key with your teammate (it expires with the lease):")
	fmt.Println()
	fmt.Printf("  %s\n", key.Encode())
	fmt.Println()
	fmt.Printf("Human form: %s\n", key.Human())
	fmt.Println()
	fmt.Println("Teammate runs:  lrm join <key>")
	fmt.Printf("Lease: %ds. Mappings auto-expire; run `lrm daemon` to keep them renewed.\n", lifetime)
	if !upnpOK && !pmpOK {
		fmt.Printf("WARNING: no router mapping — ensure port %d is forwarded or the peer cannot dial you.\n", port)
	}
	// Safety: a Port Key is useless if nobody accepts the dial. The share
	// port must be served (normally by `lrm daemon` in this repo).
	if served {
		fmt.Printf("Local listener detected on port %d ✓ (peers can dial now)\n", port)
	} else {
		printServeWarning(port)
	}
	return nil
}

// printServeWarning tells the user their share port has no listener.
func printServeWarning(port int) {
	fmt.Println()
	fmt.Printf("⚠  NOBODY IS LISTENING on port %d on THIS machine.\n", port)
	fmt.Println("   Your teammate's dial will FAIL until you serve this repo:")
	fmt.Printf("       lrm daemon --port %d   (in another terminal, same repo)\n", port)
	fmt.Println("   The daemon also renews the router lease automatically.")
}

// isListening probes whether something accepts TCP on localhost:port.
func isListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// wsBytes decodes a hex workspace ID ("" → nil, for v1 keys).
func wsBytes(wsHex string) []byte {
	if wsHex == "" {
		return nil
	}
	b, err := hex.DecodeString(wsHex)
	if err != nil || len(b) != 16 {
		return nil
	}
	return b
}

func cmdJoin(args []string) error {
	initFlag, args := hasFlag(args, "--init")
	viaAddr, args := flagVal(args, "--via")
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm join <portkey> [--init] [--via H:P]")
	}
	keyStr := args[0]
	key, err := portkey.Decode(keyStr)
	if err != nil {
		return fmt.Errorf("bad port key: %w", err)
	}
	var r *store.Repo
	if initFlag {
		cwd, _ := os.Getwd()
		base := filepath.Base(cwd)
		if _, err := os.Stat(filepath.Join(cwd, ".lrm")); err != nil {
			r, err = store.InitWithWorkspace(cwd, base, store.DefaultPort, key.WorkspaceHex())
			if err != nil {
				return err
			}
			fmt.Printf("initialized repo in %s\n", cwd)
		} else {
			r, err = openRepo()
			if err != nil {
				return err
			}
		}
	} else {
		r, err = openRepo()
		if err != nil {
			return fmt.Errorf("%w (hint: run inside a repo, or `lrm join <key> --init`)", err)
		}
	}
	defer r.Close()
	// Workspace scoping: v2 keys carry the workspace they were minted for.
	if keyWS := key.WorkspaceHex(); keyWS != "" {
		if r.Config.Workspace != "" && r.Config.Workspace != keyWS {
			return fmt.Errorf("this key belongs to a different workspace (%.8s… ≠ %.8s…) — clone it into a fresh directory instead",
				r.Config.Workspace, keyWS)
		}
		if r.Config.Workspace == "" {
			if err := r.SetWorkspace(keyWS); err == nil {
				_ = r.SaveConfig()
			}
		}
	}
	if viaAddr != "" {
		fmt.Printf("dialing %s (peer %x) via relay %s...\n", key.Addr(), key.PeerID[:4], viaAddr)
	} else {
		fmt.Printf("dialing %s (peer %x)...\n", key.Addr(), key.PeerID[:4])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var sc *transport.SecureConn
	if viaAddr != "" {
		nodeID, nerr := node.LoadOrCreateIdentity()
		if nerr != nil {
			return fmt.Errorf("device identity unavailable: %w", nerr)
		}
		sc, err = relay.Connect(ctx, viaAddr, key.Addr(), r.Identity, nodeID, key.PeerID)
	} else {
		sc, err = transport.Dial(ctx, key.Addr(), r.Identity, r.Identity.Pub, key.PeerID)
	}
	if err != nil {
		return fmt.Errorf("secure dial failed: %w", err)
	}
	defer sc.Close()
	fmt.Println("handshake verified ✓ (peer identity matches Port Key)")
	sess := mux.NewSession(sc, true)
	defer sess.Close()
	eng := sync.New(r)
	res, err := eng.SyncWithSession(sess, true, "")
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	fmt.Printf("sync complete: fetched=%d pushed=%d\n", res.Fetched, res.Pushed)
	fmt.Printf("result: %s\n", res.Message)
	if res.ConflictBranch != "" {
		fmt.Printf("conflict branch: %s (your work untouched; merge when ready)\n", res.ConflictBranch)
	}
	return nil
}

func cmdSync(args []string) error {
	peerAddr, _ := flagVal(args, "--peer")
	viaAddr, _ := flagVal(args, "--via")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	targets := []string{}
	if peerAddr != "" {
		targets = append(targets, peerAddr)
	} else {
		fmt.Println("discovering LAN peers...")
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		peers, _ := mdns.Browse(ctx, 5*time.Second)
		cancel()
		self := r.Identity.HexID()
		for _, p := range peers {
			if p.PeerHex == self || p.Port == 0 || p.Addr() == "" {
				continue
			}
			targets = append(targets, p.Addr())
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no peers to sync with (try --peer HOST:PORT or run `lrm daemon` on teammates' machines)")
	}
	nodeID, nodeErr := node.LoadOrCreateIdentity()
	if viaAddr != "" && nodeErr != nil {
		return fmt.Errorf("device identity unavailable: %w", nodeErr)
	}
	for _, t := range targets {
		if viaAddr != "" {
			fmt.Printf("syncing with %s via relay %s...\n", t, viaAddr)
		} else {
			fmt.Printf("syncing with %s...\n", t)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		var sc *transport.SecureConn
		var err error
		if viaAddr != "" {
			sc, err = relay.Connect(ctx, viaAddr, t, r.Identity, nodeID, nil) // TOFU on target
		} else {
			sc, err = transport.Dial(ctx, t, r.Identity, r.Identity.Pub, nil) // TOFU on LAN
		}
		cancel()
		if err != nil {
			fmt.Printf("  dial failed: %v\n", shortErr(err))
			continue
		}
		sess := mux.NewSession(sc, true)
		eng := sync.New(r)
		res, err := eng.SyncWithSession(sess, true, "")
		_ = sess.Close()
		_ = sc.Close()
		if err != nil {
			fmt.Printf("  sync failed: %v\n", shortErr(err))
			continue
		}
		fmt.Printf("  fetched=%d pushed=%d: %s\n", res.Fetched, res.Pushed, res.Message)
	}
	return nil
}

func cmdDaemon(args []string) error {
	portStr, _ := flagVal(args, "--port", "-p")
	staticPeer, _ := flagVal(args, "--peer")
	r, err := openRepo()
	if err != nil {
		return err
	}
	port := r.Config.Port
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil {
			port = n
		}
	}
	cfg := daemon.DefaultConfig(port)
	d := daemon.New(r, cfg)
	if staticPeer != "" {
		if err := d.AddStaticPeer(staticPeer); err != nil {
			return err
		}
	}
	d.OnSync(func(res *sync.SyncResult) {
		fmt.Printf("[sync] %s (fetched=%d pushed=%d)\n", res.Message, res.Fetched, res.Pushed)
	})
	d.OnChange(func(n int) {
		fmt.Printf("[watch] auto-committed %d change(s)\n", n)
	})
	d.OnEvent(func(msg string) {
		fmt.Printf("[mesh] %s\n", msg)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d.SetStopFunc(stop) // control socket: {"cmd":"stop"}
	nodeInfo := ""
	if id, err := node.LoadOrCreateIdentity(); err == nil {
		nodeInfo = fmt.Sprintf(", device %s", id.ShortID())
	}
	fmt.Printf("LRM daemon starting (user %s, peer %s%s, port %d)\n", r.Config.User, r.Identity.ShortID(), nodeInfo, port)
	fmt.Println("watching workspace + announcing on LAN. Ctrl-C to stop (mappings will be removed).")
	fmt.Println("control socket: .lrm/daemon.sock — `lrm status` shows the live presence table.")
	if staticPeer != "" {
		fmt.Printf("static peer: %s (dialed continuously)\n", staticPeer)
	}
	if err := d.Run(ctx); err != nil {
		return err
	}
	fmt.Println("daemon stopped. Port mappings torn down. Goodbye!")
	return nil
}

// --- small helpers ---

func shortHex(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func shortList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, shortHex(s))
	}
	return out
}

func shortOrUnborn(tip string) string {
	if tip == "" {
		return "(unborn)"
	}
	return shortHex(tip)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func shortErr(err error) string {
	if err == nil {
		return "unknown"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func lanIP() string {
	addrs := mdns.LocalAddrs()
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}
