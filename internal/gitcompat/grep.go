package gitcompat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/diff"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// MaxGrepMatches caps reported matches (flooding helps nobody).
const MaxGrepMatches = 200

// GrepMatch is one literal-substring hit.
type GrepMatch struct {
	Path   string
	Lineno int // 1-based
	Text   string
}

// GrepResult carries matches plus truncation/skip accounting.
type GrepResult struct {
	Matches   []GrepMatch
	Truncated bool
	Skipped   int // files skipped (too large or binary)
}

// GrepWorkdir searches the working tree (skips .lrm). Matching is a
// literal substring (case-insensitive when ci is set) — predictable,
// no regex dialect to misremember.
func GrepWorkdir(r *store.Repo, pattern string, ci bool) (*GrepResult, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	res := &GrepResult{}
	needle := pattern
	if ci {
		needle = strings.ToLower(pattern)
	}
	ign := merkle.LoadIgnore(r.Root)

	err := filepath.WalkDir(r.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || res.Truncated {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".lrm" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			if rel, rerr := filepath.Rel(r.Root, path); rerr == nil && rel != "." {
				if ign.Ignored(filepath.ToSlash(rel)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return nil
		}
		if ign.Ignored(filepath.ToSlash(rel)) {
			return nil
		}
		raw, err := readWorkFile(path)
		if err != nil {
			res.Skipped++
			return nil
		}
		scanLines(filepath.ToSlash(rel), raw, needle, ci, res)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// GrepRef searches a historical tree at commit.
func GrepRef(r *store.Repo, commit cas.Hash, pattern string, ci bool) (*GrepResult, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	flat, err := TreeAtCommit(r, commit)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	res := &GrepResult{}
	needle := pattern
	if ci {
		needle = strings.ToLower(pattern)
	}
	for _, p := range paths {
		if res.Truncated {
			break
		}
		h, err := cas.ParseHex(flat[p])
		if err != nil {
			res.Skipped++
			continue
		}
		raw, err := ReadObjectBytes(r.CAS, h, MaxReadBytes)
		if err != nil || diff.IsBinary(raw) {
			res.Skipped++
			continue
		}
		scanLines(p, raw, needle, ci, res)
	}
	return res, nil
}

func readWorkFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxReadBytes {
		return nil, fmt.Errorf("too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if diff.IsBinary(raw) {
		return nil, fmt.Errorf("binary")
	}
	return raw, nil
}

func scanLines(path string, raw []byte, needle string, ci bool, res *GrepResult) {
	for i, line := range splitLines(string(raw)) {
		hay := line
		if ci {
			hay = strings.ToLower(line)
		}
		if strings.Contains(hay, needle) {
			if len(res.Matches) >= MaxGrepMatches {
				res.Truncated = true
				return
			}
			res.Matches = append(res.Matches, GrepMatch{Path: path, Lineno: i + 1, Text: line})
		}
	}
}
