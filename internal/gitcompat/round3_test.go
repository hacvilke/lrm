package gitcompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// w writes a workdir file.
func w(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBlameBasic(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "one\ntwo\nthree\n")
	h1 := commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "one\nTWO\nthree\nfour\n")
	h2 := commitAll(t, r, "second")
	lines, err := Blame(r, h2, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 {
		t.Fatalf("got %d lines", len(lines))
	}
	want := map[int]string{1: cas.Hex(h1), 2: cas.Hex(h2), 3: cas.Hex(h1), 4: cas.Hex(h2)}
	for _, b := range lines {
		if b.Hash != want[b.Lineno] {
			t.Fatalf("line %d: got %s want %s", b.Lineno, b.Hash[:8], want[b.Lineno][:8])
		}
		if b.Author != "tester" || b.Text == "" {
			t.Fatalf("bad meta: %+v", b)
		}
	}
	// Blame at old ref sees old content.
	old, err := Blame(r, h1, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 3 || old[1].Hash != cas.Hex(h1) {
		t.Fatalf("old blame: %+v", old)
	}
	// Missing file / binary errors.
	if _, err := Blame(r, h2, "nope.txt"); err == nil {
		t.Fatal("expected error for missing file")
	}
	w(t, r.Root, "bin.dat", "a\x00b")
	h3 := commitAll(t, r, "bin")
	if _, err := Blame(r, h3, "bin.dat"); err == nil {
		t.Fatal("expected error for binary file")
	}
}

func TestBlameAddDelete(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "a\nb\nc\n")
	commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "b\nc\nd\n") // delete a, add d
	h2 := commitAll(t, r, "second")
	lines, err := Blame(r, h2, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines", len(lines))
	}
	if lines[2].Hash != cas.Hex(h2) || lines[2].Text != "d" {
		t.Fatalf("d misattributed: %+v", lines[2])
	}
	if lines[0].Hash == cas.Hex(h2) || lines[1].Hash == cas.Hex(h2) {
		t.Fatalf("b/c misattributed: %+v", lines)
	}
}

func TestCherryPick(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "keep.txt", "k\n")
	w(t, r.Root, "move.txt", "v1\n")
	commitAll(t, r, "base")
	// Side branch change.
	if err := r.SetRef("side", mustTip(t, r)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("side"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "move.txt", "v2\n")
	w(t, r.Root, "new.txt", "n\n")
	pickMe := commitAll(t, r, "side work")
	// Back on main, unrelated change.
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "keep.txt", "k2\n")
	commitAll(t, r, "main work")
	res, err := CherryPick(r, pickMe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hash == "" {
		t.Fatal("empty pick hash")
	}
	raw, _ := os.ReadFile(filepath.Join(r.Root, "move.txt"))
	if string(raw) != "v2\n" {
		t.Fatalf("move.txt=%q", raw)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "new.txt")); err != nil {
		t.Fatal("new.txt not picked")
	}
	// Message carries the trailer.
	h, _ := cas.ParseHex(res.Hash)
	c, _ := r.DAG.Get(h)
	if !strings.Contains(c.Message, "cherry-picked-from: ") || !strings.Contains(c.Message, "side work") {
		t.Fatalf("msg=%q", c.Message)
	}
}

func TestCherryPickConflict(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "base\n")
	commitAll(t, r, "base")
	if err := r.SetRef("side", mustTip(t, r)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch("side"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "f.txt", "side\n")
	pickMe := commitAll(t, r, "side")
	if err := r.SetHeadBranch("main"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "f.txt", "main\n")
	commitAll(t, r, "main")
	if _, err := CherryPick(r, pickMe); err == nil {
		t.Fatal("expected conflict error")
	} else if !strings.Contains(err.Error(), "f.txt") {
		t.Fatalf("err=%v", err)
	}
	// Workdir untouched by the failed pick.
	raw, _ := os.ReadFile(filepath.Join(r.Root, "f.txt"))
	if string(raw) != "main\n" {
		t.Fatalf("workdir clobbered: %q", raw)
	}
	// Dirty workdir refuses.
	w(t, r.Root, "f.txt", "dirty\n")
	if _, err := CherryPick(r, pickMe); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty err=%v", err)
	}
}

func mustTip(t *testing.T, r interface {
	HeadCommit() (cas.Hash, string, error)
}) string {
	t.Helper()
	h, _, err := r.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	return cas.Hex(h)
}

func TestGrep(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "a.txt", "hello world\nsecond line\n")
	w(t, r.Root, "sub/b.txt", "HELLO again\nnothing\n")
	w(t, r.Root, "bin.dat", "x\x00y hello\n")
	commitAll(t, r, "first")
	res, err := GrepWorkdir(r, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Path != "a.txt" || res.Matches[0].Lineno != 1 {
		t.Fatalf("matches=%+v", res.Matches)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped=%d (want 1 binary)", res.Skipped)
	}
	res, err = GrepWorkdir(r, "hello", true)
	if err != nil || len(res.Matches) != 2 {
		t.Fatalf("ci matches=%+v err=%v", res.Matches, err)
	}
	// Historical grep.
	tip, _, _ := r.HeadCommit()
	res, err = GrepRef(r, tip, "second", false)
	if err != nil || len(res.Matches) != 1 {
		t.Fatalf("ref matches=%+v err=%v", res.Matches, err)
	}
	if _, err := GrepWorkdir(r, "", false); err == nil {
		t.Fatal("expected empty-pattern error")
	}
}

func TestClean(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "tracked.txt", "t\n")
	commitAll(t, r, "first")
	w(t, r.Root, "loose.txt", "x\n")
	w(t, r.Root, "junk/a.txt", "x\n")
	w(t, r.Root, "junk/deep/b.txt", "x\n")
	// Dry run lists files.
	res, err := Clean(r, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 3 || res.Removed {
		t.Fatalf("dry=%+v", res)
	}
	// Dirs collapse.
	res, err = Clean(r, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Candidates, ",")
	if !strings.Contains(joined, "junk/") || strings.Contains(joined, "junk/a.txt") {
		t.Fatalf("collapsed=%v", res.Candidates)
	}
	// Force deletes; tracked survives.
	res, err = Clean(r, true, true, false)
	if err != nil || !res.Removed {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "loose.txt")); !os.IsNotExist(err) {
		t.Fatal("loose.txt survived")
	}
	if _, err := os.Stat(filepath.Join(r.Root, "junk")); !os.IsNotExist(err) {
		t.Fatal("junk/ survived")
	}
	if _, err := os.Stat(filepath.Join(r.Root, "tracked.txt")); err != nil {
		t.Fatal("tracked.txt wrongly deleted")
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".lrm", "config.json")); err != nil {
		t.Fatal(".lrm touched!")
	}
}

func TestDescribe(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	h1 := commitAll(t, r, "first")
	if _, err := Describe(r, h1); err == nil {
		t.Fatal("expected no-tag error")
	}
	if err := r.CreateTag("v1", cas.Hex(h1)); err != nil {
		t.Fatal(err)
	}
	got, err := Describe(r, h1)
	if err != nil || got != "v1" {
		t.Fatalf("got %q err %v", got, err)
	}
	w(t, r.Root, "f.txt", "v2\n")
	h2 := commitAll(t, r, "second")
	got, err = Describe(r, h2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "v1-1-g") || !strings.HasSuffix(got, cas.Hex(h2)[:12]) {
		t.Fatalf("got %q", got)
	}
}

func TestAmend(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	h1 := commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "v2\n")
	h2, err := Amend(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h1 {
		t.Fatal("amend did not move")
	}
	tip, _, _ := r.HeadCommit()
	if tip != h2 {
		t.Fatal("branch not at amended commit")
	}
	c, _ := r.DAG.Get(h2)
	if c.Message != "first" || len(c.Parents) != 0 {
		t.Fatalf("amended meta: %+v", c)
	}
	// Log still has ONE commit (replaced, not appended).
	_, commits, err := r.DAG.WalkTipOrder(h2, 10)
	if err != nil || len(commits) != 1 {
		t.Fatalf("walk=%d err=%v", len(commits), err)
	}
	// Nothing-to-amend errors.
	if _, err := Amend(r, ""); err == nil {
		t.Fatal("expected nothing-to-amend error")
	}
	// Message-only amend works.
	h3, err := Amend(r, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	c3, _ := r.DAG.Get(h3)
	if c3.Message != "renamed" {
		t.Fatalf("msg=%q", c3.Message)
	}
}

func TestConfigSave(t *testing.T) {
	r := mkRepo(t)
	if err := r.SetUser("  "); err == nil {
		t.Fatal("expected empty-user error")
	}
	if err := r.SetPort(99999); err == nil {
		t.Fatal("expected bad-port error")
	}
	if err := r.SetUser("bob"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPort(1234); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	root := r.Root
	_ = r.Close()
	r2, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if r2.Config.User != "bob" || r2.Config.Port != 1234 {
		t.Fatalf("cfg=%+v", r2.Config)
	}
}
