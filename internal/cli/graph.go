package cli

import (
	"fmt"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/store"
)

// GraphOrder returns commits reachable from tip in topological order
// (children before parents, DFS-based, deterministic), bounded by limit
// (<=0 means all). Also returns a lookup map for rendering.
func GraphOrder(r *store.Repo, tip cas.Hash, limit int) ([]cas.Hash, map[cas.Hash]*dag.Commit, error) {
	lookup := map[cas.Hash]*dag.Commit{}
	var post []cas.Hash // post-order (parents before children)
	seen := map[cas.Hash]bool{}
	var visit func(h cas.Hash) error
	visit = func(h cas.Hash) error {
		if seen[h] {
			return nil
		}
		seen[h] = true
		c, err := r.DAG.Get(h)
		if err != nil {
			return err
		}
		lookup[h] = c
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				if err := visit(ph); err != nil {
					return err
				}
			}
		}
		post = append(post, h)
		return nil
	}
	if err := visit(tip); err != nil {
		return nil, nil, err
	}
	// Reverse post-order => children before parents (topological).
	order := make([]cas.Hash, 0, len(post))
	for i := len(post) - 1; i >= 0; i-- {
		order = append(order, post[i])
	}
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	return order, lookup, nil
}

// RenderGraph renders order (from GraphOrder) as ASCII art, git-log---graph
// style (simplified: no interstitial merge-slash lines, joins collapse).
func RenderGraph(order []cas.Hash, lookup map[cas.Hash]*dag.Commit) []string {
	var rows []string
	var pending []cas.Hash // active columns
	inSet := map[cas.Hash]bool{}
	indexOf := func(h cas.Hash) int {
		for i, ph := range pending {
			if ph == h {
				return i
			}
		}
		return -1
	}
	allowed := map[cas.Hash]bool{}
	for _, h := range order {
		allowed[h] = true
	}
	for _, h := range order {
		col := indexOf(h)
		if col < 0 {
			col = len(pending)
			pending = append(pending, h)
			inSet[h] = true
		}
		// Row prefix: "*" at our column, "|" elsewhere.
		var sb strings.Builder
		for i := range pending {
			if i == col {
				sb.WriteString("* ")
			} else {
				sb.WriteString("| ")
			}
		}
		c := lookup[h]
		msg := "(missing)"
		parents := []string{}
		if c != nil {
			msg = firstLine(c.Message)
			parents = c.Parents
		}
		fmt.Fprintf(&sb, "%s %s", cas.Short(h), msg)
		rows = append(rows, sb.String())
		if len(parents) > 1 {
			var mb strings.Builder
			for range pending {
				mb.WriteString("| ")
			}
			shorts := make([]string, 0, len(parents))
			for _, p := range parents {
				shorts = append(shorts, shortHex(p))
			}
			fmt.Fprintf(&mb, "Merge: %s", strings.Join(shorts, " "))
			rows = append(rows, mb.String())
		}
		// Advance frontier: drop our column, insert parents.
		pending = append(pending[:col], pending[col+1:]...)
		delete(inSet, h)
		insertAt := col
		for pi, p := range parents {
			ph, err := cas.ParseHex(p)
			if err != nil || !allowed[ph] {
				continue
			}
			if j := indexOf(ph); j >= 0 {
				continue // join: already an active column
			}
			if pi == 0 {
				// First parent takes our column.
				pending = append(pending, cas.Nil)
				copy(pending[insertAt+1:], pending[insertAt:])
				pending[insertAt] = ph
				insertAt++
			} else {
				pending = append(pending, ph)
			}
			inSet[ph] = true
		}
	}
	return rows
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
