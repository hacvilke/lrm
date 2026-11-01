package sync

import (
	"fmt"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/lca"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/replog"
	"github.com/lrm-project/lrm/internal/store"
)

// LocalMergeResult describes a local branch merge.
type LocalMergeResult struct {
	MergedCommit   string
	ConflictBranch string
	FastForwarded  bool
	Message        string
	// Unresolved lists paths that both sides changed. A merge still completes:
	// those files keep the LOCAL version (or the remote one with PreferTheirs)
	// and the remote version stays reachable on the peer branch.
	Unresolved []string
}

// MergeOptions tunes how overlapping paths are resolved.
type MergeOptions struct {
	// PreferTheirs takes the remote version of a conflicting file instead of
	// the local one. Default: keep mine, since the person running the merge is
	// the one who has to live with the file.
	PreferTheirs bool
}

// MergeLocal merges remoteTip into the current branch of r.
func MergeLocal(r *store.Repo, remoteTip cas.Hash, remoteName string) (*LocalMergeResult, error) {
	return MergeLocalOpts(r, remoteTip, remoteName, MergeOptions{})
}

// MergeLocalOpts is MergeLocal with conflict policy.
//
// A single hot file used to abort the whole merge ("resolve manually then
// commit"), which stranded every other file on the peer branch — measured in
// the two-device soak: 48 files transferred, 0 applied, forever. Now the merge
// runs to completion: non-conflicting paths merge normally, conflicting paths
// keep one side (yours by default), and the result says exactly which. Both
// versions stay in the object store and the peer branch is never deleted, so
// nothing can be lost; `--theirs` picks the other side.
func MergeLocalOpts(r *store.Repo, remoteTip cas.Hash, remoteName string, opts MergeOptions) (*LocalMergeResult, error) {
	res := &LocalMergeResult{}
	localTip, localBranch, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	if localTip == cas.Nil {
		if err := r.SetRef(localBranch, cas.Hex(remoteTip)); err != nil {
			return nil, err
		}
		r.AppendReflog(localBranch, "", cas.Hex(remoteTip), "merge", "adopted "+remoteName)
		if err := Checkout(r, remoteTip); err != nil {
			return nil, err
		}
		res.FastForwarded = true
		res.Message = "adopted " + remoteName + " (empty local branch)"
		return res, nil
	}
	if remoteTip == localTip {
		res.Message = "already up to date"
		return res, nil
	}
	base, err := lca.Find(r.DAG, localTip, remoteTip)
	if err != nil || base == cas.Nil {
		return res, fmt.Errorf("histories are disjoint; refusing local merge (sync with peer to create a conflict branch)")
	}
	if base == remoteTip {
		res.Message = "already up to date (" + remoteName + " is an ancestor)"
		return res, nil
	}
	if base == localTip {
		oldHex, _ := r.GetRef(localBranch)
		if err := r.SetRef(localBranch, cas.Hex(remoteTip)); err != nil {
			return nil, err
		}
		r.AppendReflog(localBranch, oldHex, cas.Hex(remoteTip), "merge", "fast-forward to "+remoteName)
		if err := Checkout(r, remoteTip); err != nil {
			return nil, err
		}
		res.FastForwarded = true
		res.Message = "fast-forwarded to " + cas.Short(remoteTip)
		return res, nil
	}
	e := New(r)
	merged, overlapping, err := e.tryMerge(base, localTip, remoteTip)
	if err != nil {
		return nil, err
	}
	if len(overlapping) > 0 {
		// Resolve overlapping paths in favour of one side and carry on.
		lc2, _ := r.DAG.Get(localTip)
		rc2, _ := r.DAG.Get(remoteTip)
		lm, rm := map[string]string{}, map[string]string{}
		if lh, err := cas.ParseHex(lc2.Tree); err == nil {
			_ = merkle.Flatten(r.CAS, lh, "", lm)
		}
		if rh, err := cas.ParseHex(rc2.Tree); err == nil {
			_ = merkle.Flatten(r.CAS, rh, "", rm)
		}
		side := lm
		if opts.PreferTheirs {
			side = rm
		}
		for _, p := range overlapping {
			if h, ok := side[p]; ok {
				merged[p] = h
			} else {
				delete(merged, p) // that side deleted the file
			}
		}
		res.Unresolved = overlapping
	}
	root, err := BuildTreeFromMap(r.CAS, merged)
	if err != nil {
		return nil, err
	}
	lc, _ := r.DAG.Get(localTip)
	rc, _ := r.DAG.Get(remoteTip)
	clock := lc.Clock.Clone()
	clock.Merge(rc.Clock)
	clock.Increment(r.Identity.HexID())
	c := &dag.Commit{
		Version: 1, Tree: cas.Hex(root),
		Parents: []string{cas.Hex(localTip), cas.Hex(remoteTip)},
		Author:  r.Config.User, PeerHex: r.Identity.HexID(),
		Timestamp: time.Now().UnixNano(), Message: "Merge " + remoteName, Clock: clock,
	}
	h, err := r.DAG.Put(c)
	if err != nil {
		return nil, err
	}
	oldHex, _ := r.GetRef(localBranch)
	if err := r.SetRef(localBranch, cas.Hex(h)); err != nil {
		return nil, err
	}
	r.AppendReflog(localBranch, oldHex, cas.Hex(h), "merge", "merged "+remoteName)
	if err := Checkout(r, h); err != nil {
		return nil, err
	}
	res.MergedCommit = cas.Hex(h)
	res.Message = "merged " + remoteName + " → " + cas.Short(h)
	if len(res.Unresolved) > 0 {
		which := "your version"
		if opts.PreferTheirs {
			which = remoteName + "'s version"
		}
		res.Message = fmt.Sprintf("merged %s → %s (%d overlapping file(s) kept %s: %s; the other side is still on the peer branch)",
			remoteName, cas.Short(h), len(res.Unresolved), which, strings.Join(res.Unresolved, ", "))
	}
	_, _ = r.Replog.Append(replog.Entry{Type: replog.TypeMerge, PeerHex: r.Identity.HexID(), Commit: cas.Hex(h), Message: res.Message})
	return res, nil
}
