package gitcompat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// CherryEntry marks one head-unique commit: '+' means upstream lacks it,
// '-' means an equivalent commit is already upstream.
type CherryEntry struct {
	Applied bool   // true → '-', false → '+'
	Commit  string // full hex
	Subject string // first message line
}

// Cherry lists commits reachable from head but not from upstream,
// oldest-first. Equivalence is content+subject equality (a file-level
// patch-id-lite: rebase and cherry-pick copies share both — tree hashes
// alone would miss, since map-built trees carry different entry metadata
// than scan-built ones).
func Cherry(r *store.Repo, upstream, head cas.Hash) ([]CherryEntry, error) {
	if !r.DAG.Has(upstream) {
		return nil, fmt.Errorf("unknown commit %s", cas.Short(upstream))
	}
	if !r.DAG.Has(head) {
		return nil, fmt.Errorf("unknown commit %s", cas.Short(head))
	}
	// Upstream reachability + equivalence signatures.
	upReach := map[cas.Hash]bool{}
	upSig := map[string]bool{}
	sigOf := newCherrySig(r)
	stack := []cas.Hash{upstream}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if upReach[h] {
			continue
		}
		upReach[h] = true
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		upSig[sigOf.sig(h, c)] = true
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	// Head-minus-upstream set.
	mine := map[cas.Hash]bool{}
	stack = []cas.Hash{head}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if mine[h] || upReach[h] {
			continue
		}
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		mine[h] = true
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil {
				stack = append(stack, ph)
			}
		}
	}
	// Kahn topo order (oldest-first), hex-sorted ready list for determinism.
	inSet := func(h cas.Hash) bool { return mine[h] }
	indeg := map[cas.Hash]int{}
	children := map[cas.Hash][]cas.Hash{}
	subjects := map[cas.Hash]string{}
	for h := range mine {
		c, err := r.DAG.Get(h)
		if err != nil {
			continue
		}
		subjects[h] = firstLineOf(c.Message)
		for _, p := range c.Parents {
			ph, err := cas.ParseHex(p)
			if err != nil || !inSet(ph) {
				continue
			}
			indeg[h]++
			children[ph] = append(children[ph], h)
		}
	}
	var ready []cas.Hash
	for h := range mine {
		if indeg[h] == 0 {
			ready = append(ready, h)
		}
	}
	var out []CherryEntry
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return cas.Hex(ready[i]) < cas.Hex(ready[j]) })
		h := ready[0]
		ready = ready[1:]
		c, _ := r.DAG.Get(h)
		applied := false
		if c != nil {
			applied = upSig[sigOf.sig(h, c)]
		}
		out = append(out, CherryEntry{Applied: applied, Commit: cas.Hex(h), Subject: subjects[h]})
		for _, ch := range children[h] {
			indeg[ch]--
			if indeg[ch] == 0 {
				ready = append(ready, ch)
			}
		}
	}
	return out, nil
}

// cherrySig memoizes content+subject equivalence signatures.
type cherrySig struct {
	r    *store.Repo
	memo map[cas.Hash]string
}

func newCherrySig(r *store.Repo) *cherrySig {
	return &cherrySig{r: r, memo: map[cas.Hash]string{}}
}

func (s *cherrySig) sig(h cas.Hash, c *dag.Commit) string {
	if sig, ok := s.memo[h]; ok {
		return sig
	}
	sig := "tree:" + c.Tree // fallback when the tree won't flatten
	if th, err := cas.ParseHex(c.Tree); err == nil {
		flat := map[string]string{}
		if err := merkle.Flatten(s.r.CAS, th, "", flat); err == nil {
			paths := make([]string, 0, len(flat))
			for p := range flat {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			var sb strings.Builder
			for _, p := range paths {
				sb.WriteString(p)
				sb.WriteByte(0)
				sb.WriteString(flat[p])
				sb.WriteByte(0)
			}
			sig = fmt.Sprintf("%x", sha256Of([]byte(sb.String())))
		}
	}
	sig += "\n" + firstLineOf(c.Message)
	s.memo[h] = sig
	return sig
}
