package dag

import (
	"io"
	"os"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
)

// copyObjectTo streams r into the CAS at an explicit domain-separated address.
func copyObjectTo(store *cas.Store, h cas.Hash, r io.Reader) error {
	dst := store.Path(h)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmpDir := filepath.Join(store.Dir(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "commit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.CopyBuffer(tmp, r, make([]byte, 32*1024)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
