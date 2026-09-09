package gitcompat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// PickResult describes a cherry-picked commit.
type PickResult struct {
	Hash    string
	Message string
}

// CherryPick replays target onto the current branch tip using file-level
// 3-way merge (base = target's first parent, or the empty tree for a root
// or merge-picked commit's first parent). The workdir must be clean;
// conflicting paths abort with no changes (same rule as local merge).
func CherryPick(r *store.Repo, target cas.Hash) (*PickResult, error) {
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return nil, err
	}
	if !res.Clean {
		return nil, fmt.Errorf("workdir dirty (%d change(s)): commit or stash first", len(res.Changes))
	}
	tip, branch, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	if tip == cas.Nil {
		return nil, fmt.Errorf("no commits yet (nothing to pick onto)")
	}
	if target == tip {
		return nil, fmt.Errorf("already at %s", cas.Short(target))
	}
	tc, err := r.DAG.Get(target)
	if err != nil {
		return nil, fmt.Errorf("not a commit: %s", cas.Short(target))
	}
	base := map[string]string{}
	if len(tc.Parents) > 0 {
		bh, err := cas.ParseHex(tc.Parents[0])
		if err != nil {
			return nil, err
		}
		base, err = TreeAtCommit(r, bh)
		if err != nil {
			return nil, err
		}
	}
	ours, err := TreeAtCommit(r, tip)
	if err != nil {
		return nil, err
	}
	theirs, err := TreeAtCommit(r, target)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	merged := map[string]string{}
	var conflicts []string
	for p := range paths {
		b, o, t := base[p], ours[p], theirs[p]
		bEx, oEx, tEx := hasKey(base, p), hasKey(ours, p), hasKey(theirs, p)
		switch {
		case oEx == tEx && o == t:
			if oEx { // identical (or both deleted)
				merged[p] = o
			}
		case oEx == bEx && o == b:
			if tEx { // unchanged locally → take theirs
				merged[p] = t
			}
		case tEx == bEx && t == b:
			if oEx { // unchanged in pick → keep ours
				merged[p] = o
			}
		default:
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fmt.Errorf("cherry-pick conflicts in %d file(s): %v — resolve manually then commit", len(conflicts), conflicts)
	}
	root, err := sync.BuildTreeFromMap(r.CAS, merged)
	if err != nil {
		return nil, err
	}
	msg := strings.TrimSpace(tc.Message) + "\n\ncherry-picked-from: " + cas.Hex(target)[:12]
	h, _, err := r.Commit(msg, root)
	if err != nil {
		return nil, err
	}
	if err := sync.Checkout(r, h); err != nil {
		return nil, err
	}
	return &PickResult{Hash: cas.Hex(h),
		Message: fmt.Sprintf("cherry-picked %s onto %s → %s", cas.Short(target), branch, cas.Short(h))}, nil
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
