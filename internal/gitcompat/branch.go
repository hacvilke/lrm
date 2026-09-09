package gitcompat

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/lca"
	"github.com/lrm-project/lrm/internal/store"
)

// BranchDeleteResult reports a deleted branch (tip kept for recovery hints).
type BranchDeleteResult struct {
	Name string
	Tip  string // full hex ("" when the branch was unborn)
}

// DeleteBranch removes a branch ref. Safe mode (force=false) refuses tips
// not merged into the current branch — nothing reachable only from the
// deleted branch may be lost. The branch's ref-log is KEPT (with a final
// deletion entry), so `ref-log <name>` still shows the old tip for recovery.
func DeleteBranch(r *store.Repo, name string, force bool) (*BranchDeleteResult, error) {
	branches, err := r.ListBranches()
	if err != nil {
		return nil, err
	}
	exists := containsStr(branches, name)
	if !exists {
		return nil, fmt.Errorf("branch %q not found", name)
	}
	cur, err := r.HeadBranch()
	if err != nil {
		return nil, err
	}
	if name == cur {
		return nil, fmt.Errorf("cannot delete the branch you're on (%q — switch first)", name)
	}
	if name == RebaseBranch {
		if _, err := os.Stat(rebaseStatePath(r)); err == nil {
			return nil, fmt.Errorf("cannot delete %q: a rebase is in progress (--continue or --abort first)", name)
		}
	}
	if name == ScratchBranch && BisectActive(r) {
		return nil, fmt.Errorf("cannot delete %q: a bisect is in progress (reset first)", name)
	}
	tipHex, err := r.GetRef(name)
	if err != nil {
		return nil, err
	}
	if !force && tipHex != "" {
		tip, err := cas.ParseHex(tipHex)
		if err != nil {
			return nil, fmt.Errorf("branch %q has a corrupt tip ref (use -D to delete anyway)", name)
		}
		headTip, headBr, err := r.HeadCommit()
		if err != nil {
			return nil, err
		}
		merged := false
		if headTip != cas.Nil {
			merged, _ = isAncestorOf(r, tip, headTip)
		}
		if !merged {
			return nil, fmt.Errorf("branch %q is not merged into %q (use -D to delete anyway)", name, headBr)
		}
	}
	detail := "deleted (was unborn)"
	if tipHex != "" {
		detail = "deleted (was " + shortHexOf(tipHex) + ")"
		r.AppendReflog(name, tipHex, store.ZeroHex, "branch -d", detail)
	}
	if err := os.Remove(filepath.Join(r.LrmDir, "refs", "heads", name)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &BranchDeleteResult{Name: name, Tip: tipHex}, nil
}

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func shortHexOf(hexStr string) string {
	if len(hexStr) > 12 {
		return hexStr[:12]
	}
	return hexStr
}

// MergeBase resolves the best common ancestor of a and b (LCA).
func MergeBase(r *store.Repo, a, b cas.Hash) (cas.Hash, error) {
	base, err := lca.Find(r.DAG, a, b)
	if err != nil || base == cas.Nil {
		return cas.Nil, fmt.Errorf("no merge base (disjoint histories)")
	}
	return base, nil
}
