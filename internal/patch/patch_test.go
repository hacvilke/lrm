package patch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

func mkRepo(t *testing.T) *store.Repo {
	t.Helper()
	r, err := store.Init(filepath.Join(t.TempDir(), "proj"), "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func snap(t *testing.T, r *store.Repo) cas.Hash {
	t.Helper()
	h, err := merkle.BuildTree(r.CAS, r.Root, merkle.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBuildTextPatches(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	_ = os.WriteFile(filepath.Join(r.Root, "gone.txt"), []byte("bye\n"), 0o644)
	h1 := snap(t, r)

	_ = os.WriteFile(filepath.Join(r.Root, "a.txt"), []byte("one\nTWO\nthree\nfour\n"), 0o644)
	_ = os.Remove(filepath.Join(r.Root, "gone.txt"))
	_ = os.WriteFile(filepath.Join(r.Root, "new.txt"), []byte("hello\n"), 0o644)
	h2 := snap(t, r)

	fps, err := Build(r, h1, h2, 0)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FilePatch{}
	for _, fp := range fps {
		byPath[fp.Path] = fp
	}
	if byPath["a.txt"].Kind != "modified" {
		t.Fatalf("a.txt kind=%s", byPath["a.txt"].Kind)
	}
	u := byPath["a.txt"].Patch.Unified()
	if !strings.Contains(u, "-two") || !strings.Contains(u, "+TWO") || !strings.Contains(u, "+four") {
		t.Fatalf("bad patch:\n%s", u)
	}
	if byPath["gone.txt"].Kind != "deleted" {
		t.Fatalf("gone.txt kind=%s", byPath["gone.txt"].Kind)
	}
	if !strings.Contains(byPath["gone.txt"].Patch.Unified(), "-bye") {
		t.Fatal("deleted patch missing -bye")
	}
	if byPath["new.txt"].Kind != "added" {
		t.Fatalf("new.txt kind=%s", byPath["new.txt"].Kind)
	}
}

func TestBuildBinaryAndLarge(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "b.bin"), []byte{0, 1, 2, 3}, 0o644)
	h1 := snap(t, r)
	_ = os.WriteFile(filepath.Join(r.Root, "b.bin"), []byte{0, 1, 2, 4}, 0o644)
	// >1 chunk file (chunked manifest entry).
	big := make([]byte, 200*1024)
	for i := range big {
		big[i] = byte(i)
	}
	_ = os.WriteFile(filepath.Join(r.Root, "big.bin"), big, 0o644)
	h2 := snap(t, r)

	fps, err := Build(r, h1, h2, 0)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FilePatch{}
	for _, fp := range fps {
		byPath[fp.Path] = fp
	}
	if byPath["b.bin"].Kind != "binary" {
		t.Fatalf("b.bin kind=%s want binary", byPath["b.bin"].Kind)
	}
	if byPath["big.bin"].Kind != "large" {
		t.Fatalf("big.bin kind=%s want large", byPath["big.bin"].Kind)
	}
}

func TestBuildEmptySide(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "a.txt"), []byte("hi\n"), 0o644)
	h := snap(t, r)
	fps, err := Build(r, cas.Nil, h, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 || fps[0].Kind != "added" {
		t.Fatalf("fps=%+v", fps)
	}
}
