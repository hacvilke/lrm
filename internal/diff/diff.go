// Package diff computes delta diffs between file versions.
//
// Small text files get line-oriented unified diffs (Myers-lite via LCS on
// lines with bounded memory). Large/binary files get chunk-level descriptors
// so the sync engine can stream only changed blocks.
package diff

import (
	"bytes"
	"fmt"
	"strings"
)

// Hunk is a contiguous changed region.
type Hunk struct {
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Lines    []string `json:"lines"` // prefixed with ' ', '+', '-'
}

// Patch describes how to transform OldHash → NewHash for one file.
type Patch struct {
	Path    string `json:"path"`
	OldHash string `json:"old_hash"`
	NewHash string `json:"new_hash"`
	Binary  bool   `json:"binary"`
	Hunks   []Hunk `json:"hunks"`
}

// Unified renders a git-style unified diff string.
func (p Patch) Unified() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", p.Path, p.Path)
	for _, h := range p.Hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.OldStart+1, h.OldLines, h.NewStart+1, h.NewLines)
		for _, l := range h.Lines {
			sb.WriteString(l + "\n")
		}
	}
	return sb.String()
}

// IsBinary sniffs for NUL bytes (binary heuristic), streaming-friendly for
// small prefixes.
func IsBinary(b []byte) bool {
	sniff := b
	if len(sniff) > 8000 {
		sniff = sniff[:8000]
	}
	return bytes.IndexByte(sniff, 0) >= 0
}

// DiffLines computes a line diff between oldText and newText.
// Uses a memory-bounded LCS (Hirschberg-lite via DP rows) suitable for
// files up to ~50k lines; larger inputs fall back to whole-file replace.
func DiffLines(path, oldHash, newHash, oldText, newText string) Patch {
	if IsBinary([]byte(oldText)) || IsBinary([]byte(newText)) {
		return Patch{Path: path, OldHash: oldHash, NewHash: newHash, Binary: true}
	}
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	// Bound: if product too large, emit single replace hunk.
	if int64(len(a))*int64(len(b)) > 4_000_000 {
		return Patch{Path: path, OldHash: oldHash, NewHash: newHash, Hunks: []Hunk{{
			OldStart: 0, OldLines: len(a), NewStart: 0, NewLines: len(b),
			Lines: append(prefixLines(a, "-"), prefixLines(b, "+")...),
		}}}
	}
	ops := lcsOps(a, b)
	return Patch{Path: path, OldHash: oldHash, NewHash: newHash, Hunks: opsToHunks(ops, a, b)}
}

type op struct {
	kind int // 0=equal, 1=del, 2=add
	aIdx int
	bIdx int
}

func lcsOps(a, b []string) []op {
	n, m := len(a), len(b)
	// DP with two rows for lengths, plus full backtrack via stored table
	// only when small; otherwise use simple greedy fallback.
	if n*m > 4_000_000 {
		var ops []op
		i, j := 0, 0
		for i < n && j < m {
			if a[i] == b[j] {
				ops = append(ops, op{0, i, j})
				i++
				j++
			} else if n-i > m-j {
				ops = append(ops, op{1, i, j})
				i++
			} else {
				ops = append(ops, op{2, i, j})
				j++
			}
		}
		for ; i < n; i++ {
			ops = append(ops, op{1, i, j})
		}
		for ; j < m; j++ {
			ops = append(ops, op{2, i, j})
		}
		return ops
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, op{0, i, j})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, op{1, i, j})
			i++
		} else {
			ops = append(ops, op{2, i, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{1, i, j})
	}
	for ; j < m; j++ {
		ops = append(ops, op{2, i, j})
	}
	return ops
}

func opsToHunks(ops []op, a, b []string) []Hunk {
	const context = 3
	// Mark changed regions with context.
	changed := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind != 0 {
			changed[i] = true
		}
	}
	for i := range ops {
		if changed[i] {
			for k := 1; k <= context; k++ {
				if i-k >= 0 {
					changed[i-k] = true
				}
				if i+k < len(ops) {
					changed[i+k] = true
				}
			}
		}
	}
	var hunks []Hunk
	i := 0
	for i < len(ops) {
		if !changed[i] {
			i++
			continue
		}
		start := i
		for i < len(ops) && changed[i] {
			i++
		}
		end := i
		var lines []string
		oldStart, newStart := -1, -1
		oc, nc := 0, 0
		for _, o := range ops[start:end] {
			switch o.kind {
			case 0:
				if oldStart < 0 {
					oldStart, newStart = o.aIdx, o.bIdx
				}
				lines = append(lines, " "+a[o.aIdx])
				oc++
				nc++
			case 1:
				if oldStart < 0 {
					oldStart, newStart = o.aIdx, o.bIdx
				}
				lines = append(lines, "-"+a[o.aIdx])
				oc++
			case 2:
				if oldStart < 0 {
					oldStart, newStart = o.aIdx, o.bIdx
				}
				lines = append(lines, "+"+b[o.bIdx])
				nc++
			}
		}
		if oldStart < 0 {
			oldStart, newStart = 0, 0
		}
		hunks = append(hunks, Hunk{OldStart: oldStart, OldLines: oc, NewStart: newStart, NewLines: nc, Lines: lines})
	}
	return hunks
}

func prefixLines(lines []string, p string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, p+l)
	}
	return out
}
