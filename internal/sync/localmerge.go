package sync

import (
	"fmt"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/lca"
	"github.com/lrm-project/lrm/internal/replog"
	"github.com/lrm-project/lrm/internal/store"
)

// LocalMergeResult describes a local branch merge.
type LocalMergeResult struct {
	MergedCommit   string
	ConflictBranch string
	FastForwarded  bool
	Message        string
}

// MergeLocal merges remoteTip into the current branch of r.
func MergeLocal(r *store.Repo, remoteTip cas.Hash, remoteName string) (*LocalMergeResult, error) {
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
		return res, fmt.Errorf("merge conflicts in %d file(s): %v — resolve manually then commit", len(overlapping), overlapping)
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
		Author: r.Config.User, PeerHex: r.Identity.HexID(),
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
	_, _ = r.Replog.Append(replog.Entry{Type: replog.TypeMerge, PeerHex: r.Identity.HexID(), Commit: cas.Hex(h), Message: res.Message})
	return res, nil
}
