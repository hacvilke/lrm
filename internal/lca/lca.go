// Package lca finds the Lowest Common Ancestor (merge-base) of two tips.
//
// Used by the sync engine to determine exactly where two developers'
// histories diverged, enabling optimal fetch (only missing objects) and
// conflict detection.
package lca

import (
	"fmt"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
)

// Find returns the lowest common ancestor of a and b (best merge-base).
// If one tip is an ancestor of the other, that ancestor is returned.
// If histories are disjoint, returns Nil + ErrNoCommonAncestor.
func Find(store *dag.Store, a, b cas.Hash) (cas.Hash, error) {
	if a == b {
		return a, nil
	}
	if a == cas.Nil {
		return b, nil
	}
	if b == cas.Nil {
		return a, nil
	}
	// Collect ancestors of both tips with depths (distance from tip).
	depthA, err := ancestorDepths(store, a)
	if err != nil {
		return cas.Nil, err
	}
	depthB, err := ancestorDepths(store, b)
	if err != nil {
		return cas.Nil, err
	}
	// Fast paths: one tip is an ancestor of the other.
	if _, ok := depthA[b]; ok {
		return b, nil // b is ancestor of a
	}
	if _, ok := depthB[a]; ok {
		return a, nil // a is ancestor of b
	}
	// Common ancestors = intersection.
	var common []cas.Hash
	for h := range depthA {
		if _, ok := depthB[h]; ok {
			common = append(common, h)
		}
	}
	if len(common) == 0 {
		return cas.Nil, ErrNoCommonAncestor
	}
	if len(common) == 1 {
		return common[0], nil
	}
	// Lowest = common ancestors that are NOT ancestors of any other common
	// ancestor (i.e. maximal elements = closest to the tips).
	commonSet := map[cas.Hash]bool{}
	for _, h := range common {
		commonSet[h] = true
	}
	isAncestorOfAnother := func(cand cas.Hash) bool {
		// Is cand reachable from any other common candidate?
		for _, other := range common {
			if other == cand {
				continue
			}
			if reaches(store, other, cand, commonSet) {
				return true
			}
		}
		return false
	}
	var lowest []cas.Hash
	for _, h := range common {
		if !isAncestorOfAnother(h) {
			lowest = append(lowest, h)
		}
	}
	if len(lowest) == 0 {
		lowest = common
	}
	// Tie-break (criss-cross merges): closest to both tips.
	best := lowest[0]
	bestScore := depthA[best] + depthB[best]
	for _, h := range lowest[1:] {
		if s := depthA[h] + depthB[h]; s < bestScore {
			best, bestScore = h, s
		}
	}
	return best, nil
}

// reaches reports whether target is reachable from start (bounded walk).
func reaches(store *dag.Store, start, target cas.Hash, limit map[cas.Hash]bool) bool {
	if start == target {
		return true
	}
	seen := map[cas.Hash]bool{start: true}
	stack := []cas.Hash{start}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c, err := store.Get(h)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			ph, err := cas.ParseHex(p)
			if err != nil || seen[ph] {
				continue
			}
			if ph == target {
				return true
			}
			seen[ph] = true
			stack = append(stack, ph)
		}
	}
	return false
}

// ErrNoCommonAncestor indicates disjoint histories.
var ErrNoCommonAncestor = errNoAncestor{}

type errNoAncestor struct{}

func (errNoAncestor) Error() string { return "no common ancestor" }

// IsAncestor reports whether anc is reachable from desc (inclusive).
func IsAncestor(store *dag.Store, anc, desc cas.Hash) (bool, error) {
	if anc == desc {
		return true, nil
	}
	if anc == cas.Nil {
		return true, nil
	}
	seen := map[cas.Hash]bool{}
	stack := []cas.Hash{desc}
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
		c, err := store.Get(h)
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

// Diverged lists commits on tip that are NOT reachable from base (i.e. the
// "new" side after LCA), in BFS order. Used to compute minimal fetch sets.
func Diverged(store *dag.Store, base, tip cas.Hash) ([]cas.Hash, error) {
	if tip == cas.Nil {
		return nil, nil
	}
	baseReach := map[cas.Hash]bool{}
	if base != cas.Nil {
		stack := []cas.Hash{base}
		for len(stack) > 0 {
			h := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if baseReach[h] {
				continue
			}
			baseReach[h] = true
			c, err := store.Get(h)
			if err != nil {
				continue
			}
			for _, p := range c.Parents {
				if ph, err := cas.ParseHex(p); err == nil {
					stack = append(stack, ph)
				}
			}
		}
	}
	var out []cas.Hash
	seen := map[cas.Hash]bool{}
	queue := []cas.Hash{tip}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if seen[h] || baseReach[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
		c, err := store.Get(h)
		if err != nil {
			return nil, fmt.Errorf("load commit %s: %w", cas.Short(h), err)
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil && !seen[ph] {
				queue = append(queue, ph)
			}
		}
	}
	return out, nil
}

func ancestorDepths(store *dag.Store, tip cas.Hash) (map[cas.Hash]int, error) {
	depths := map[cas.Hash]int{tip: 0}
	queue := []cas.Hash{tip}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		c, err := store.Get(h)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			ph, err := cas.ParseHex(p)
			if err != nil {
				continue
			}
			if _, ok := depths[ph]; !ok {
				depths[ph] = depths[h] + 1
				queue = append(queue, ph)
			}
		}
	}
	return depths, nil
}
