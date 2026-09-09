package sync

import (
	stdsync "sync"
	"testing"

	"net"
	"os"
	"path/filepath"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/store"
)

func mkRepo(t *testing.T, user string) *store.Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	r, err := store.Init(dir, user, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func commitFile(t *testing.T, r *store.Repo, name, content, msg string) cas.Hash {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := r.Commit(msg, res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	return h
}

// TestSyncFetchEmptyPeer verifies a full fetch from a populated repo to an
// empty one over an in-memory mux session pair.
func TestSyncFetchEmptyPeer(t *testing.T) {
	a := mkRepo(t, "alice")
	commitFile(t, a, "hello.txt", "hello from alice\n", "alice first")
	commitFile(t, a, "notes.md", "# notes\nsecond file\n", "alice second")

	b := mkRepo(t, "bob")

	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false) // alice = responder
	sessB := mux.NewSession(pb, true)  // bob = dialer/initiator
	defer sessA.Close()
	defer sessB.Close()

	var wg stdsync.WaitGroup
	wg.Add(2)
	var resA, resB *SyncResult
	var errA, errB error
	go func() {
		defer wg.Done()
		resA, errA = New(a).SyncWithSession(sessA, false, "")
	}()
	go func() {
		defer wg.Done()
		resB, errB = New(b).SyncWithSession(sessB, true, "")
	}()
	wg.Wait()
	if errA != nil {
		t.Fatalf("responder: %v", errA)
	}
	if errB != nil {
		t.Fatalf("initiator: %v", errB)
	}
	_ = resA
	if resB.Fetched == 0 {
		t.Fatal("expected bob to fetch commits")
	}
	// Bob's tip should equal Alice's tip; files materialized.
	tipA, _, _ := a.HeadCommit()
	tipB, _, _ := b.HeadCommit()
	if tipA != tipB {
		t.Fatalf("tips differ: a=%s b=%s", cas.Short(tipA), cas.Short(tipB))
	}
	raw, err := os.ReadFile(filepath.Join(b.Root, "hello.txt"))
	if err != nil {
		t.Fatalf("bob missing hello.txt: %v", err)
	}
	if string(raw) != "hello from alice\n" {
		t.Fatalf("bad content: %q", raw)
	}
	raw, err = os.ReadFile(filepath.Join(b.Root, "notes.md"))
	if err != nil || string(raw) != "# notes\nsecond file\n" {
		t.Fatalf("bad notes.md: %q %v", raw, err)
	}
}

// TestSyncConflictBranch verifies concurrent edits to the same file produce
// a HEAD-peer-* branch instead of overwriting.
func TestSyncConflictBranch(t *testing.T) {
	a := mkRepo(t, "alice")
	h0 := commitFile(t, a, "shared.txt", "base\n", "base")
	_ = h0

	// Clone alice → carol via sync, then diverge both on the same file.
	c := mkRepo(t, "carol")
	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false)
	sessC := mux.NewSession(pb, true)
	var wg stdsync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = New(a).SyncWithSession(sessA, false, "") }()
	go func() { defer wg.Done(); _, _ = New(c).SyncWithSession(sessC, true, "") }()
	wg.Wait()
	sessA.Close()
	sessC.Close()

	commitFile(t, a, "shared.txt", "alice edit\n", "alice concurrent")
	commitFile(t, c, "shared.txt", "carol edit\n", "carol concurrent")

	// Sync carol → alice (alice initiates against carol).
	pa2, pb2 := net.Pipe()
	sessA2 := mux.NewSession(pa2, true)  // alice dials
	sessC2 := mux.NewSession(pb2, false) // carol serves
	defer sessA2.Close()
	defer sessC2.Close()
	wg.Add(2)
	var resA *SyncResult
	var errA, errC error
	go func() { defer wg.Done(); _, errC = New(c).SyncWithSession(sessC2, false, "") }()
	go func() { defer wg.Done(); resA, errA = New(a).SyncWithSession(sessA2, true, "") }()
	wg.Wait()
	if errA != nil {
		t.Fatalf("alice sync: %v", errA)
	}
	if errC != nil {
		t.Fatalf("carol sync: %v", errC)
	}
	if resA.ConflictBranch == "" {
		t.Fatalf("expected conflict branch, got result: %+v", resA)
	}
	// Alice's own tip must be untouched (still her edit).
	raw, _ := os.ReadFile(filepath.Join(a.Root, "shared.txt"))
	if string(raw) != "alice edit\n" {
		t.Fatalf("alice workdir overwritten: %q", raw)
	}
	// Conflict branch exists and points at carol's tip.
	tipC, _, _ := c.HeadCommit()
	cbHex, _ := a.GetRef(resA.ConflictBranch)
	if cbHex != cas.Hex(tipC) {
		t.Fatalf("conflict branch %s=%s, want carol tip %s", resA.ConflictBranch, cbHex, cas.Hex(tipC))
	}
}

// TestSyncCleanMerge verifies non-overlapping concurrent edits auto-merge.
func TestSyncCleanMerge(t *testing.T) {
	a := mkRepo(t, "alice")
	commitFile(t, a, "base.txt", "base\n", "base")
	b := mkRepo(t, "bob")
	pa, pb := net.Pipe()
	sA := mux.NewSession(pa, false)
	sB := mux.NewSession(pb, true)
	var wg stdsync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = New(a).SyncWithSession(sA, false, "") }()
	go func() { defer wg.Done(); _, _ = New(b).SyncWithSession(sB, true, "") }()
	wg.Wait()
	sA.Close()
	sB.Close()

	commitFile(t, a, "alice-only.txt", "A\n", "alice adds file")
	commitFile(t, b, "bob-only.txt", "B\n", "bob adds file")

	pa2, pb2 := net.Pipe()
	sA2 := mux.NewSession(pa2, true)
	sB2 := mux.NewSession(pb2, false)
	defer sA2.Close()
	defer sB2.Close()
	wg.Add(2)
	var resA *SyncResult
	var errA error
	go func() { defer wg.Done(); _, _ = New(b).SyncWithSession(sB2, false, "") }()
	go func() { defer wg.Done(); resA, errA = New(a).SyncWithSession(sA2, true, "") }()
	wg.Wait()
	if errA != nil {
		t.Fatalf("sync: %v", errA)
	}
	if resA.ConflictBranch != "" {
		t.Fatalf("unexpected conflict branch: %s", resA.ConflictBranch)
	}
	if resA.MergedCommit == "" {
		t.Fatalf("expected auto-merge commit, got: %+v", resA)
	}
	// Both files present in alice's workdir.
	if _, err := os.Stat(filepath.Join(a.Root, "alice-only.txt")); err != nil {
		t.Fatal("alice-only.txt missing after merge")
	}
	if _, err := os.Stat(filepath.Join(a.Root, "bob-only.txt")); err != nil {
		t.Fatal("bob-only.txt missing after merge")
	}
}
