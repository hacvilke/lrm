package merkle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRootChangesOnEdit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "world")
	store, _ := cas.New(t.TempDir()) // separate from root (real layout: root/.lrm/objects, ignored)
	h1, err := BuildTree(store, root, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "a.txt"), "hello!")
	h2, err := BuildTree(store, root, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("root hash must mutate when a file changes")
	}
	changes, err := Diff(store, h1, h2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" || changes[0].Kind != "modified" {
		t.Fatalf("unexpected diff: %+v", changes)
	}
}

func TestIgnoresLrmDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "x")
	_ = os.MkdirAll(filepath.Join(root, ".lrm"), 0o755)
	writeFile(t, filepath.Join(root, ".lrm", "junk"), "junk")
	lrmObjects := filepath.Join(root, ".lrm", "objects")
	store, _ := cas.New(lrmObjects) // real layout: objects live under ignored .lrm
	h1, err := BuildTree(store, root, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".lrm", "junk2"), "more")
	h2, err := BuildTree(store, root, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal(".lrm changes must not affect root hash")
	}
}
