// Git-reverse compat commands: git spellings, LRM (P2P) standing.
// push/pull/fetch/clone/remote are network aliases over the live sync
// engine; add/stash/reset/fsck/gc/tag wrap internal/gitcompat.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/gitcompat"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/portkey"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
)

func cmdAdd(args []string) error {
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	// Accept (and ignore) -A/-u flags: tracking is always whole-workspace.
	var paths []string
	for _, a := range args {
		if a == "-A" || a == "-u" || a == "." {
			continue
		}
		paths = append(paths, a)
	}
	res, err := gitcompat.Add(r, paths)
	if err != nil {
		return err
	}
	if res.Tracked == 0 {
		fmt.Println("nothing to add (working dir clean — tracking is automatic)")
		return nil
	}
	fmt.Printf("tracking is automatic — %d path(s) will be included in the next commit:\n", res.Tracked)
	for _, p := range res.Paths {
		fmt.Printf("  %s\n", p)
	}
	return nil
}

func cmdPush(args []string) error {
	peerAddr, _ := flagVal(args, "--peer")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	targets, err := resolveTargets(peerAddr)
	if err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Printf("pushing to %s...\n", t)
		res, err := dialAndSync(r, t, nil)
		if err != nil {
			fmt.Printf("  failed: %v\n", shortErr(err))
			continue
		}
		fmt.Printf("  pushed=%d fetched=%d: %s\n", res.Pushed, res.Fetched, res.Message)
	}
	return nil
}

func cmdPull(args []string) error {
	peerAddr, _ := flagVal(args, "--peer")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	targets, err := resolveTargets(peerAddr)
	if err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Printf("pulling from %s...\n", t)
		res, err := dialAndSync(r, t, nil)
		if err != nil {
			fmt.Printf("  failed: %v\n", shortErr(err))
			continue
		}
		fmt.Printf("  fetched=%d pushed=%d: %s\n", res.Fetched, res.Pushed, res.Message)
		if res.ConflictBranch != "" {
			fmt.Printf("  conflict branch: %s (your work untouched)\n", res.ConflictBranch)
		}
	}
	return nil
}

func cmdClone(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: lrm clone <portkey> [dir]")
	}
	keyStr := args[0]
	dir := ""
	if len(args) > 1 {
		dir = args[1]
	} else {
		dir = "lrm-clone"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, ".lrm")); err == nil {
		return fmt.Errorf("%s is already an LRM repo", abs)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	r, err := store.Init(abs, filepath.Base(abs), store.DefaultPort)
	if err != nil {
		return err
	}
	defer r.Close()
	key, err := portkey.Decode(keyStr)
	if err != nil {
		return fmt.Errorf("bad port key: %w", err)
	}
	fmt.Printf("cloning from %s (peer %x) into %s...\n", key.Addr(), key.PeerID[:4], abs)
	res, err := dialAndSync(r, key.Addr(), key.PeerID)
	if err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}
	fmt.Printf("cloned: fetched=%d — %s\n", res.Fetched, res.Message)
	return nil
}

func cmdStash(args []string) error {
	sub := ""
	rest := args
	if len(args) > 0 && (args[0] == "push" || args[0] == "pop" || args[0] == "list" ||
		args[0] == "show" || args[0] == "drop" || args[0] == "clear" || args[0] == "apply") {
		sub, rest = args[0], args[1:]
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	switch sub {
	case "", "push":
		msg, _ := flagVal(rest, "-m", "--message")
		e, err := gitcompat.StashPush(r, msg)
		if err != nil {
			return err
		}
		fmt.Printf("shelved delta as %s (%s)\n", e.Name, e.Message)
		fmt.Println("workdir restored to HEAD.")
	case "pop":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		e, err := gitcompat.StashPop(r, name)
		if err != nil {
			return err
		}
		fmt.Printf("restored %s (%s) — workdir is dirty with the shelved delta.\n", e.Name, e.Message)
	case "apply":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		e, err := gitcompat.StashApply(r, name)
		if err != nil {
			return err
		}
		fmt.Printf("applied %s (%s) — shelf kept.\n", e.Name, e.Message)
	case "list":
		entries, err := gitcompat.StashList(r)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("(no stashes)")
			return nil
		}
		for _, e := range entries {
			fmt.Printf("%s  %s  %s\n", e.Name, e.Commit[:12], e.Message)
		}
	case "show":
		nameOnly, rest := hasFlag(rest, "--name-only")
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		res, err := gitcompat.StashShow(r, name)
		if err != nil {
			return err
		}
		fmt.Printf("%s  %s  %s\n", res.Entry.Name, res.Entry.Commit[:12], res.Entry.Message)
		if len(res.Files) == 0 {
			fmt.Println("(no file changes vs parent)")
			return nil
		}
		for _, f := range res.Files {
			fmt.Printf("%c %s\n", f.Kind, f.Path)
		}
		if nameOnly {
			return nil
		}
		for _, f := range res.Files {
			if f.Patch.Binary {
				fmt.Printf("--- a/%s  (binary or too large — no text diff)\n", f.Path)
				continue
			}
			fmt.Print(f.Patch.Unified())
		}
	case "drop":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		e, err := gitcompat.StashDrop(r, name)
		if err != nil {
			return err
		}
		fmt.Printf("dropped %s (%s) — remaining shelves renumbered.\n", e.Name, e.Message)
	case "clear":
		n, err := gitcompat.StashClear(r)
		if err != nil {
			return err
		}
		fmt.Printf("cleared %d stash(es).\n", n)
	}
	return nil
}

func cmdNotes(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm notes add|show|list|remove <REF> [-m TEXT]")
	}
	sub, rest := args[0], args[1:]
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	switch sub {
	case "list":
		notes, err := gitcompat.NoteList(r)
		if err != nil {
			return err
		}
		if len(notes) == 0 {
			fmt.Println("(no notes)")
			return nil
		}
		for _, n := range notes {
			fmt.Printf("%s  %s\n", shortHex(n.Commit), firstLine(n.Text))
		}
	case "add":
		force, rest := hasFlag(rest, "--force", "-f")
		msg, rest := flagVal(rest, "-m", "--message")
		if len(rest) == 0 {
			return fmt.Errorf("usage: lrm notes add <REF> -m TEXT [--force]")
		}
		h, err := resolveCommitRef(r, rest[0])
		if err != nil {
			return err
		}
		if err := gitcompat.NoteAdd(r, h, msg, force); err != nil {
			return err
		}
		fmt.Printf("noted %s\n", shortHex(cas.Hex(h)))
	case "show":
		if len(rest) == 0 {
			return fmt.Errorf("usage: lrm notes show <REF>")
		}
		h, err := resolveCommitRef(r, rest[0])
		if err != nil {
			return err
		}
		text, err := gitcompat.NoteShow(r, h)
		if err != nil {
			return err
		}
		fmt.Println(text)
	case "remove":
		if len(rest) == 0 {
			return fmt.Errorf("usage: lrm notes remove <REF>")
		}
		h, err := resolveCommitRef(r, rest[0])
		if err != nil {
			return err
		}
		if err := gitcompat.NoteRemove(r, h); err != nil {
			return err
		}
		fmt.Printf("removed note on %s\n", shortHex(cas.Hex(h)))
	default:
		return fmt.Errorf("usage: lrm notes add|show|list|remove <REF> [-m TEXT]")
	}
	return nil
}

func cmdReset(args []string) error {
	mode := "--mixed" // git default
	var rest []string
	for _, a := range args {
		switch a {
		case "--soft", "--mixed", "--hard":
			mode = a
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lrm reset [--soft|--mixed|--hard] <commit>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	h, err := resolveCommitRef(r, rest[0])
	if err != nil {
		return err
	}
	switch mode {
	case "--soft":
		if err := gitcompat.ResetSoft(r, h); err != nil {
			return err
		}
		fmt.Printf("reset --soft: branch now at %s (workdir + index untouched)\n", cas.Short(h))
	case "--mixed":
		if err := gitcompat.ResetMixed(r, h); err != nil {
			return err
		}
		fmt.Printf("reset --mixed: branch now at %s (delta visible in status)\n", cas.Short(h))
	case "--hard":
		if err := gitcompat.ResetHard(r, h); err != nil {
			return err
		}
		fmt.Printf("reset --hard: branch + workdir now at %s\n", cas.Short(h))
	}
	return nil
}

func cmdFsck(_ []string) error {
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	fmt.Println("verifying CAS reachability from all refs...")
	res, err := gitcompat.Fsck(r)
	if err != nil {
		return err
	}
	fmt.Printf("commits=%d trees=%d blobs=%d manifests=%d chunks=%d bytes=%d\n",
		res.Commits, res.Trees, res.Blobs, res.Manifests, res.Chunks, res.Bytes)
	if len(res.Missing) == 0 {
		fmt.Println("OK — repository is healthy.")
		return nil
	}
	fmt.Printf("BROKEN — %d missing object(s):\n", len(res.Missing))
	for _, m := range res.Missing {
		fmt.Printf("  missing %s\n", m)
	}
	return fmt.Errorf("repository failed integrity check")
}

func cmdGC(args []string) error {
	dry, _ := hasFlag(args, "--dry-run", "-n")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	res, err := gitcompat.GC(r, dry)
	if err != nil {
		return err
	}
	verb := "removed"
	if dry {
		verb = "would remove"
	}
	fmt.Printf("examined=%d kept=%d %s=%d freed=%d bytes\n", res.Examined, res.Kept, verb, res.Removed, res.Freed)
	return nil
}

func cmdRemote(args []string) error {
	if len(args) == 0 || (len(args) == 1 && args[0] == "-v") {
		fmt.Println("LRM has no configured remotes — peers are discovered live (not URLs).")
		fmt.Println("discovering for 4s...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		peers, _ := mdns.Browse(ctx, 4*time.Second)
		cancel()
		self := ""
		if r, err := openRepo(); err == nil {
			self = r.Identity.HexID()
			_ = r.Close()
		}
		shown := 0
		for _, p := range peers {
			if p.PeerHex == self {
				continue
			}
			fmt.Printf("  %-8s %s (peer %s, %s) [live]\n", p.User, p.Addr(), p.PeerHex[:12], p.Source)
			shown++
		}
		if shown == 0 {
			fmt.Println("(no live peers right now)")
		}
		return nil
	}
	return fmt.Errorf("LRM has no remotes to configure — peers are discovered (LAN) or dialed (Port Key). `%s` is intentionally unsupported", "remote "+args[0])
}

func cmdTag(args []string) error {
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if len(args) == 2 && (args[0] == "-d" || args[0] == "--delete") {
		if err := r.DeleteTag(args[1]); err != nil {
			return err
		}
		fmt.Printf("deleted tag %s\n", args[1])
		return nil
	}
	if len(args) == 0 {
		tags, err := gitcompat.TagList(r)
		if err != nil {
			return err
		}
		if len(tags) == 0 {
			fmt.Println("(no tags)")
			return nil
		}
		for name, hexStr := range tags {
			fmt.Printf("  %-20s %s\n", name, shortHex(hexStr))
		}
		return nil
	}
	name := args[0]
	var h cas.Hash
	if len(args) > 1 {
		hh, err := resolveCommitRef(r, args[1])
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
	if err := gitcompat.TagCreate(r, name, h); err != nil {
		return err
	}
	fmt.Printf("tagged %s → %s\n", name, cas.Short(h))
	return nil
}

// --- shared network helpers ---

func resolveTargets(explicit string) ([]string, error) {
	if explicit != "" {
		return []string{explicit}, nil
	}
	fmt.Println("discovering LAN peers...")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	peers, _ := mdns.Browse(ctx, 5*time.Second)
	cancel()
	self := ""
	if r, err := openRepo(); err == nil {
		self = r.Identity.HexID()
		_ = r.Close()
	}
	var out []string
	for _, p := range peers {
		if p.PeerHex == self || p.Port == 0 || p.Addr() == "" {
			continue
		}
		out = append(out, p.Addr())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no peers found (try --peer HOST:PORT)")
	}
	return out, nil
}

func dialAndSync(r *store.Repo, addr string, expectPeerID []byte) (*sync.SyncResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sc, err := transport.Dial(ctx, addr, r.Identity, r.Identity.Pub, expectPeerID)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	sess := mux.NewSession(sc, true)
	defer sess.Close()
	return sync.New(r).SyncWithSession(sess, true, "")
}

func cmdBlame(args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return fmt.Errorf("usage: lrm blame <file> [ref]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	ref := "HEAD"
	if len(args) == 2 {
		ref = args[1]
	}
	h, err := resolveCommitRef(r, ref)
	if err != nil {
		return err
	}
	lines, err := gitcompat.Blame(r, h, filepath.ToSlash(args[0]))
	if err != nil {
		return err
	}
	for _, b := range lines {
		fmt.Println(gitcompat.FormatBlame(b))
	}
	return nil
}

func cmdPick(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: lrm cherry-pick <ref>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	h, err := resolveCommitRef(r, args[0])
	if err != nil {
		return err
	}
	res, err := gitcompat.CherryPick(r, h)
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	return nil
}

func cmdRebase(args []string) error {
	cont, args := hasFlag(args, "--continue")
	abort, args := hasFlag(args, "--abort")
	if cont && abort {
		return fmt.Errorf("pass only one of --continue or --abort")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	switch {
	case cont:
		if len(args) != 0 {
			return fmt.Errorf("usage: lrm rebase --continue")
		}
		res, err := gitcompat.RebaseContinue(r)
		if err != nil {
			return err
		}
		fmt.Println(res.Message)
	case abort:
		if len(args) != 0 {
			return fmt.Errorf("usage: lrm rebase --abort")
		}
		res, err := gitcompat.RebaseAbort(r)
		if err != nil {
			return err
		}
		fmt.Println(res.Message)
	default:
		if len(args) != 1 {
			return fmt.Errorf("usage: lrm rebase <UPSTREAM> [--continue|--abort]")
		}
		h, err := resolveCommitRef(r, args[0])
		if err != nil {
			return err
		}
		res, err := gitcompat.RebaseStart(r, h)
		if err != nil {
			return err
		}
		fmt.Println(res.Message)
	}
	return nil
}

func cmdGrep(args []string) error {
	ci, args := hasFlag(args, "-i", "--ignore-case")
	filesOnly, args := hasFlag(args, "-l", "--files-with-matches")
	if len(args) == 0 || len(args) > 2 {
		return fmt.Errorf("usage: lrm grep [-i] [-l] <pattern> [ref]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	pattern := args[0]
	var res *gitcompat.GrepResult
	if len(args) == 2 {
		h, err := resolveCommitRef(r, args[1])
		if err != nil {
			return err
		}
		res, err = gitcompat.GrepRef(r, h, pattern, ci)
		if err != nil {
			return err
		}
	} else {
		res, err = gitcompat.GrepWorkdir(r, pattern, ci)
		if err != nil {
			return err
		}
	}
	if filesOnly {
		seen := map[string]bool{}
		for _, m := range res.Matches {
			if !seen[m.Path] {
				seen[m.Path] = true
				fmt.Println(m.Path)
			}
		}
	} else {
		for _, m := range res.Matches {
			fmt.Printf("%s:%d:%s\n", m.Path, m.Lineno, m.Text)
		}
	}
	if res.Truncated {
		fmt.Fprintf(os.Stderr, "(match list truncated at %d)\n", gitcompat.MaxGrepMatches)
	}
	if res.Skipped > 0 {
		fmt.Fprintf(os.Stderr, "(%d file(s) skipped: too large or binary)\n", res.Skipped)
	}
	if len(res.Matches) == 0 {
		return fmt.Errorf("no matches for %q", pattern)
	}
	return nil
}

func cmdConfig(args []string) error {
	list, args := hasFlag(args, "--list", "-l")
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if list || len(args) == 0 {
		fmt.Printf("user=%s\nport=%d\npeer=%s\n", r.Config.User, r.Config.Port, r.Identity.ShortID())
		return nil
	}
	key := args[0]
	if len(args) == 1 {
		switch key {
		case "user":
			fmt.Println(r.Config.User)
		case "port":
			fmt.Println(r.Config.Port)
		case "peer":
			fmt.Println(r.Identity.HexID())
		default:
			return fmt.Errorf("unknown key %q (want user|port|peer)", key)
		}
		return nil
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: lrm config [--list] [user|port [value]]")
	}
	switch key {
	case "user":
		if err := r.SetUser(args[1]); err != nil {
			return err
		}
	case "port":
		var port int
		if _, err := fmt.Sscanf(args[1], "%d", &port); err != nil {
			return fmt.Errorf("port must be a number")
		}
		if err := r.SetPort(port); err != nil {
			return err
		}
	case "peer":
		return fmt.Errorf("peer id is immutable (it is your cryptographic identity)")
	default:
		return fmt.Errorf("unknown key %q (want user|port)", key)
	}
	if err := r.SaveConfig(); err != nil {
		return err
	}
	fmt.Printf("%s=%s\n", key, args[1])
	return nil
}

func cmdClean(args []string) error {
	dry, args := hasFlag(args, "-n", "--dry-run")
	force, args := hasFlag(args, "-f", "--force")
	withDirs, args := hasFlag(args, "-d", "--dir")
	allIgnored, args := hasFlag(args, "-x", "--ignored")
	if len(args) != 0 {
		return fmt.Errorf("usage: lrm clean [-n] [-f] [-d] [-x]")
	}
	_ = dry // default IS a dry run; -n just says so explicitly
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	res, err := gitcompat.Clean(r, force, withDirs, allIgnored)
	if err != nil {
		return err
	}
	if len(res.Candidates) == 0 {
		fmt.Println("(nothing untracked — workdir only holds versioned files)")
		return nil
	}
	for _, c := range res.Candidates {
		if res.Removed {
			fmt.Printf("removed %s\n", c)
		} else {
			fmt.Printf("would remove %s\n", c)
		}
	}
	if !force {
		fmt.Println("(dry run — pass -f to actually delete)")
	}
	return nil
}

func cmdDescribe(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: lrm describe [ref]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	ref := "HEAD"
	if len(args) == 1 {
		ref = args[0]
	}
	h, err := resolveCommitRef(r, ref)
	if err != nil {
		return err
	}
	name, err := gitcompat.Describe(r, h)
	if err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

func cmdBisect(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm bisect start <bad> [good...] | good|bad|skip [ref] | run <cmd...> | reset | log")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	sub := args[0]
	rest := args[1:]
	resolve := func(ref string) (cas.Hash, error) { return resolveCommitRef(r, ref) }
	switch sub {
	case "start":
		if len(rest) == 0 {
			return fmt.Errorf("usage: lrm bisect start <bad> [good...]")
		}
		bad, err := resolve(rest[0])
		if err != nil {
			return err
		}
		var goods []cas.Hash
		for _, g := range rest[1:] {
			gh, err := resolve(g)
			if err != nil {
				return err
			}
			goods = append(goods, gh)
		}
		step, err := gitcompat.BisectStart(r, bad, goods)
		if err != nil {
			return err
		}
		fmt.Println(step.Message)
		return nil
	case "good", "bad", "skip":
		if len(rest) > 1 {
			return fmt.Errorf("usage: lrm bisect %s [ref]", sub)
		}
		var ref cas.Hash
		hasRef := false
		if len(rest) == 1 {
			ref, err = resolve(rest[0])
			if err != nil {
				return err
			}
			hasRef = true
		}
		step, err := gitcompat.BisectMark(r, sub, ref, hasRef)
		if err != nil {
			return err
		}
		fmt.Println(step.Message)
		return nil
	case "run":
		if len(rest) == 0 {
			return fmt.Errorf("usage: lrm bisect run <command...>")
		}
		step, err := gitcompat.BisectRun(r, rest, func(s string) {
			if s != "" {
				fmt.Println(s)
			}
		})
		if err != nil {
			return err
		}
		_ = step
		return nil
	case "reset":
		br, err := gitcompat.BisectReset(r)
		if err != nil {
			return err
		}
		fmt.Printf("bisect reset — back on %s\n", br)
		return nil
	case "log":
		// read-only peek requires an active session; reuse mark plumbing
		return cmdBisectLog(r)
	}
	return fmt.Errorf("unknown bisect subcommand %q", sub)
}

func cmdBisectLog(r *store.Repo) error {
	raw, err := os.ReadFile(filepath.Join(r.LrmDir, "BISECT_STATE"))
	if err != nil {
		return fmt.Errorf("no bisect in progress")
	}
	var a struct {
		OrigBranch string            `json:"orig_branch"`
		Bad        string            `json:"bad"`
		Good       []string          `json:"good"`
		Tested     map[string]string `json:"tested"`
		Current    string            `json:"current"`
		Concluded  string            `json:"concluded,omitempty"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	fmt.Printf("orig: %s  bad: %s  current: %s\n", a.OrigBranch, shortHex(a.Bad), shortHex(a.Current))
	for _, g := range a.Good {
		fmt.Printf("  good: %s\n", shortHex(g))
	}
	for hx, v := range a.Tested {
		if v == "skip" {
			fmt.Printf("  skip: %s\n", shortHex(hx))
		}
	}
	if a.Concluded != "" {
		fmt.Printf("concluded: first bad = %s\n", shortHex(a.Concluded))
	}
	return nil
}

func cmdRefLog(args []string) error {
	limitStr, args := flagVal(args, "--last", "-n")
	limit := 20
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	branch := ""
	if len(args) > 1 {
		return fmt.Errorf("usage: lrm ref-log [branch] [--last N]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	if len(args) == 1 {
		branch = args[0]
	} else {
		branch, _ = r.HeadBranch()
	}
	ents, err := r.ReadReflog(branch, limit)
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		fmt.Printf("(no ref history for %s yet — moves are recorded from here on)\n", branch)
		return nil
	}
	for _, e := range ents {
		old := shortHex(e.OldHex)
		if e.OldHex == store.ZeroHex {
			old = "(born)"
		}
		fmt.Printf("%s  %s -> %s  %-12s %s\n",
			e.Time.Format("2006-01-02 15:04"), old, shortHex(e.NewHex), e.Action, e.Detail)
	}
	return nil
}

func cmdArchive(args []string) error {
	out, args := flagVal(args, "-o", "--output")
	format, args := flagVal(args, "--format")
	ref := "HEAD"
	if len(args) > 1 {
		return fmt.Errorf("usage: lrm archive [ref] -o FILE [--format tar|tar.gz|zip]")
	}
	if len(args) == 1 {
		ref = args[0]
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	h, err := resolveCommitRef(r, ref)
	if err != nil {
		return err
	}
	if out == "" {
		out = fmt.Sprintf("archive-%s.tar.gz", cas.Hex(h)[:12])
	}
	if format == "" {
		format = gitcompat.GuessFormat(out)
	}
	if format == "" {
		format = "tar.gz"
	}
	if err := gitcompat.Archive(r, h, format, out); err != nil {
		return err
	}
	fmt.Printf("archived %s → %s (%s)\n", cas.Short(h), out, format)
	return nil
}

func cmdShortlog(args []string) error {
	limitStr, args := flagVal(args, "--limit", "-n")
	limit := 0
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: lrm shortlog [ref] [--limit N]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	ref := "HEAD"
	if len(args) == 1 {
		ref = args[0]
	}
	h, err := resolveCommitRef(r, ref)
	if err != nil {
		return err
	}
	rows, err := gitcompat.Shortlog(r, h, limit)
	if err != nil {
		return err
	}
	for _, ac := range rows {
		fmt.Printf("%5d\t%s\n", ac.Commits, ac.Author)
	}
	return nil
}

func cmdMv(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: lrm mv <src> <dst>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	src := filepath.Join(r.Root, filepath.FromSlash(args[0]))
	dst := filepath.Join(r.Root, filepath.FromSlash(args[1]))
	if !withinRoot(r.Root, src) || !withinRoot(r.Root, dst) {
		return fmt.Errorf("paths must stay inside the repo")
	}
	if _, err := os.Lstat(src); err != nil {
		return fmt.Errorf("no such file %q", args[0])
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination %q already exists", args[1])
	}
	if _, err := os.Stat(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("destination directory does not exist (create it first)")
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	fmt.Printf("renamed %s -> %s\n", args[0], args[1])
	return nil
}

func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cmdRevParse(args []string) error {
	short, args := hasFlag(args, "--short")
	_, args = hasFlag(args, "--verify") // accepted for muscle memory; resolution is always strict
	abbrev, args := hasFlag(args, "--abbrev-ref")
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm rev-parse [--short] [--verify] [--abbrev-ref] <ref>...")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	var branches []string
	if abbrev {
		branches, _ = r.ListBranches()
	}
	for _, ref := range args {
		h, err := resolveCommitRef(r, ref)
		if err != nil {
			return err
		}
		hexStr := cas.Hex(h)
		if abbrev {
			if isBranchName(branches, ref) {
				fmt.Println(ref)
				continue
			}
			if b := branchAtTip(r, branches, hexStr); b != "" {
				fmt.Println(b)
				continue
			}
		}
		if short {
			fmt.Println(shortHex(hexStr))
		} else {
			fmt.Println(hexStr)
		}
	}
	return nil
}

func isBranchName(branches []string, name string) bool {
	for _, b := range branches {
		if b == name {
			return true
		}
	}
	return false
}

// branchAtTip returns the (sorted-first) branch pointing at hexStr, if any.
func branchAtTip(r *store.Repo, branches []string, hexStr string) string {
	var hits []string
	for _, b := range branches {
		if tip, _ := r.GetRef(b); tip == hexStr {
			hits = append(hits, b)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	return hits[0]
}

func cmdMergeBase(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: lrm merge-base <A> <B>")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	a, err := resolveCommitRef(r, args[0])
	if err != nil {
		return err
	}
	b, err := resolveCommitRef(r, args[1])
	if err != nil {
		return err
	}
	base, err := gitcompat.MergeBase(r, a, b)
	if err != nil {
		return err
	}
	fmt.Println(cas.Hex(base))
	return nil
}

func cmdCherry(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: lrm cherry <upstream> [<head>]")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	up, err := resolveCommitRef(r, args[0])
	if err != nil {
		return err
	}
	head := cas.Nil
	if len(args) == 2 {
		head, err = resolveCommitRef(r, args[1])
		if err != nil {
			return err
		}
	} else {
		head, _, err = r.HeadCommit()
		if err != nil {
			return err
		}
		if head == cas.Nil {
			return fmt.Errorf("no commits yet")
		}
	}
	ents, err := gitcompat.Cherry(r, up, head)
	if err != nil {
		return err
	}
	for _, e := range ents {
		mark := "+"
		if e.Applied {
			mark = "-"
		}
		fmt.Printf("%s %s %s\n", mark, shortHex(e.Commit), e.Subject)
	}
	return nil
}

func cmdCheckIgnore(args []string) error {
	verbose, args := hasFlag(args, "-v", "--verbose")
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm check-ignore [-v] <path>...")
	}
	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()
	ign := merkle.LoadIgnore(r.Root)
	matched := 0
	for _, p := range args {
		rel := filepath.ToSlash(filepath.Clean(p))
		if filepath.IsAbs(p) {
			abs := p
			r2, err := filepath.Rel(r.Root, abs)
			if err != nil || !withinRoot(r.Root, abs) {
				return fmt.Errorf("path %q is outside the repo", p)
			}
			rel = filepath.ToSlash(r2)
		}
		rel = strings.TrimPrefix(rel, "./")
		top := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			top = rel[:i]
		}
		if top == ".lrm" || top == ".git" {
			matched++
			if verbose {
				fmt.Printf("built-in:0:.lrm/.git\t%s\n", rel)
			} else {
				fmt.Println(rel)
			}
			continue
		}
		rule := ign.MatchRule(rel)
		if rule == nil || rule.Negate {
			continue
		}
		matched++
		if verbose {
			fmt.Printf(".lrmignore:%d:%s\t%s\n", rule.Line, rule.Source(), rel)
		} else {
			fmt.Println(rel)
		}
	}
	if matched == 0 {
		return fmt.Errorf("no paths ignored")
	}
	return nil
}
