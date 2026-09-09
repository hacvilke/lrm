package gitcompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// --- notes ---

func TestNotesRoundTrip(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	h := commitAll(t, r, "first")
	if err := NoteAdd(r, h, "release checkpoint", false); err != nil {
		t.Fatal(err)
	}
	// Duplicate without force refuses.
	if err := NoteAdd(r, h, "other", false); err == nil {
		t.Fatal("dup add should refuse")
	}
	// Force overwrites.
	if err := NoteAdd(r, h, "forced", true); err != nil {
		t.Fatal(err)
	}
	text, err := NoteShow(r, h)
	if err != nil || text != "forced" {
		t.Fatalf("show=%q err=%v", text, err)
	}
	notes, err := NoteList(r)
	if err != nil || len(notes) != 1 || notes[0].Commit != cas.Hex(h) {
		t.Fatalf("list=%+v err=%v", notes, err)
	}
	if err := NoteRemove(r, h); err != nil {
		t.Fatal(err)
	}
	if _, err := NoteShow(r, h); err == nil {
		t.Fatal("show after remove should fail")
	}
	if notes, _ := NoteList(r); len(notes) != 0 {
		t.Fatalf("list after remove=%v", notes)
	}
	// Unknown commit refuses.
	if err := NoteAdd(r, cas.Nil, "x", false); err == nil {
		t.Fatal("add on unknown commit should fail")
	}
}

// --- stash show/drop/clear ---

func TestStashShowDropClear(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "v1\nmod1\n")
	w(t, r.Root, "s1.txt", "new1\n")
	if _, err := StashPush(r, "stash one"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "f.txt", "v1\nmod2\n")
	w(t, r.Root, "s2.txt", "new2\n")
	if _, err := StashPush(r, "stash two"); err != nil {
		t.Fatal(err)
	}
	// Show newest (default): one M + one A with unified patches.
	res, err := StashShow(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Message != "stash two" || len(res.Files) != 2 {
		t.Fatalf("show=%+v", res)
	}
	kinds := map[string]byte{}
	for _, f := range res.Files {
		kinds[f.Path] = f.Kind
		if f.Patch.Binary || len(f.Patch.Hunks) == 0 {
			t.Fatalf("no patch for %s", f.Path)
		}
	}
	if kinds["f.txt"] != 'M' || kinds["s2.txt"] != 'A' {
		t.Fatalf("kinds=%v", kinds)
	}
	// Show oldest by name.
	old, err := StashShow(r, "stash@{0}")
	if err != nil || old.Entry.Message != "stash one" {
		t.Fatalf("show oldest=%+v err=%v", old, err)
	}
	if _, err := StashShow(r, "stash@{9}"); err == nil {
		t.Fatal("show of missing stash should fail")
	}
	// Drop oldest compacts: stash@{1} becomes stash@{0}.
	dropped, err := StashDrop(r, "stash@{0}")
	if err != nil || dropped.Message != "stash one" {
		t.Fatalf("drop=%+v err=%v", dropped, err)
	}
	ents, _ := StashList(r)
	if len(ents) != 1 || ents[0].Name != "stash@{0}" || ents[0].Message != "stash two" {
		t.Fatalf("after drop=%+v", ents)
	}
	// Drop newest by default, then clear is a no-op count.
	if _, err := StashDrop(r, ""); err != nil {
		t.Fatal(err)
	}
	n, err := StashClear(r)
	if err != nil || n != 0 {
		t.Fatalf("clear=%d err=%v", n, err)
	}
	if _, err := StashDrop(r, ""); err == nil {
		t.Fatal("drop on empty should fail")
	}
}

// --- GC honors ref-logs ---

func TestGCProtectsReflogTips(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	commitAll(t, r, "first")
	w(t, r.Root, "doomed.txt", "gone\n")
	doomed := commitAll(t, r, "doomed tip")
	tip, _, _ := r.HeadCommit()
	if tip != doomed {
		t.Fatal("setup wrong")
	}
	// Reset away: doomed is now unreachable from any ref.
	c, err := r.DAG.Get(doomed)
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := cas.ParseHex(c.Parents[0])
	if err := ResetHard(r, parent); err != nil {
		t.Fatal(err)
	}
	// A true orphan (no ref, no log entry) must still be swept.
	orphan, err := r.DAG.Put(&dag.Commit{Version: 1, Tree: c.Tree, Message: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := GC(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if gc.Removed == 0 {
		t.Fatal("expected the orphan pruned")
	}
	if r.DAG.Has(orphan) {
		t.Fatal("orphan survived GC")
	}
	// The reset-away tip survives via its ref-log entry.
	if _, err := r.DAG.Get(doomed); err != nil {
		t.Fatalf("reflog tip was collected: %v", err)
	}
	flat, err := TreeAtCommit(r, doomed)
	if err != nil || flat["doomed.txt"] == "" {
		t.Fatalf("doomed tree broken: %v %v", flat, err)
	}
}

// --- .lrmignore ---

func TestIgnoreBuildCleanGrep(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, ".lrmignore", "*.log\nbuild/\n")
	w(t, r.Root, "keep.txt", "keep needle\n")
	w(t, r.Root, "debug.log", "drop needle\n")
	w(t, r.Root, "build/o.o", "drop needle\n")
	h := commitAll(t, r, "ignore test")
	flat, err := TreeAtCommit(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if flat["keep.txt"] == "" || flat[".lrmignore"] == "" {
		t.Fatalf("kept files missing: %v", flat)
	}
	if _, ok := flat["debug.log"]; ok {
		t.Fatal("debug.log should be ignored")
	}
	if _, ok := flat["build/o.o"]; ok {
		t.Fatal("build/o.o should be ignored")
	}
	// Grep skips ignored content.
	w(t, r.Root, "loose.log", "loose needle\n")
	gres, err := GrepWorkdir(r, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range gres.Matches {
		if strings.HasSuffix(m.Path, ".log") || strings.HasPrefix(m.Path, "build/") {
			t.Fatalf("grep hit ignored file %s", m.Path)
		}
	}
	if len(gres.Matches) != 1 || gres.Matches[0].Path != "keep.txt" {
		t.Fatalf("grep=%+v", gres.Matches)
	}
	// Clean hides ignored unless all=true.
	w(t, r.Root, "q.tmp", "q\n")
	cres, err := Clean(r, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cres.Candidates) != 1 || cres.Candidates[0] != "q.tmp" {
		t.Fatalf("clean=%v", cres.Candidates)
	}
	cres, err = Clean(r, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cres.Candidates, ",")
	if !strings.Contains(joined, "loose.log") || !strings.Contains(joined, "q.tmp") {
		t.Fatalf("clean -x=%v", cres.Candidates)
	}
}

// --- rebase ---

// mkDiverged builds: main c1→c2, feat c1→f1→f2 (disjoint files).
// Returns repo (HEAD on feat, workdir checked out).
func mkDiverged(t *testing.T) (*store.Repo, cas.Hash, cas.Hash) {
	t.Helper()
	r := mkRepo(t)
	w(t, r.Root, "base.txt", "base\n")
	c1 := commitAll(t, r, "c1 base")
	if err := r.SetRef("feat", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "feat1.txt", "f1\n")
	commitAll(t, r, "f1 feature one")
	w(t, r.Root, "feat2.txt", "f2\n")
	commitAll(t, r, "f2 feature two")
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "main.txt", "m\n")
	c2 := commitAll(t, r, "c2 mainline")
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	featTip, _, _ := r.HeadCommit()
	if err := sync.Checkout(r, featTip); err != nil {
		t.Fatal(err)
	}
	return r, c2, featTip
}

func chainMessages(t *testing.T, r *store.Repo, tip cas.Hash, n int) []string {
	t.Helper()
	var out []string
	h := tip
	for i := 0; i < n; i++ {
		c, err := r.DAG.Get(h)
		if err != nil {
			t.Fatal(err)
		}
		out = append([]string{c.Message}, out...)
		if len(c.Parents) == 0 {
			break
		}
		h, _ = cas.ParseHex(c.Parents[0])
	}
	return out
}

func TestRebaseClean(t *testing.T) {
	r, c2, featTip := mkDiverged(t)
	res, err := RebaseStart(r, c2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped {
		t.Fatalf("unexpected stop: %s", res.Message)
	}
	newTip, br, _ := r.HeadCommit()
	if br != "feat" || newTip == featTip {
		t.Fatalf("tip not moved: %s %s", br, cas.Short(newTip))
	}
	msgs := chainMessages(t, r, newTip, 4)
	want := []string{"c1 base", "c2 mainline", "f1 feature one", "f2 feature two"}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("chain=%v want=%v", msgs, want)
		}
		if strings.Contains(msgs[i], "cherry-picked-from") {
			t.Fatal("rebase must keep original messages (no trailer)")
		}
	}
	// Workdir holds the union.
	for _, f := range []string{"base.txt", "main.txt", "feat1.txt", "feat2.txt"} {
		if _, err := os.Stat(filepath.Join(r.Root, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	// No traces: scratch branch + state gone, branch reflogged.
	if ref, _ := r.GetRef(RebaseBranch); ref != "" {
		t.Fatal("scratch branch survived")
	}
	if _, err := os.Stat(filepath.Join(r.LrmDir, "REBASE_STATE")); !os.IsNotExist(err) {
		t.Fatal("state file survived")
	}
	log, err := r.ReadReflog("feat", 1)
	if err != nil || len(log) != 1 || log[0].Action != "rebase" {
		t.Fatalf("reflog=%+v err=%v", log, err)
	}
	// Idempotent: rebasing again is up-to-date, not an error.
	res, err = RebaseStart(r, c2)
	if err != nil || strings.Contains(res.Message, "replayed") {
		t.Fatalf("second rebase=%+v err=%v", res, err)
	}
}

func TestRebaseConflictContinue(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "a.txt", "base\n")
	c1 := commitAll(t, r, "c1")
	if err := r.SetRef("feat", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "a.txt", "feat side\n")
	commitAll(t, r, "f1 clash")
	featTip, _, _ := r.HeadCommit()
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "a.txt", "main side\n")
	c2 := commitAll(t, r, "c2 clash")
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, featTip); err != nil {
		t.Fatal(err)
	}
	res, err := RebaseStart(r, c2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || len(res.Files) != 1 || res.Files[0] != "a.txt" {
		t.Fatalf("stop=%+v", res)
	}
	// Branch untouched while stopped; HEAD sits on scratch.
	if tip, _ := r.GetRef("feat"); tip != cas.Hex(featTip) {
		t.Fatal("branch moved during stopped rebase")
	}
	if br, _ := r.HeadBranch(); br != RebaseBranch {
		t.Fatalf("HEAD=%s", br)
	}
	// A second start refuses while stopped.
	if _, err := RebaseStart(r, c2); err == nil {
		t.Fatal("double start should refuse")
	}
	// Resolve + continue replays with the ORIGINAL message.
	w(t, r.Root, "a.txt", "resolved\n")
	res, err = RebaseContinue(r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped {
		t.Fatalf("still stopped: %s", res.Message)
	}
	newTip, br, _ := r.HeadCommit()
	if br != "feat" {
		t.Fatalf("HEAD=%s", br)
	}
	msgs := chainMessages(t, r, newTip, 3)
	if msgs[0] != "c1" || msgs[1] != "c2 clash" || msgs[2] != "f1 clash" {
		t.Fatalf("chain=%v", msgs)
	}
	raw, _ := os.ReadFile(filepath.Join(r.Root, "a.txt"))
	if string(raw) != "resolved\n" {
		t.Fatalf("workdir=%q", raw)
	}
}

func TestRebaseAbort(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "a.txt", "base\n")
	c1 := commitAll(t, r, "c1")
	if err := r.SetRef("feat", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "a.txt", "feat side\n")
	commitAll(t, r, "f1")
	featTip, _, _ := r.HeadCommit()
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "a.txt", "main side\n")
	c2 := commitAll(t, r, "c2")
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, featTip); err != nil {
		t.Fatal(err)
	}
	res, err := RebaseStart(r, c2)
	if err != nil || !res.Stopped {
		t.Fatalf("start=%+v err=%v", res, err)
	}
	res, err = RebaseAbort(r)
	if err != nil {
		t.Fatal(err)
	}
	tip, br, _ := r.HeadCommit()
	if br != "feat" || tip != featTip {
		t.Fatalf("abort left %s @ %s", br, cas.Short(tip))
	}
	raw, _ := os.ReadFile(filepath.Join(r.Root, "a.txt"))
	if string(raw) != "feat side\n" {
		t.Fatalf("workdir=%q", raw)
	}
	if ref, _ := r.GetRef(RebaseBranch); ref != "" {
		t.Fatal("scratch branch survived abort")
	}
	if _, err := os.Stat(filepath.Join(r.LrmDir, "REBASE_STATE")); !os.IsNotExist(err) {
		t.Fatal("state survived abort")
	}
	// Continue/abort with no state both fail cleanly.
	if _, err := RebaseContinue(r); err == nil {
		t.Fatal("continue without state should fail")
	}
	if _, err := RebaseAbort(r); err == nil {
		t.Fatal("abort without state should fail")
	}
}

func TestRebaseRefusesMerge(t *testing.T) {
	r, c2, _ := mkDiverged(t)
	// Side branch off c1, merged into feat: merge commit on the feat path
	// while c2 stays outside feat's history. Find c1 via c2's parent.
	c2c, err := r.DAG.Get(c2)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := cas.ParseHex(c2c.Parents[0])
	if err := r.SetRef("side", cas.Hex(c1)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("side"); err != nil {
		t.Fatal(err)
	}
	if err := sync.Checkout(r, c1); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "side.txt", "s\n")
	s1 := commitAll(t, r, "s1")
	if err := r.SetHeadBranch("feat"); err != nil {
		t.Fatal(err)
	}
	featHex, _ := r.GetRef("feat")
	featTip, _ := cas.ParseHex(featHex)
	if err := sync.Checkout(r, featTip); err != nil {
		t.Fatal(err)
	}
	mres, err := sync.MergeLocal(r, s1, "side")
	if err != nil || mres.FastForwarded {
		t.Fatalf("merge=%+v err=%v", mres, err)
	}
	if _, err := RebaseStart(r, c2); err == nil {
		t.Fatal("rebase over a merge should refuse")
	} else if !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestRebaseDirtyGate(t *testing.T) {
	r, c2, _ := mkDiverged(t)
	w(t, r.Root, "dirty.txt", "x\n")
	if _, err := RebaseStart(r, c2); err == nil {
		t.Fatal("dirty workdir should gate rebase")
	}
}
