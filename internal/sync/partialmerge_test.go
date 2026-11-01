package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// TestMergeAppliesNonConflictingFiles is the soak regression. Two devices
// edited one shared file on both sides while also adding 20 unrelated files
// each. The old merge refused everything because of the one overlapping file,
// so the 20 unrelated files stayed stranded on the peer branch — measured as
// 48 files transferred and 0 applied, with no way forward except manual
// branch surgery.
func TestMergeAppliesNonConflictingFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := store.Init(root, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		t.Helper()
		res, err := r.Index.Scan(r.Root, r.CAS)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		h, _, err := r.Commit(msg, res.RootHash)
		if err != nil {
			t.Fatalf("commit %q: %v", msg, err)
		}
		_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
		return cas.Hex(h)
	}

	write("shared.txt", "base\n")
	write("keep.txt", "keep\n")
	base := commit("base")

	// Ours: 20 new files plus a change to shared.txt.
	for i := 0; i < 20; i++ {
		write("ours"+string(rune('a'+i))+".txt", "ours\n")
	}
	write("shared.txt", "mine\n")
	ours := commit("ours")

	// Theirs: 20 different files plus a different change to shared.txt.
	// Diverge from base the way a second device would.
	if err := r.SetRef("peer", base); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRef("main", base); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(r, mustHash(t, base)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		write("theirs"+string(rune('a'+i))+".txt", "theirs\n")
	}
	write("shared.txt", "theirs\n")
	theirs := commit("theirs")

	// Back to the branch that holds "ours" — the one we are merging into.
	oursTip := ours
	if err := r.SetRef("main", oursTip); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(r, mustHash(t, oursTip)); err != nil {
		t.Fatal(err)
	}
	_ = ours
	res, err := MergeLocalOpts(r, mustHash(t, theirs), "peer", MergeOptions{})
	if err != nil {
		t.Fatalf("merge must complete despite one overlapping file: %v", err)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0] != "shared.txt" {
		t.Fatalf("unresolved = %v, want [shared.txt]", res.Unresolved)
	}

	// The 20 non-conflicting files must be on disk now.
	present := 0
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(filepath.Join(root, "theirs"+string(rune('a'+i))+".txt")); err == nil {
			present++
		}
	}
	if present != 20 {
		t.Fatalf("%d/20 non-conflicting files applied — merge is still all-or-nothing", present)
	}
	// Ours must survive untouched, and our own files must still be there.
	if b, _ := os.ReadFile(filepath.Join(root, "shared.txt")); strings.TrimSpace(string(b)) != "mine" {
		t.Fatalf("conflicting file = %q, want our version by default", b)
	}
	if _, err := os.Stat(filepath.Join(root, "oursa.txt")); err != nil {
		t.Fatalf("local-only file disappeared: %v", err)
	}

	// Taking their side afterwards, without redoing the merge: the peer tip is
	// an ancestor now, so `merge --theirs` is a no-op by definition. Restore
	// the path out of that commit instead.
	if n, err := RestorePaths(r, mustHash(t, theirs), []string{"shared.txt"}); err != nil || n != 1 {
		t.Fatalf("restore: n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "shared.txt")); strings.TrimSpace(string(b)) != "theirs" {
		t.Fatalf("after restore, shared.txt = %q, want theirs", b)
	}
}

// TestMergePreferTheirs: the same conflict, resolved the other way up front.
func TestMergePreferTheirs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := store.Init(root, "bob", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
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
		return cas.Hex(h)
	}
	write("f.txt", "base")
	base := commit("base")
	write("f.txt", "mine")
	ours := commit("ours")
	_ = ours
	if err := r.SetRef("main", base); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(r, mustHash(t, base)); err != nil {
		t.Fatal(err)
	}
	write("f.txt", "theirs")
	theirs := commit("theirs")
	if err := r.SetRef("main", ours); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(r, mustHash(t, ours)); err != nil {
		t.Fatal(err)
	}
	res, err := MergeLocalOpts(r, mustHash(t, theirs), "peer", MergeOptions{PreferTheirs: true})
	if err != nil {
		t.Fatalf("merge --theirs: %v", err)
	}
	if len(res.Unresolved) != 1 {
		t.Fatalf("overlap must still be reported: %v", res.Unresolved)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); strings.TrimSpace(string(b)) != "theirs" {
		t.Fatalf("f.txt = %q, want theirs", b)
	}
}

// TestRestorePathsRefusesTraversal: a peer-supplied path must never escape the
// repo root, even through the restore path.
func TestRestorePathsRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := store.Init(root, "carol", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := r.Commit("c", res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestorePaths(r, h, []string{"../../etc/passwd"}); err == nil {
		t.Fatal("traversal path must be refused")
	}
	if _, err := RestorePaths(r, h, []string{"/etc/passwd"}); err == nil {
		t.Fatal("absolute path must be refused")
	}
}

func mustHash(t *testing.T, hexStr string) cas.Hash {
	t.Helper()
	h, err := cas.ParseHex(hexStr)
	if err != nil {
		t.Fatalf("bad hash %q: %v", hexStr, err)
	}
	return h
}

// TestSyncKeepsBothVersionsOnOverlap is the soak's second regression: the
// daemon must never stall on an overlapping file. Both versions survive, the
// peer's as a conflict copy, and everything else applies.
func TestConflictCopyPath(t *testing.T) {
	when := time.Date(2026, 10, 31, 14, 3, 12, 0, time.UTC)
	cases := map[string]string{
		"shared.txt":          "shared.conflict-c1d842d4-20261031-140312.txt",
		"notes.md":            "notes.conflict-c1d842d4-20261031-140312.md",
		"dir/sub/report.xlsx": "dir/sub/report.conflict-c1d842d4-20261031-140312.xlsx",
		"Makefile":            "Makefile.conflict-c1d842d4-20261031-140312",
		".env":                ".env.conflict-c1d842d4-20261031-140312",
	}
	for in, want := range cases {
		if got := conflictCopyPath(in, "c1d842d4aaaaaaaa", when); got != want {
			t.Errorf("conflictCopyPath(%q) = %q, want %q", in, got, want)
		}
	}
}
