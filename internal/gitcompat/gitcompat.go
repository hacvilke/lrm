// Package gitcompat implements the git-reverse compatibility layer.
//
// Every command is a git spelling reversed into LRM standing: same muscle
// memory, but peer-to-peer semantics (no remotes, no force-push, DAG history
// that can never be overwritten).
//
// Provided: add (validate), stash push/pop/list, reset --soft/--mixed/--hard,
// fsck (CAS reachability), gc (unreachable pruning), tag (lightweight refs).
// Network aliases (push/pull/fetch/clone/remote) live in internal/cli since
// they reuse the live sync engine.
package gitcompat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// --- add ---

// AddResult reports what `lrm add` validated.
type AddResult struct {
	Tracked int
	Paths   []string
}

// Add validates that paths are tracked by LRM's always-on virtual staging.
// LRM needs no manual staging (`git add` ritual reversed away): this command
// exists for muscle memory and confirms what the next commit will include.
func Add(r *store.Repo, paths []string) (*AddResult, error) {
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, ch := range res.Changes {
		changed[ch.Path] = true
	}
	out := &AddResult{}
	if len(paths) == 0 {
		out.Tracked = len(res.Changes)
		for _, ch := range res.Changes {
			out.Paths = append(out.Paths, ch.Kind+":"+ch.Path)
		}
		return out, nil
	}
	for _, p := range paths {
		clean := filepath.ToSlash(filepath.Clean(p))
		full := filepath.Join(r.Root, filepath.FromSlash(clean))
		if _, err := os.Stat(full); err != nil {
			// Possibly a deleted path still in the delta.
			if changed[clean] {
				out.Paths = append(out.Paths, "deleted:"+clean)
				out.Tracked++
				continue
			}
			return nil, fmt.Errorf("pathspec %q does not match any file", p)
		}
		if changed[clean] {
			out.Paths = append(out.Paths, "staged:"+clean)
		} else {
			out.Paths = append(out.Paths, "clean:"+clean)
		}
		out.Tracked++
	}
	return out, nil
}

// --- stash ---

// shelfDir is where stashed deltas live as commits (refs/shelves/*).
func shelfDir(r *store.Repo) string { return filepath.Join(r.LrmDir, "refs", "shelves") }

// StashEntry describes one shelf.
type StashEntry struct {
	Name    string // e.g. stash@{0}
	Commit  string // hex
	Message string
	Time    int64
}

// StashPush shelves the current workdir delta: the delta is committed onto a
// shelf ref, then the workdir is restored to HEAD (clean). Returns entry name.
func StashPush(r *store.Repo, message string) (*StashEntry, error) {
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return nil, err
	}
	if res.Clean {
		return nil, fmt.Errorf("no local changes to stash")
	}
	tip, br, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	if tip == cas.Nil {
		return nil, fmt.Errorf("cannot stash before the first commit")
	}
	// Commit delta onto shelf (parent = HEAD, but NOT on the branch).
	var clock map[string]uint64
	if tc, err := r.DAG.Get(tip); err == nil {
		clock = tc.Clock.Clone()
	} else {
		clock = map[string]uint64{}
	}
	clock[r.Identity.HexID()]++
	if message == "" {
		message = fmt.Sprintf("shelf on %s: %d file(s)", br, len(res.Changes))
	}
	c := &dag.Commit{
		Version: 1, Tree: cas.Hex(res.RootHash),
		Parents:   []string{cas.Hex(tip)},
		Author:    r.Config.User,
		PeerHex:   r.Identity.HexID(),
		Timestamp: time.Now().UnixNano(),
		Message:   message,
		Clock:     clock,
	}
	h, err := r.DAG.Put(c)
	if err != nil {
		return nil, err
	}
	name, err := nextShelfName(r)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(shelfDir(r), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(shelfDir(r), name), []byte(cas.Hex(h)+"\n"), 0o644); err != nil {
		return nil, err
	}
	// Restore workdir to HEAD.
	if err := sync.Checkout(r, tip); err != nil {
		return nil, err
	}
	return &StashEntry{Name: "stash@{" + name + "}", Commit: cas.Hex(h), Message: message, Time: c.Timestamp}, nil
}

func nextShelfName(r *store.Repo) (string, error) {
	ents, err := os.ReadDir(shelfDir(r))
	if err != nil {
		if os.IsNotExist(err) {
			return "0", nil
		}
		return "", err
	}
	max := -1
	for _, e := range ents {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%d", max+1), nil
}

// StashList returns shelves newest-first.
func StashList(r *store.Repo) ([]StashEntry, error) {
	ents, err := os.ReadDir(shelfDir(r))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []StashEntry
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(shelfDir(r), e.Name()))
		if err != nil {
			continue
		}
		hexStr := strings.TrimSpace(string(raw))
		h, err := cas.ParseHex(hexStr)
		if err != nil {
			continue
		}
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		out = append(out, StashEntry{Name: "stash@{" + e.Name() + "}", Commit: hexStr, Message: c.Message, Time: c.Timestamp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out, nil
}

// StashPop restores a shelf's tree into the workdir WITHOUT committing
// (workdir becomes dirty with the shelved delta) and drops the shelf.
// Empty name = newest.
func StashPop(r *store.Repo, name string) (*StashEntry, error) {
	entries, err := StashList(r)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no stash entries found")
	}
	pick := findStashEntry(entries, name)
	if pick == nil {
		return nil, fmt.Errorf("unknown stash %q", name)
	}
	h, _ := cas.ParseHex(pick.Commit)
	c, err := r.DAG.Get(h)
	if err != nil {
		return nil, err
	}
	th, _ := cas.ParseHex(c.Tree)
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return nil, err
	}
	// Write shelved files into workdir WITHOUT touching the index/refs,
	// so `status` shows them as the restored delta.
	for p, hx := range flat {
		oh, err := cas.ParseHex(hx)
		if err != nil {
			return nil, err
		}
		if err := writeObjectToFile(r, oh, filepath.Join(r.Root, filepath.FromSlash(p))); err != nil {
			return nil, fmt.Errorf("restore %s: %w", p, err)
		}
	}
	// Drop the shelf and compact the rest (a pop renumbers, git parity).
	_ = os.Remove(filepath.Join(shelfDir(r), shelfFileName(pick.Name)))
	_ = compactShelves(r)
	return pick, nil
}

func writeObjectToFile(r *store.Repo, h cas.Hash, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if raw, err := r.CAS.GetBytes(h, 4<<20); err == nil {
		var m chunker.Manifest
		if err := jsonUnmarshal(raw, &m); err == nil && m.Version == 1 && len(m.Chunks) > 0 {
			tmp := dst + ".lrmtmp"
			out, err := os.Create(tmp)
			if err != nil {
				return err
			}
			rerr := chunker.Reassemble(r.CAS, &m, out)
			cerr := out.Close()
			if rerr != nil {
				_ = os.Remove(tmp)
				return rerr
			}
			if cerr != nil {
				_ = os.Remove(tmp)
				return cerr
			}
			return os.Rename(tmp, dst)
		}
	}
	rc, err := r.CAS.Get(h)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".lrmtmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, cpErr := io.CopyBuffer(out, rc, make([]byte, 32*1024))
	cerr := out.Close()
	if cpErr != nil {
		_ = os.Remove(tmp)
		return cpErr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, dst)
}

// --- reset ---

// ResetSoft moves the current branch ref to target (workdir + index untouched).
func ResetSoft(r *store.Repo, target cas.Hash) error {
	br, old, err := resetMove(r, target)
	if err != nil {
		return err
	}
	r.AppendReflog(br, old, cas.Hex(target), "reset --soft", "to "+cas.Short(target))
	return nil
}

// resetMove moves the current branch ref (unlogged; callers log the mode).
func resetMove(r *store.Repo, target cas.Hash) (string, string, error) {
	br, err := r.HeadBranch()
	if err != nil {
		return "", "", err
	}
	if !r.DAG.Has(target) {
		return "", "", fmt.Errorf("unknown commit %s", cas.Short(target))
	}
	old, _ := r.GetRef(br)
	if err := r.SetRef(br, cas.Hex(target)); err != nil {
		return "", "", err
	}
	return br, old, nil
}

// ResetMixed moves the ref AND refreshes the index to the target tree
// (workdir untouched — changes appear as deltas, like git reset --mixed).
func ResetMixed(r *store.Repo, target cas.Hash) error {
	br, old, err := resetMove(r, target)
	if err != nil {
		return err
	}
	r.AppendReflog(br, old, cas.Hex(target), "reset --mixed", "to "+cas.Short(target))
	c, err := r.DAG.Get(target)
	if err != nil {
		return err
	}
	th, _ := cas.ParseHex(c.Tree)
	if err := r.Index.UpdateFromScan(r.Root, r.CAS, th); err != nil {
		return err
	}
	return r.Index.Save()
}

// ResetHard moves the ref and rewrites the workdir to the target tree.
func ResetHard(r *store.Repo, target cas.Hash) error {
	br, old, err := resetMove(r, target)
	if err != nil {
		return err
	}
	r.AppendReflog(br, old, cas.Hex(target), "reset --hard", "to "+cas.Short(target))
	return sync.Checkout(r, target)
}

// --- tag ---

func tagDir(r *store.Repo) string { return filepath.Join(r.LrmDir, "refs", "tags") }

// TagCreate creates a lightweight tag ref.
func TagCreate(r *store.Repo, name string, target cas.Hash) error {
	if name == "" || strings.ContainsAny(name, " /\\~^:?*[]") {
		return fmt.Errorf("invalid tag name %q", name)
	}
	if !r.DAG.Has(target) {
		return fmt.Errorf("unknown commit %s", cas.Short(target))
	}
	if err := os.MkdirAll(tagDir(r), 0o755); err != nil {
		return err
	}
	p := filepath.Join(tagDir(r), name)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("tag %q already exists", name)
	}
	return os.WriteFile(p, []byte(cas.Hex(target)+"\n"), 0o644)
}

// TagList lists tags → commit.
func TagList(r *store.Repo) (map[string]string, error) {
	ents, err := os.ReadDir(tagDir(r))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(tagDir(r), e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = strings.TrimSpace(string(raw))
	}
	return out, nil
}

// --- fsck ---

// collectRoots gathers reachability roots: branch heads, tags, shelves,
// plus ref-log old/new tips — so amended or reset-away commits survive
// collection while their log entries exist (git kept-by-reflog parity).
func collectRoots(r *store.Repo) []cas.Hash {
	var tips []cas.Hash
	collectRefDir := func(dir string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if h, err := cas.ParseHex(strings.TrimSpace(string(raw))); err == nil {
				tips = append(tips, h)
			}
		}
	}
	collectRefDir(filepath.Join(r.LrmDir, "refs", "heads"))
	collectRefDir(tagDir(r))
	collectRefDir(shelfDir(r))
	if ents, err := os.ReadDir(filepath.Join(r.LrmDir, "logs", "refs")); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			entries, err := r.ReadReflog(e.Name(), 0)
			if err != nil {
				continue
			}
			for _, re := range entries {
				for _, hx := range []string{re.OldHex, re.NewHex} {
					hx = strings.TrimSpace(hx)
					if hx == "" || hx == store.ZeroHex {
						continue
					}
					if h, err := cas.ParseHex(hx); err == nil {
						tips = append(tips, h)
					}
				}
			}
		}
	}
	return tips
}

// FsckResult summarizes repository health.
type FsckResult struct {
	Commits  int
	Trees    int
	Blobs    int
	Chunks   int
	Manifests int
	Missing  []string
	Bytes    int64
}

// Fsck walks every ref tip (branches, tags, shelves) and verifies all
// reachable objects exist in the CAS.
func Fsck(r *store.Repo) (*FsckResult, error) {
	res := &FsckResult{}
	seen := map[cas.Hash]bool{}
	tips := collectRoots(r)

	var walkTree func(h cas.Hash)
	walkTree = func(h cas.Hash) {
		if seen[h] {
			return
		}
		seen[h] = true
		if !r.CAS.Exists(h) {
			res.Missing = append(res.Missing, "tree "+cas.Hex(h))
			return
		}
		if sz, err := r.CAS.Stat(h); err == nil {
			res.Bytes += sz
		}
		res.Trees++
		t, err := merkle.LoadTree(r.CAS, h)
		if err != nil {
			res.Missing = append(res.Missing, "tree-decode "+cas.Short(h))
			return
		}
		for _, en := range t.Entries {
			eh, err := cas.ParseHex(en.Hash)
			if err != nil {
				continue
			}
			if en.IsDir {
				walkTree(eh)
				continue
			}
			if seen[eh] {
				continue
			}
			seen[eh] = true
			if !r.CAS.Exists(eh) {
				res.Missing = append(res.Missing, "blob "+cas.Hex(eh))
				continue
			}
			if sz, err := r.CAS.Stat(eh); err == nil {
				res.Bytes += sz
			}
			if en.Chunked {
				res.Manifests++
				m, err := chunker.LoadManifest(r.CAS, eh)
				if err != nil {
					res.Missing = append(res.Missing, "manifest "+cas.Short(eh))
					continue
				}
				for _, ch := range m.Chunks {
					chh, err := cas.ParseHex(ch)
					if err != nil || seen[chh] {
						continue
					}
					seen[chh] = true
					if !r.CAS.Exists(chh) {
						res.Missing = append(res.Missing, "chunk "+ch)
						continue
					}
					if sz, err := r.CAS.Stat(chh); err == nil {
						res.Bytes += sz
					}
					res.Chunks++
				}
			} else {
				res.Blobs++
			}
		}
	}

	stack := append([]cas.Hash{}, tips...)
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		if !r.CAS.Exists(h) {
			res.Missing = append(res.Missing, "commit "+cas.Hex(h))
			continue
		}
		if sz, err := r.CAS.Stat(h); err == nil {
			res.Bytes += sz
		}
		res.Commits++
		c, err := r.DAG.Get(h)
		if err != nil {
			res.Missing = append(res.Missing, "commit-decode "+cas.Short(h))
			continue
		}
		if th, err := cas.ParseHex(c.Tree); err == nil {
			walkTree(th)
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	sort.Strings(res.Missing)
	return res, nil
}

// --- gc ---

// GCResult summarizes a collection pass.
type GCResult struct {
	Examined int
	Removed  int
	Freed    int64
	Kept     int
}

// GC deletes CAS objects unreachable from any ref. With dryRun, only counts.
func GC(r *store.Repo, dryRun bool) (*GCResult, error) {
	res := &GCResult{}
	// Reachable set via fsck-style walk (reuse Fsck's traversal by collecting
	// hashes here with a lighter duplicate walk).
	reachable := map[string]bool{}
	fakeCount := func() {}
	_ = fakeCount
	// Walk roots → commits → trees → blobs (mirror of Fsck, recording hex).
	tips := collectRoots(r)

	mark := func(h cas.Hash) { reachable[strings.ToLower(cas.Hex(h))] = true }
	seenCommit := map[cas.Hash]bool{}
	var walkTree func(h cas.Hash)
	walkTree = func(h cas.Hash) {
		mark(h)
		t, err := merkle.LoadTree(r.CAS, h)
		if err != nil {
			return
		}
		for _, en := range t.Entries {
			eh, err := cas.ParseHex(en.Hash)
			if err != nil {
				continue
			}
			if en.IsDir {
				walkTree(eh)
				continue
			}
			mark(eh)
			if en.Chunked {
				if m, err := chunker.LoadManifest(r.CAS, eh); err == nil {
					for _, ch := range m.Chunks {
						reachable[strings.ToLower(ch)] = true
					}
				}
			}
		}
	}
	stack := append([]cas.Hash{}, tips...)
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seenCommit[h] {
			continue
		}
		seenCommit[h] = true
		mark(h)
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		if th, err := cas.ParseHex(c.Tree); err == nil {
			walkTree(th)
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	// NOTE: blob bytes of domain-separated trees/commits are stored twice
	// (content hash + labeled address). The content-hash twin is technically
	// unreachable by hex walk but must be KEPT while its twin is reachable.
	// Simplest correct rule: keep an object if its hex OR any twin is marked.
	// We approximate twins by content: for each reachable tree/commit address,
	// also mark SHA-256(raw bytes). Bounded: only for small tree/commit objects.
	for hexStr := range reachable {
		h, err := cas.ParseHex(hexStr)
		if err != nil {
			continue
		}
		raw, err := r.CAS.GetBytes(h, 64<<20)
		if err != nil {
			continue
		}
		reachable[strings.ToLower(fmt.Sprintf("%x", sha256Of(raw)))] = true
	}

	// Sweep objects/ fanout.
	objDir := filepath.Join(r.LrmDir, "objects")
	fanouts, err := os.ReadDir(objDir)
	if err != nil {
		return nil, err
	}
	for _, fan := range fanouts {
		if !fan.IsDir() || fan.Name() == "tmp" || len(fan.Name()) != 2 {
			continue
		}
		inners, err := os.ReadDir(filepath.Join(objDir, fan.Name()))
		if err != nil {
			continue
		}
		for _, in := range inners {
			if in.IsDir() {
				continue
			}
			res.Examined++
			hexStr := strings.ToLower(fan.Name() + in.Name())
			if reachable[hexStr] {
				res.Kept++
				continue
			}
			full := filepath.Join(objDir, fan.Name(), in.Name())
			var sz int64
			if fi, err := in.Info(); err == nil {
				sz = fi.Size()
			}
			if !dryRun {
				if err := os.Remove(full); err != nil {
					continue
				}
			}
			res.Removed++
			res.Freed += sz
		}
	}
	return res, nil
}
