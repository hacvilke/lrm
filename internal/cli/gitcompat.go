// Git-reverse compat commands: git spellings, LRM (P2P) standing.
// push/pull/fetch/clone/remote are network aliases over the live sync
// engine; add/stash/reset/fsck/gc/tag wrap internal/gitcompat.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/gitcompat"
	"github.com/lrm-project/lrm/internal/mdns"
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
	if len(args) > 0 && (args[0] == "push" || args[0] == "pop" || args[0] == "list") {
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
