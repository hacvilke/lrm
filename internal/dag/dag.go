// Package dag implements the commit Direct Acyclic Graph (DAG).
//
// Commits are content-addressed (SHA-256 over canonical JSON) and stored
// as CAS objects. Each commit carries a vector clock so concurrent histories
// can be ordered / detected as divergent.
package dag

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/vectorclock"
)

// Commit is an immutable history node.
type Commit struct {
	Version   int               `json:"version"`
	Tree      string            `json:"tree"`    // hex root hash
	Parents   []string          `json:"parents"` // hex commit hashes
	Author    string            `json:"author"`
	PeerHex   string            `json:"peer"` // author PeerID hex
	Timestamp int64             `json:"timestamp"`
	Message   string            `json:"message"`
	Clock     vectorclock.Clock `json:"clock"`
}

// Hash returns the commit's content address.
func (c *Commit) Hash() cas.Hash {
	raw, _ := json.Marshal(c)
	typed := append([]byte("lrm-commit-v1\n"), raw...)
	return sha256.Sum256(typed)
}

// Store persists commits in a CAS plus in-memory index helpers.
type Store struct {
	cas *cas.Store
}

// New creates a DAG store over a CAS.
func New(c *cas.Store) *Store { return &Store{cas: c} }

// CAS exposes the underlying store.
func (s *Store) CAS() *cas.Store { return s.cas }

// Put serializes + stores a commit, returning its hash.
func (s *Store) Put(c *Commit) (cas.Hash, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return cas.Nil, err
	}
	h := c.Hash()
	if s.cas.Exists(h) {
		return h, nil
	}
	// Store raw bytes at domain-separated address.
	got, err := s.cas.PutBytes(raw)
	if err != nil {
		return cas.Nil, err
	}
	if got == h {
		return h, nil
	}
	// Copy blob bytes to the domain-separated address.
	rc, err := s.cas.Get(got)
	if err != nil {
		return cas.Nil, err
	}
	defer rc.Close()
	// Re-store via temp file at explicit path.
	return h, copyObjectTo(s.cas, h, rc)
}

// Get loads a commit by hash.
func (s *Store) Get(h cas.Hash) (*Commit, error) {
	raw, err := s.cas.GetBytes(h, 4<<20)
	if err != nil {
		return nil, err
	}
	var c Commit
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode commit %s: %w", cas.Short(h), err)
	}
	return &c, nil
}

// Has reports presence.
func (s *Store) Has(h cas.Hash) bool { return s.cas.Exists(h) }

// Parents resolves parent hashes.
func (s *Store) Parents(c *Commit) ([]cas.Hash, error) {
	out := make([]cas.Hash, 0, len(c.Parents))
	for _, p := range c.Parents {
		h, err := cas.ParseHex(p)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// WalkTipOrder returns commits reachable from tip in reverse-chronological
// order (BFS by timestamp desc), up to limit (0 = all).
func (s *Store) WalkTipOrder(tip cas.Hash, limit int) ([]cas.Hash, []*Commit, error) {
	var hashes []cas.Hash
	var commits []*Commit
	seen := map[cas.Hash]bool{}
	queue := []cas.Hash{tip}
	for len(queue) > 0 {
		// Pop highest-timestamp first (simple selection; fine for local scale).
		best := 0
		var bestC *Commit
		loaded := make([]*Commit, len(queue))
		for i, h := range queue {
			if seen[h] {
				continue
			}
			c, err := s.Get(h)
			if err != nil {
				return nil, nil, err
			}
			loaded[i] = c
			if bestC == nil || c.Timestamp > bestC.Timestamp {
				best, bestC = i, c
			}
		}
		h := queue[best]
		queue = append(queue[:best], queue[best+1:]...)
		if seen[h] {
			continue
		}
		seen[h] = true
		c := bestC
		if c == nil {
			continue
		}
		hashes = append(hashes, h)
		commits = append(commits, c)
		if limit > 0 && len(hashes) >= limit {
			break
		}
		for _, p := range c.Parents {
			ph, err := cas.ParseHex(p)
			if err != nil {
				return nil, nil, err
			}
			if !seen[ph] {
				queue = append(queue, ph)
			}
		}
	}
	return hashes, commits, nil
}

// Heads returns tips not reachable from any other given tip (useful for
// multi-head sync state).
func (s *Store) Heads(tips []cas.Hash) []cas.Hash {
	reachable := map[cas.Hash]bool{}
	for _, t := range tips {
		s.markReachable(t, reachable)
	}
	// A tip is a head if no other tip reaches it... approximate: tips that
	// are not reachable from the union excluding themselves.
	var heads []cas.Hash
	for i, t := range tips {
		others := map[cas.Hash]bool{}
		for j, o := range tips {
			if i != j {
				s.markReachable(o, others)
			}
		}
		if !others[t] {
			heads = append(heads, t)
		}
	}
	_ = reachable
	sort.Slice(heads, func(i, j int) bool { return cas.Hex(heads[i]) < cas.Hex(heads[j]) })
	return heads
}

func (s *Store) markReachable(tip cas.Hash, seen map[cas.Hash]bool) {
	stack := []cas.Hash{tip}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		c, err := s.Get(h)
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
