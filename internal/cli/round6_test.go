package cli

import (
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/store"
)

func TestBranchAtTip(t *testing.T) {
	r, err := store.Init(filepath.Join(t.TempDir(), "proj"), "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	hexA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := r.SetRef("main", hexA); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRef("feat", hexB); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRef("alias", hexA); err != nil {
		t.Fatal(err)
	}
	branches, _ := r.ListBranches()
	if !isBranchName(branches, "main") || isBranchName(branches, "ghost") {
		t.Fatal("isBranchName wrong")
	}
	// Sorted-first of the two branches sharing hexA.
	if got := branchAtTip(r, branches, hexA); got != "alias" {
		t.Fatalf("atTip=%q", got)
	}
	if got := branchAtTip(r, branches, hexB); got != "feat" {
		t.Fatalf("atTip=%q", got)
	}
	if got := branchAtTip(r, branches, "cccc"); got != "" {
		t.Fatalf("atTip=%q", got)
	}
}
