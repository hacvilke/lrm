package gitcompat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// CleanResult lists what would be (or was) removed. Candidates are
// workdir files absent from the tip tree — tracked files are never
// touched, and .lrm is never entered.
type CleanResult struct {
	Candidates []string // repo-relative, dirs end with "/" when collapsed
	Removed    bool     // force was set: candidates were deleted
}

// Clean finds untracked files. With dirs=true, fully-untracked
// directories collapse to a single "dir/" entry. Nothing is deleted
// unless force=true (safe default: dry run). Ignored paths (see
// .lrmignore) are skipped unless all=true (git clean -x parity).
func Clean(r *store.Repo, force, dirs, all bool) (*CleanResult, error) {
	tip, _, err := r.HeadCommit()
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	if tip != cas.Nil {
		flat, err := TreeAtCommit(r, tip)
		if err != nil {
			return nil, err
		}
		for p := range flat {
			tracked[p] = true
		}
	}
	ign := merkle.LoadIgnore(r.Root)
	var files []string
	seenDirs := map[string]bool{}
	trackedDirs := map[string]bool{} // dirs containing ≥1 tracked file
	for p := range tracked {
		for d := pathDir(p); d != ""; d = pathDir(d) {
			trackedDirs[d] = true
		}
	}
	err = filepath.WalkDir(r.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".lrm" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			if !all && path != r.Root {
				if rel, rerr := filepath.Rel(r.Root, path); rerr == nil {
					if ign.Ignored(filepath.ToSlash(rel)) {
						return filepath.SkipDir
					}
				}
			}
			if path != r.Root {
				rel, _ := filepath.Rel(r.Root, path)
				seenDirs[filepath.ToSlash(rel)] = true
			}
			return nil
		}
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !all && ign.Ignored(rel) {
			return nil
		}
		if !tracked[rel] {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	cands := files
	if dirs {
		cands = collapseDirs(files, seenDirs, trackedDirs)
	}
	res := &CleanResult{Candidates: cands}
	if !force {
		return res, nil
	}
	for _, c := range cands {
		full := filepath.Join(r.Root, filepath.FromSlash(strings.TrimSuffix(c, "/")))
		if strings.HasSuffix(c, "/") {
			if err := os.RemoveAll(full); err != nil {
				return nil, fmt.Errorf("remove %s: %w", c, err)
			}
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove %s: %w", c, err)
		}
	}
	res.Removed = true
	return res, nil
}

// collapseDirs replaces file lists under fully-untracked dirs with "dir/".
func collapseDirs(files []string, seenDirs, trackedDirs map[string]bool) []string {
	collapsed := map[string]bool{}
	for d := range seenDirs {
		if trackedDirs[d] || d == "" {
			continue
		}
		// Fully untracked only if it holds ≥1 candidate (skip empties' parents... include empties too).
		collapsed[d+"/"] = true
	}
	// Drop nested entries shadowed by a collapsed parent.
	var dirs []string
	for d := range collapsed {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	keepDirs := []string{}
	for _, d := range dirs {
		shadowed := false
		for _, k := range keepDirs {
			if strings.HasPrefix(d, k) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			keepDirs = append(keepDirs, d)
		}
	}
	var out []string
	for _, f := range files {
		shadowed := false
		for _, k := range keepDirs {
			if strings.HasPrefix(f, k) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, f)
		}
	}
	out = append(out, keepDirs...)
	sort.Strings(out)
	return out
}

func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}
