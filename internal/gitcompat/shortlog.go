package gitcompat

import (
	"sort"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// AuthorCount pairs an author with their commit count.
type AuthorCount struct {
	Author  string
	Commits int
}

// Shortlog counts commits per author over tip's history (limit <= 0: all).
// Sorted by count desc, author asc (deterministic).
func Shortlog(r *store.Repo, tip cas.Hash, limit int) ([]AuthorCount, error) {
	_, commits, err := r.DAG.WalkTipOrder(tip, limit)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, c := range commits {
		counts[c.Author]++
	}
	out := make([]AuthorCount, 0, len(counts))
	for a, n := range counts {
		out = append(out, AuthorCount{Author: a, Commits: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Commits != out[j].Commits {
			return out[i].Commits > out[j].Commits
		}
		return out[i].Author < out[j].Author
	})
	return out, nil
}
