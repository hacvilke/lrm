package sync

import (
	stdsync "sync"
	"testing"

	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/store"
)

// testWS is the shared workspace ID: repos created by mkRepo are peers of
// one workspace. Stranger-repo scenarios pass their own workspace ID.
const testWS = "11111111111111111111111111111111"

func mkRepo(t *testing.T, user string) *store.Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	r, err := store.InitWithWorkspace(dir, user, 0, testWS)
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

// TestSyncConflictBranch verifies concurrent edits to the same file keep both
// versions instead of overwriting one of them.
//
// This used to assert that the peer's tip was parked on a HEAD-peer-* branch.
// A 90-second soak showed why that is not good enough: the overlap is baked
// into the two commits, so every retry hits the same conflict branch, and the
// peer's unrelated files never arrive — 48 files transferred, 0 applied,
// indefinitely, with nobody at the keyboard to run `lrm merge`.
//
// New contract: the local file is untouched, the peer's version lands beside
// it as a conflict copy, and the sync reports the overlap in Conflicts.
func TestSyncConflictBranch(t *testing.T) {
	a := mkRepo(t, "alice")
	commitFile(t, a, "shared.txt", "base\n", "base")

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
	commitFile(t, a, "alice-only.txt", "A\n", "alice adds a file too")
	commitFile(t, c, "shared.txt", "carol edit\n", "carol concurrent")
	commitFile(t, c, "carol-only.txt", "C\n", "carol adds a file too")

	// Sync carol → alice (alice initiates against carol).
	pa2, pb2 := net.Pipe()
	sessA2 := mux.NewSession(pa2, true)
	sessC2 := mux.NewSession(pb2, false)
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

	// The overlap is reported, not fatal.
	if len(resA.Conflicts) != 1 || resA.Conflicts[0] != "shared.txt" {
		t.Fatalf("Conflicts = %v, want [shared.txt] (result: %+v)", resA.Conflicts, resA)
	}
	if resA.MergedCommit == "" {
		t.Fatalf("expected an auto-merge commit, got %+v", resA)
	}

	// Alice's own text is intact — a merge must never silently pick a winner.
	raw, _ := os.ReadFile(filepath.Join(a.Root, "shared.txt"))
	if string(raw) != "alice edit\n" {
		t.Fatalf("alice workdir overwritten: %q", raw)
	}

	// Carol's text is recoverable on disk, under a name that says where it
	// came from.
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".conflict-") {
			copies = append(copies, e.Name())
		}
	}
	if len(copies) != 1 {
		t.Fatalf("expected exactly one conflict copy, got %v (%v)", copies, entries)
	}
	if !strings.HasPrefix(copies[0], "shared.conflict-") || !strings.HasSuffix(copies[0], ".txt") {
		t.Fatalf("conflict copy has an unhelpful name: %q", copies[0])
	}
	body, _ := os.ReadFile(filepath.Join(a.Root, copies[0]))
	if string(body) != "carol edit\n" {
		t.Fatalf("conflict copy = %q, want carol's text", body)
	}

	// And the files that did NOT conflict must have arrived — that is the
	// whole point of the fix.
	if b, err := os.ReadFile(filepath.Join(a.Root, "carol-only.txt")); err != nil || string(b) != "C\n" {
		t.Fatalf("carol-only.txt did not sync: %q %v", b, err)
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

// TestPushLegCompleteness is a regression test: when the DIALER holds commits
// the responder lacks, the push leg must transfer commits AND all objects, so
// the responder ends fast-forwarded (or merged) with zero missing objects.
func TestPushLegCompleteness(t *testing.T) {
	r := mkRepo(t, "responder")
	commitFile(t, r, "base.txt", "base\n", "base")

	d := mkRepo(t, "dialer")
	// Seed dialer with responder's history first.
	pa, pb := net.Pipe()
	sR := mux.NewSession(pa, false)
	sD := mux.NewSession(pb, true)
	var wg stdsync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = New(r).SyncWithSession(sR, false, "") }()
	go func() { defer wg.Done(); _, _ = New(d).SyncWithSession(sD, true, "") }()
	wg.Wait()
	sR.Close()
	sD.Close()

	// Dialer adds UNIQUE content the responder has never seen.
	commitFile(t, d, "dialer-only.txt", "secret sauce\n", "dialer work")

	// Dialer dials responder: fetch leg finds nothing new; push leg must
	// deliver the commit + tree + blob; responder fast-forwards.
	pa2, pb2 := net.Pipe()
	sR2 := mux.NewSession(pa2, false)
	sD2 := mux.NewSession(pb2, true)
	defer sR2.Close()
	defer sD2.Close()
	wg.Add(2)
	var resR, resD *SyncResult
	var errR, errD error
	go func() { defer wg.Done(); resR, errR = New(r).SyncWithSession(sR2, false, "") }()
	go func() { defer wg.Done(); resD, errD = New(d).SyncWithSession(sD2, true, "") }()
	wg.Wait()
	if errD != nil {
		t.Fatalf("dialer: %v", errD)
	}
	if errR != nil {
		t.Fatalf("responder: %v", errR)
	}
	_ = resR
	_ = resD
	tipD, _, _ := d.HeadCommit()
	tipR, _, _ := r.HeadCommit()
	if tipD != tipR {
		t.Fatalf("responder tip %s != dialer tip %s", cas.Short(tipR), cas.Short(tipD))
	}
	raw, err := os.ReadFile(filepath.Join(r.Root, "dialer-only.txt"))
	if err != nil || string(raw) != "secret sauce\n" {
		t.Fatalf("pushed file missing/corrupt: %q %v", raw, err)
	}
	// No missing objects: every reachable commit's tree must exist.
	hashes, commits, err := r.DAG.WalkTipOrder(tipR, 100)
	if err != nil {
		t.Fatal(err)
	}
	_ = hashes
	for _, c := range commits {
		th, _ := cas.ParseHex(c.Tree)
		if !r.CAS.Exists(th) {
			t.Fatalf("missing tree %s for pushed history", c.Tree[:12])
		}
	}
}

// TestSyncWorkspaceMismatchRefused is the stranger-on-the-LAN scenario: two
// repos in different workspaces must NOT sync. Before workspace gating, the
// stranger's tip was fetched and parked as a HEAD-peer-* conflict ref.
func TestSyncWorkspaceMismatchRefused(t *testing.T) {
	a := mkRepo(t, "alice") // workspace testWS
	commitFile(t, a, "secret.txt", "alice's project\n", "alice first")

	// bob lives in a different workspace entirely.
	dir := filepath.Join(t.TempDir(), "bob")
	b, err := store.InitWithWorkspace(dir, "bob", 0, "22222222222222222222222222222222")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	commitFile(t, b, "other.txt", "bob's unrelated project\n", "bob first")

	tipA, _, _ := a.HeadCommit()
	brsBeforeB, _ := b.ListBranches()

	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false)
	sessB := mux.NewSession(pb, true)
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

	// The dialer is told the sync was refused.
	if errB == nil || !strings.Contains(errB.Error(), "workspace mismatch") {
		t.Fatalf("dialer should be refused with workspace mismatch, got err=%v", errB)
	}
	_ = resB
	// The responder reports it without failing the session.
	if errA != nil {
		t.Fatalf("responder: %v", errA)
	}
	if resA == nil || !strings.Contains(resA.Message, "workspace mismatch") {
		t.Fatalf("responder should report mismatch, got %+v", resA)
	}
	// No objects crossed: bob must not know alice's tip…
	if b.DAG.Has(tipA) {
		t.Fatal("foreign workspace commit leaked into bob's DAG")
	}
	// …and no HEAD-peer-* conflict branch was created.
	brsAfterB, _ := b.ListBranches()
	if len(brsAfterB) != len(brsBeforeB) {
		t.Fatalf("branch set changed on refusal: before=%v after=%v", brsBeforeB, brsAfterB)
	}
	for _, br := range brsAfterB {
		if strings.HasPrefix(br, "HEAD-peer-") {
			t.Fatalf("conflict branch created on refusal: %s", br)
		}
	}
}

// TestSyncWorkspaceAdoption is the v1-Port-Key join flow: an untethered
// repo (no workspace yet) takes the sharer's workspace on first sync.
func TestSyncWorkspaceAdoption(t *testing.T) {
	a := mkRepo(t, "alice") // workspace testWS
	commitFile(t, a, "hello.txt", "hello from alice\n", "alice first")

	// bob: fresh repo created by a v1 join (no workspace carried in the key).
	dir := filepath.Join(t.TempDir(), "bob")
	b, err := store.InitWithWorkspace(dir, "bob", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false)
	sessB := mux.NewSession(pb, true)

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
	sessA.Close()
	sessB.Close()
	_ = resA
	if errA != nil {
		t.Fatalf("responder: %v", errA)
	}
	if errB != nil {
		t.Fatalf("dialer: %v", errB)
	}
	if resB == nil || resB.Fetched == 0 {
		t.Fatal("bob should have fetched alice's history")
	}
	if b.Config.Workspace != testWS {
		t.Fatalf("bob should have adopted workspace %s, has %q", testWS, b.Config.Workspace)
	}
	// The adoption must be persisted, not just in-memory.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Config.Workspace != testWS {
		t.Fatalf("adopted workspace not persisted: %q", reopened.Config.Workspace)
	}
}
