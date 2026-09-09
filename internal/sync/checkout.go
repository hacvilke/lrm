package sync

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// Checkout materializes a commit's tree into the working directory.
// Files stream from CAS (blobs or chunk manifests) with bounded memory.
// Files present on disk but absent from the tree are removed (except .lrm).
func Checkout(r *store.Repo, commit cas.Hash) error {
	c, err := r.DAG.Get(commit)
	if err != nil {
		return err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return err
	}
	// Write files.
	for p, hx := range flat {
		h, err := cas.ParseHex(hx)
		if err != nil {
			return err
		}
		full := filepath.Join(r.Root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := writeObjectToFile(r.CAS, h, full); err != nil {
			return fmt.Errorf("checkout %s: %w", p, err)
		}
	}
	// Remove extras.
	_ = filepath.WalkDir(r.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".lrm" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, ok := flat[rel]; !ok {
			_ = os.Remove(path)
		}
		return nil
	})
	// Refresh index to the new tree state.
	if err := r.Index.UpdateFromScan(r.Root, r.CAS, th); err != nil {
		return err
	}
	return r.Index.Save()
}

func writeObjectToFile(casStore *cas.Store, h cas.Hash, dst string) error {
	// Auto-detect chunk manifest vs raw blob by attempting manifest decode
	// on a bounded prefix (manifests are small JSON).
	if isManifest(casStore, h) {
		m, err := chunker.LoadManifest(casStore, h)
		if err != nil {
			return err
		}
		tmp := dst + ".lrmtmp"
		out, err := os.Create(tmp)
		if err != nil {
			return err
		}
		rerr := chunker.Reassemble(casStore, m, out)
		cerr := out.Close()
		if rerr != nil {
			_ = os.Remove(tmp)
			return rerr
		}
		if cerr != nil {
			_ = os.Remove(tmp)
			return cerr
		}
		return os.Rename(tmp, dst)
	}
	rc, err := casStore.Get(h)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".lrmtmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, cpErr := io.CopyBuffer(out, rc, make([]byte, 32*1024))
	cerr := out.Close()
	if cpErr != nil {
		_ = os.Remove(tmp)
		return cpErr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, dst)
}

func isManifest(s *cas.Store, h cas.Hash) bool {
	raw, err := s.GetBytes(h, 4<<20)
	if err != nil {
		return false
	}
	var m chunker.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Version == 1 && len(m.Chunks) > 0 && m.ChunkSize > 0
}
