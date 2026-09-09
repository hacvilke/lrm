package gitcompat

import (
	"fmt"
	"sort"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// Describe names commit after the nearest reachable tag:
// "<tag>" when exact, else "<tag>-<distance>-g<short12>".
// BFS over all parents; ties break by lexicographically smallest tag
// (deterministic regardless of traversal order).
func Describe(r *store.Repo, commit cas.Hash) (string, error) {
	tags, err := r.ListTags()
	if err != nil {
		return "", err
	}
	byTarget := map[string][]string{}
	for name, hx := range tags {
		byTarget[hx] = append(byTarget[hx], name)
	}
	type item struct {
		h     cas.Hash
		depth int
	}
	seen := map[cas.Hash]bool{commit: true}
	queue := []item{{commit, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if names, ok := byTarget[cas.Hex(cur.h)]; ok {
			sort.Strings(names)
			if cur.depth == 0 {
				return names[0], nil
			}
			return fmt.Sprintf("%s-%d-g%s", names[0], cur.depth, cas.Hex(commit)[:12]), nil
		}
		c, err := r.DAG.Get(cur.h)
		if err != nil {
			continue
		}
		for _, ph := range c.Parents {
			h, err := cas.ParseHex(ph)
			if err != nil || seen[h] {
				continue
			}
			seen[h] = true
			queue = append(queue, item{h, cur.depth + 1})
		}
	}
	return "", fmt.Errorf("no tag reachable from %s", cas.Short(commit))
}
