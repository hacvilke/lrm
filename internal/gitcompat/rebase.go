package gitcompat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// Rebase replays the current branch's unique commits onto a new upstream
// (git rebase, linear history only). Replay happens on a scratch branch so
// the real branch moves exactly once, at the end — which makes --abort a
// pure restore: the branch never moved, so nothing needs undoing.
//
// Rebased commits keep their ORIGINAL messages (no trailer): a rebase
// rewrites history, it does not annotate it.

// RebaseBranch is the scratch branch replay commits land on.
const RebaseBranch = "rebase-scratch"

// RebaseState tracks an in-progress (possibly conflict-stopped) rebase.
type RebaseState struct {
	Branch   string   // original branch being rebased
	OrigTip  string   // its tip before the rebase (full hex)
	Upstream string   // new base (full hex)
	Pending  []string // original commits left to replay, oldest-first
	Done     []string // original commits already replayed, oldest-first
}

// RebaseResult reports a finished step.
type RebaseResult struct {
	Message  string
	Stopped  bool     // conflict stop: resolve, then --continue or --abort
	Conflict string   // short hash of the stopping commit (when Stopped)
	Files    []string // conflicting paths (when Stopped)
}

func rebaseStatePath(r *store.Repo) string { return filepath.Join(r.LrmDir, "REBASE_STATE") }

func saveRebaseState(r *store.Repo, st *RebaseState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(rebaseStatePath(r), raw, 0o644)
}

func loadRebaseState(r *store.Repo) (*RebaseState, error) {
	raw, err := os.ReadFile(rebaseStatePath(r))
	if err != nil {
		return nil, fmt.Errorf("no rebase in progress")
	}
	var st RebaseState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("corrupt rebase state: %w", err)
	}
	if st.Branch == "" || st.OrigTip == "" {
		return nil, fmt.Errorf("corrupt rebase state (missing branch/tip)")
	}
	return &st, nil
}

// RebaseStart replays the current branch onto upstream. Returns Stopped=true
// when a commit conflicts (state saved for --continue/--abort).
func RebaseStart(r *store.Repo, upstream cas.Hash) (*RebaseResult, error) {
	if _, err := os.Stat(rebaseStatePath(r)); err == nil {
		return nil, fmt.Errorf("rebase already in progress (resolve, then `lrm rebase --continue`, or `lrm rebase --abort`)")
	}
	scan, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return nil, err
	}
	if !scan.Clean {
		return nil, fmt.Errorf("workdir dirty (%d change(s)): commit or stash first", len(scan.Changes))
	}
	tip, branch, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	if tip == cas.Nil {
		return nil, fmt.Errorf("no commits yet (nothing to rebase)")
	}
	if _, err := r.DAG.Get(upstream); err != nil {
		return nil, fmt.Errorf("unknown upstream commit %s", cas.Short(upstream))
	}
	if upstream == tip {
		return &RebaseResult{Message: fmt.Sprintf("%s is already up to date with %s", branch, cas.Short(upstream))}, nil
	}
	if anc, _ := isAncestorOf(r, upstream, tip); anc {
		return &RebaseResult{Message: fmt.Sprintf("%s is already up to date with %s", branch, cas.Short(upstream))}, nil
	}
	chain, err := rebaseChain(r, tip, upstream)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return &RebaseResult{Message: fmt.Sprintf("%s is already up to date with %s", branch, cas.Short(upstream))}, nil
	}
	if ref, _ := r.GetRef(RebaseBranch); ref != "" {
		return nil, fmt.Errorf("scratch branch %q already exists (stale? delete .lrm/refs/heads/%s)", RebaseBranch, RebaseBranch)
	}
	if err := r.SetRef(RebaseBranch, cas.Hex(upstream)); err != nil {
		return nil, err
	}
	r.AppendReflog(RebaseBranch, "", cas.Hex(upstream), "rebase", "scratch at upstream "+cas.Short(upstream))
	st := &RebaseState{Branch: branch, OrigTip: cas.Hex(tip), Upstream: cas.Hex(upstream)}
	for _, h := range chain {
		st.Pending = append(st.Pending, cas.Hex(h))
	}
	if err := saveRebaseState(r, st); err != nil {
		return nil, err
	}
	if err := r.SetHeadBranch(RebaseBranch); err != nil {
		return nil, err
	}
	if err := sync.Checkout(r, upstream); err != nil {
		return nil, err
	}
	return rebaseReplay(r, st)
}

// isAncestorOf reports whether anc is reachable from tip (all parents).
func isAncestorOf(r *store.Repo, anc, tip cas.Hash) (bool, error) {
	seen := map[cas.Hash]bool{}
	stack := []cas.Hash{tip}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if h == anc {
			return true, nil
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	return false, nil
}

// rebaseChain returns branch-unique commits oldest-first by walking
// first-parents from tip until an upstream-reachable commit. Merge commits
// on the path refuse: linear history only.
func rebaseChain(r *store.Repo, tip, upstream cas.Hash) ([]cas.Hash, error) {
	reach := map[cas.Hash]bool{}
	stack := []cas.Hash{upstream}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reach[h] {
			continue
		}
		reach[h] = true
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	var rev []cas.Hash
	h := tip
	for {
		if reach[h] {
			break
		}
		c, err := r.DAG.Get(h)
		if err != nil {
			return nil, fmt.Errorf("history walk hit missing commit %s", cas.Short(h))
		}
		if len(c.Parents) > 1 {
			return nil, fmt.Errorf("cannot rebase merge commit %s (linear history only — rebase one side first)", cas.Short(h))
		}
		rev = append(rev, h)
		if len(c.Parents) == 0 {
			break // root: unrelated histories replay wholesale
		}
		nh, err := cas.ParseHex(c.Parents[0])
		if err != nil {
			return nil, err
		}
		h = nh
	}
	chain := make([]cas.Hash, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		chain = append(chain, rev[i])
	}
	return chain, nil
}

// rebaseReplay applies every pending commit with its original message.
// A conflict saves state and stops (not an error: a pause for the human).
func rebaseReplay(r *store.Repo, st *RebaseState) (*RebaseResult, error) {
	for len(st.Pending) > 0 {
		target, err := cas.ParseHex(st.Pending[0])
		if err != nil {
			return nil, err
		}
		tc, err := r.DAG.Get(target)
		if err != nil {
			return nil, err
		}
		if _, err := applyPick(r, target, tc.Message, "rebase"); err != nil {
			if pc := asPickConflict(err); pc != nil {
				_ = saveRebaseState(r, st)
				return &RebaseResult{
					Stopped:  true,
					Conflict: cas.Short(target),
					Files:    pc.Files,
					Message: fmt.Sprintf("stopped at %s (%s): %d conflicting file(s): %v\nresolve in the workdir, then `lrm rebase --continue` (or `lrm rebase --abort`)",
						cas.Short(target), firstLineOf(tc.Message), len(pc.Files), pc.Files),
				}, nil
			}
			_ = saveRebaseState(r, st)
			return nil, err
		}
		st.Done = append(st.Done, st.Pending[0])
		st.Pending = st.Pending[1:]
		_ = saveRebaseState(r, st)
	}
	return rebaseFinish(r, st)
}

// rebaseFinish moves the real branch onto the replayed tip and cleans up.
func rebaseFinish(r *store.Repo, st *RebaseState) (*RebaseResult, error) {
	newTip, _, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	if err := r.SetRef(st.Branch, cas.Hex(newTip)); err != nil {
		return nil, err
	}
	upShort := st.Upstream
	if len(upShort) > 8 {
		upShort = upShort[:8]
	}
	r.AppendReflog(st.Branch, st.OrigTip, cas.Hex(newTip), "rebase",
		fmt.Sprintf("%d commit(s) onto %s", len(st.Done), upShort))
	if err := r.SetHeadBranch(st.Branch); err != nil {
		return nil, err
	}
	_ = os.Remove(filepath.Join(r.LrmDir, "refs", "heads", RebaseBranch))
	_ = os.Remove(rebaseStatePath(r))
	if err := sync.Checkout(r, newTip); err != nil {
		return nil, err
	}
	return &RebaseResult{Message: fmt.Sprintf("rebased %s onto %s: %d commit(s) replayed → %s",
		st.Branch, upShort, len(st.Done), cas.Short(newTip))}, nil
}

// RebaseContinue commits the current workdir tree as the resolution of the
// stopping commit (original message kept) and resumes the replay. A tree
// identical to the scratch tip means "nothing to resolve" — the commit is
// skipped instead of recorded empty.
func RebaseContinue(r *store.Repo) (*RebaseResult, error) {
	st, err := loadRebaseState(r)
	if err != nil {
		return nil, err
	}
	if len(st.Pending) == 0 {
		return nil, fmt.Errorf("rebase state has nothing pending (stale? `lrm rebase --abort` to clean up)")
	}
	br, err := r.HeadBranch()
	if err != nil {
		return nil, err
	}
	if br != RebaseBranch && br != st.Branch {
		return nil, fmt.Errorf("HEAD is on %q — return to %q or %q first", br, RebaseBranch, st.Branch)
	}
	if err := r.SetHeadBranch(RebaseBranch); err != nil {
		return nil, err
	}
	target, err := cas.ParseHex(st.Pending[0])
	if err != nil {
		return nil, err
	}
	tc, err := r.DAG.Get(target)
	if err != nil {
		return nil, err
	}
	scan, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return nil, err
	}
	scratchHex, _ := r.GetRef(RebaseBranch)
	if sh, err := cas.ParseHex(scratchHex); err == nil {
		if sc, err := r.DAG.Get(sh); err == nil && sc.Tree == cas.Hex(scan.RootHash) {
			st.Done = append(st.Done, st.Pending[0])
			st.Pending = st.Pending[1:]
			_ = saveRebaseState(r, st)
			res, err := rebaseReplay(r, st)
			if err != nil {
				return nil, err
			}
			res.Message = fmt.Sprintf("skipped %s (no changes after resolve)\n%s", cas.Short(target), res.Message)
			return res, nil
		}
	}
	if _, _, err := r.Commit(tc.Message, scan.RootHash); err != nil {
		return nil, err
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, scan.RootHash)
	_ = r.Index.Save()
	st.Done = append(st.Done, st.Pending[0])
	st.Pending = st.Pending[1:]
	_ = saveRebaseState(r, st)
	return rebaseReplay(r, st)
}

// RebaseAbort restores the original branch tip (it never moved — only the
// scratch branch did) and removes all rebase traces.
func RebaseAbort(r *store.Repo) (*RebaseResult, error) {
	st, err := loadRebaseState(r)
	if err != nil {
		return nil, err
	}
	orig, err := cas.ParseHex(st.OrigTip)
	if err != nil {
		return nil, err
	}
	if err := r.SetHeadBranch(st.Branch); err != nil {
		return nil, err
	}
	_ = os.Remove(filepath.Join(r.LrmDir, "refs", "heads", RebaseBranch))
	_ = os.Remove(rebaseStatePath(r))
	if err := sync.Checkout(r, orig); err != nil {
		return nil, err
	}
	r.AppendReflog(st.Branch, st.OrigTip, st.OrigTip, "rebase --abort", "restored "+cas.Short(orig))
	return &RebaseResult{Message: fmt.Sprintf("rebase aborted — %s restored to %s", st.Branch, cas.Short(orig))}, nil
}
