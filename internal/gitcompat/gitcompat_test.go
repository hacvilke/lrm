package gitcompat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
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

func commitAll(t *testing.T, r *store.Repo, msg string) cas.Hash {
	t.Helper()
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := r.Commit(msg, res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	_ = r.Index.Save()
	return h
}

func TestStashRoundTrip(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "f.txt"), []byte("v1\n"), 0o644)
	commitAll(t, r, "first")
	_ = os.WriteFile(filepath.Join(r.Root, "f.txt"), []byte("v2-dirty\n"), 0o644)

	e, err := StashPush(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "stash@{0}" {
		t.Fatalf("bad stash name %s", e.Name)
	}
	// Workdir restored to HEAD.
	raw, _ := os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "v1\n" {
		t.Fatalf("workdir not restored: %q", raw)
	}
	res, _ := r.Index.Scan(r.Root, r.CAS)
	if !res.Clean {
		t.Fatal("workdir should be clean after stash")
	}
	// Pop restores delta (dirty).
	got, err := StashPop(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != e.Commit {
		t.Fatal("pop returned different entry")
	}
	raw, _ = os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "v2-dirty\n" {
		t.Fatalf("delta not restored: %q", raw)
	}
	// Shelf dropped.
	entries, _ := StashList(r)
	if len(entries) != 0 {
		t.Fatal("shelf should be dropped after pop")
	}
}

func TestResetSoftMixedHard(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "f.txt"), []byte("v1\n"), 0o644)
	h1 := commitAll(t, r, "one")
	_ = os.WriteFile(filepath.Join(r.Root, "f.txt"), []byte("v2\n"), 0o644)
	_ = commitAll(t, r, "two")

	if err := ResetSoft(r, h1); err != nil {
		t.Fatal(err)
	}
	tipHex, _ := r.GetRef("main")
	if tipHex != cas.Hex(h1) {
		t.Fatal("soft reset did not move ref")
	}
	// Workdir untouched (still v2).
	raw, _ := os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "v2\n" {
		t.Fatal("soft reset must not touch workdir")
	}
	if err := ResetHard(r, h1); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "v1\n" {
		t.Fatalf("hard reset workdir: %q", raw)
	}
}

func TestTagFsckGC(t *testing.T) {
	r := mkRepo(t)
	_ = os.WriteFile(filepath.Join(r.Root, "f.txt"), []byte("data\n"), 0o644)
	h1 := commitAll(t, r, "one")
	if err := TagCreate(r, "v0.1", h1); err != nil {
		t.Fatal(err)
	}
	if err := TagCreate(r, "v0.1", h1); err == nil {
		t.Fatal("duplicate tag should fail")
	}
	tags, err := TagList(r)
	if err != nil || tags["v0.1"] != cas.Hex(h1) {
		t.Fatalf("tag list: %v %v", tags, err)
	}
	f, err := Fsck(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Missing) != 0 {
		t.Fatalf("fsck missing: %v", f.Missing)
	}
	if f.Commits != 1 || f.Blobs != 1 {
		t.Fatalf("fsck counts: %+v", f)
	}
	// Plant an unreachable object, then gc it.
	orphan, err := r.CAS.PutBytes([]byte("orphan-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := GC(r, true)
	if err != nil {
		t.Fatal(err)
	}
	if g.Removed == 0 {
		t.Fatal("dry-run should report the orphan")
	}
	if !r.CAS.Exists(orphan) {
		t.Fatal("dry-run must not delete")
	}
	g, err = GC(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.CAS.Exists(orphan) {
		t.Fatal("gc should delete orphan")
	}
	// Reachable objects survive.
	f2, _ := Fsck(r)
	if len(f2.Missing) != 0 {
		t.Fatalf("gc deleted reachable objects: %v", f2.Missing)
	}
}
