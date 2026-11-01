package sync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// RestorePaths writes the given paths from commit into the working tree,
// overwriting whatever is there. Files absent from that commit are deleted
// locally.
//
// This is the "take theirs" half of a conflicted merge: the merge keeps your
// version and leaves the peer's on the branch, so the way back is to copy the
// path out of that commit. Paths are validated against the repo root exactly
// like Checkout — a hostile tree must not write outside the workspace.
func RestorePaths(r *store.Repo, commit cas.Hash, paths []string) (restored int, err error) {
	c, err := r.DAG.Get(commit)
	if err != nil {
		return 0, err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return 0, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return 0, err
	}
	// Validate every requested path before touching any file.
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		rel := path.Clean(filepath.ToSlash(p))
		if rel == "." || rel == ".." || path.IsAbs(rel) || len(rel) > 4 && rel[:3] == ".." {
			return 0, fmt.Errorf("refusing suspicious path %q", p)
		}
		if _, ok := flat[rel]; !ok && !repoHasPath(r, rel) {
			return 0, fmt.Errorf("%q is not in %s", p, cas.Short(commit))
		}
		clean = append(clean, rel)
	}
	for _, rel := range clean {
		dest := filepath.Join(r.Root, filepath.FromSlash(rel))
		hHex, ok := flat[rel]
		if !ok {
			// Not in that commit: it was deleted there. Mirror that.
			if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
				return restored, err
			}
			restored++
			continue
		}
		h, err := cas.ParseHex(hHex)
		if err != nil {
			return restored, err
		}
		raw, err := r.CAS.GetBytes(h, 64<<20)
		if err != nil {
			return restored, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return restored, err
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}

// repoHasPath reports whether the path exists in the current working tree.
func repoHasPath(r *store.Repo, rel string) bool {
	_, err := os.Stat(filepath.Join(r.Root, filepath.FromSlash(rel)))
	return err == nil
}
