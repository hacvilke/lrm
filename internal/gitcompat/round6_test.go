package gitcompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/sync"
)

// --- branch -d/-D ---

func TestBranchDelete(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "base.txt", "base\n")
	c1 := commitAll(t, r, "c1 base")
	if err := r.SetRef("feat", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "feat.txt", "f\n")
	commitAll(t, r, "f1")
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "main.txt", "m\n")
	commitAll(t, r, "c2")
	// Current branch refuses (even forced).
	if _, err := DeleteBranch(r, "main", true); err == nil {
		t.Fatal("deleting current branch should refuse")
	}
	// Missing branch.
	if _, err := DeleteBranch(r, "ghost", true); err == nil {
		t.Fatal("deleting missing branch should fail")
	}
	// Safe mode refuses unmerged.
	if _, err := DeleteBranch(r, "feat", false); err == nil {
		t.Fatal("safe delete of unmerged branch should refuse")
	} else if !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("wrong error: %v", err)
	}
	// Force deletes; ref-log keeps a deletion entry for recovery.
	featHex, _ := r.GetRef("feat")
	res, err := DeleteBranch(r, "feat", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tip != featHex {
		t.Fatalf("tip=%s want %s", res.Tip, featHex)
	}
	if tip, _ := r.GetRef("feat"); tip != "" {
		t.Fatal("ref survived deletion")
	}
	log, err := r.ReadReflog("feat", 1)
	if err != nil || len(log) != 1 || log[0].Action != "branch -d" {
		t.Fatalf("reflog=%+v err=%v", log, err)
	}
	if log[0].OldHex != featHex {
		t.Fatal("deletion entry lost the old tip")
	}
	// Merged branch deletes safely.
	if err := r.SetRef("side", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteBranch(r, "side", false); err != nil {
		t.Fatalf("merged delete failed: %v", err)
	}
	// Scratch guard: rebase-scratch with live state refuses even -D.
	headTip, _, _ := r.HeadCommit()
	if err := r.SetRef(RebaseBranch, cas.Hex(headTip)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.LrmDir, "REBASE_STATE"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteBranch(r, RebaseBranch, true); err == nil {
		t.Fatal("scratch delete during rebase should refuse")
	}
}

// --- stash apply ---

func TestStashApplyKeepsShelf(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "v2\n")
	if _, err := StashPush(r, "keeper"); err != nil {
		t.Fatal(err)
	}
	e, err := StashApply(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Message != "keeper" {
		t.Fatalf("applied=%+v", e)
	}
	raw, _ := os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "v2\n" {
		t.Fatalf("workdir=%q", raw)
	}
	// Shelf still listed (unlike pop).
	ents, _ := StashList(r)
	if len(ents) != 1 {
		t.Fatalf("shelf dropped by apply: %+v", ents)
	}
	if _, err := StashApply(r, "stash@{9}"); err == nil {
		t.Fatal("apply of missing stash should fail")
	}
}

// --- merge-base ---

func TestMergeBase(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "base.txt", "base\n")
	c1 := commitAll(t, r, "c1")
	w(t, r.Root, "m.txt", "m\n")
	c2 := commitAll(t, r, "c2")
	base, err := MergeBase(r, c1, c2)
	if err != nil || base != c1 {
		t.Fatalf("base=%s err=%v", cas.Short(base), err)
	}
	// Disjoint histories have no base.
	orphan, err := r.DAG.Put(&dag.Commit{Version: 1, Tree: strings.Repeat("0", 64), Message: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeBase(r, orphan, c2); err == nil {
		t.Fatal("disjoint merge-base should fail")
	}
}

// --- cherry ---

func TestCherry(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "base.txt", "base\n")
	c1 := commitAll(t, r, "c1 base")
	if err := r.SetRef("feat", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "f1.txt", "f1\n")
	f1 := commitAll(t, r, "f1 feature")
	w(t, r.Root, "f2.txt", "f2\n")
	f2 := commitAll(t, r, "f2 feature")
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "main.txt", "m\n")
	c2 := commitAll(t, r, "c2 mainline")
	// Both feat commits missing upstream, oldest-first.
	ents, err := Cherry(r, c2, f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 || ents[0].Commit != cas.Hex(f1) || ents[1].Commit != cas.Hex(f2) {
		t.Fatalf("ents=%+v", ents)
	}
	for _, e := range ents {
		if e.Applied {
			t.Fatalf("should be +: %+v", e)
		}
	}
	if ents[0].Subject != "f1 feature" {
		t.Fatalf("subject=%q", ents[0].Subject)
	}
	// Empty range, empty result — not an error.
	if ents, err := Cherry(r, f2, f1); err != nil || len(ents) != 0 {
		t.Fatalf("empty=%v err=%v", ents, err)
	}
	// Cherry-pick f1 onto a fresh copy of c1: equivalent → '-'.
	if err := r.SetRef("exact", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("exact"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	if _, err := CherryPick(r, f1); err != nil {
		t.Fatal(err)
	}
	exactTip, _, _ := r.HeadCommit()
	ents, err = Cherry(r, exactTip, f1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || !ents[0].Applied {
		t.Fatalf("applied copy should be -: %+v", ents)
	}
}
