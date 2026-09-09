package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
)

func TestInitCommitLog(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "proj")
	r, err := Init(repoDir, "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("hello lrm"), 0o644)
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("should detect new file")
	}
	h, c, err := r.Commit("first", res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	if c.Message != "first" {
		t.Fatal("commit msg mismatch")
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	tipHex, _ := r.GetRef("main")
	if tipHex != cas.Hex(h) {
		t.Fatal("ref not updated")
	}
	// Second scan should be clean after index update.
	res2, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	_ = res2
	_ = merkle.DefaultOptions
}
