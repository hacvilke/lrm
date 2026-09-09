package gitcompat

import (
	"fmt"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/replog"
	"github.com/lrm-project/lrm/internal/store"
)

// Amend replaces the tip commit with the current workdir state, keeping
// the tip's parents (history stays a DAG — the old tip simply becomes
// unreferenced). message == "" keeps the tip's message.
func Amend(r *store.Repo, message string) (cas.Hash, error) {
	tip, branch, err := r.HeadCommit()
	if err != nil {
		return cas.Nil, err
	}
	if tip == cas.Nil {
		return cas.Nil, fmt.Errorf("no commits yet (nothing to amend)")
	}
	tipC, err := r.DAG.Get(tip)
	if err != nil {
		return cas.Nil, err
	}
	if message == "" {
		message = tipC.Message
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return cas.Nil, err
	}
	if res.Clean && message == tipC.Message {
		return cas.Nil, fmt.Errorf("nothing to amend (workdir clean, message unchanged)")
	}
	clock := tipC.Clock.Clone()
	clock.Increment(r.Identity.HexID())
	c := &dag.Commit{
		Version: 1, Tree: cas.Hex(res.RootHash), Parents: append([]string{}, tipC.Parents...),
		Author: r.Config.User, PeerHex: r.Identity.HexID(),
		Timestamp: time.Now().UnixNano(), Message: message, Clock: clock,
	}
	h, err := r.DAG.Put(c)
	if err != nil {
		return cas.Nil, err
	}
	if err := r.SetRef(branch, cas.Hex(h)); err != nil {
		return cas.Nil, err
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	_ = r.Index.Save()
	_, _ = r.Replog.Append(replog.Entry{
		Type: replog.TypeCommit, PeerHex: r.Identity.HexID(),
		Commit: cas.Hex(h), Message: "amend: " + message, Clock: clock.Clone(),
		Extra: map[string]string{"branch": branch, "amended": cas.Hex(tip)},
	})
	return h, nil
}
