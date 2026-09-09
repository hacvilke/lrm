package gitcompat

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

func TestBisectFindsCulprit(t *testing.T) {
	r := mkRepo(t)
	var hashes []cas.Hash
	for i := 0; i < 7; i++ {
		w(t, r.Root, "v.txt", strings.Repeat("x", i+1)+"\n")
		hashes = append(hashes, commitAll(t, r, "c"+string(rune('0'+i))))
	}
	// Culprit is c3 (index 3): everything before good, c3+ bad.
	step, err := BisectStart(r, hashes[6], []cas.Hash{hashes[0]})
	if err != nil {
		t.Fatal(err)
	}
	steps := 0
	for !step.Done && len(step.Range) == 0 {
		steps++
		if steps > 10 {
			t.Fatal("bisect did not converge")
		}
		cur, err := cas.ParseHex(step.Next)
		if err != nil {
			t.Fatal(err)
		}
		verdict := "bad"
		// c0..c2 good, c3+ bad.
		for i, h := range hashes {
			if h == cur {
				if i < 3 {
					verdict = "good"
				}
				break
			}
		}
		step, err = BisectMark(r, verdict, cas.Nil, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !step.Done || step.Culprit != cas.Hex(hashes[3]) {
		t.Fatalf("culprit=%s range=%v (want %s)", step.Culprit, step.Range, cas.Hex(hashes[3])[:12])
	}
	// Workdir sits on the culprit; reset restores main tip.
	raw, _ := os.ReadFile(filepath.Join(r.Root, "v.txt"))
	if string(raw) != "xxxx\n" {
		t.Fatalf("workdir not on culprit: %q", raw)
	}
	br, err := BisectReset(r)
	if err != nil {
		t.Fatal(err)
	}
	if br != "main" {
		t.Fatalf("br=%s", br)
	}
	tip, _, _ := r.HeadCommit()
	if tip != hashes[6] {
		t.Fatal("reset did not restore tip")
	}
	if BisectActive(r) {
		t.Fatal("state file survived reset")
	}
}

func TestBisectRun(t *testing.T) {
	r := mkRepo(t)
	var hashes []cas.Hash
	for i := 0; i < 5; i++ {
		content := "ok\n"
		if i >= 2 {
			content = "BROKEN\n"
		}
		w(t, r.Root, "v.txt", content)
		hashes = append(hashes, commitAll(t, r, "c"+string(rune('0'+i))))
	}
	if _, err := BisectStart(r, hashes[4], []cas.Hash{hashes[0]}); err != nil {
		t.Fatal(err)
	}
	// Test fails (exit 1) when BROKEN is present.
	step, err := BisectRun(r, []string{"sh", "-c", "grep -q BROKEN v.txt && exit 1 || exit 0"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || step.Culprit != cas.Hex(hashes[2]) {
		t.Fatalf("culprit=%s (want %s)", step.Culprit, cas.Hex(hashes[2])[:12])
	}
	if _, err := BisectReset(r); err != nil {
		t.Fatal(err)
	}
}

func TestBisectErrors(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	h1 := commitAll(t, r, "one")
	w(t, r.Root, "f.txt", "v2\n")
	h2 := commitAll(t, r, "two")
	// good must be an ancestor of bad.
	if _, err := BisectStart(r, h1, []cas.Hash{h2}); err == nil {
		t.Fatal("expected ancestor error")
	}
	// same commit good+bad.
	if _, err := BisectStart(r, h1, []cas.Hash{h1}); err == nil {
		t.Fatal("expected same-commit error")
	}
	// dirty workdir refuses.
	w(t, r.Root, "f.txt", "dirty\n")
	if _, err := BisectStart(r, h2, nil); err == nil {
		t.Fatal("expected dirty error")
	}
	w(t, r.Root, "f.txt", "v2\n")
	res, _ := r.Index.Scan(r.Root, r.CAS)
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	_ = r.Index.Save()
	if _, err := BisectStart(r, h2, []cas.Hash{h1}); err != nil {
		t.Fatal(err)
	}
	// double start refuses.
	if _, err := BisectStart(r, h2, nil); err == nil {
		t.Fatal("expected double-start error")
	}
	if _, err := BisectReset(r); err != nil {
		t.Fatal(err)
	}
	// mark without session refuses.
	if _, err := BisectMark(r, "good", cas.Nil, false); err == nil {
		t.Fatal("expected no-session error")
	}
}

func TestBisectSkipRange(t *testing.T) {
	r := mkRepo(t)
	var hashes []cas.Hash
	for i := 0; i < 3; i++ {
		w(t, r.Root, "v.txt", strings.Repeat("y", i+1)+"\n")
		hashes = append(hashes, commitAll(t, r, "c"+string(rune('0'+i))))
	}
	step, err := BisectStart(r, hashes[2], []cas.Hash{hashes[0]})
	if err != nil {
		t.Fatal(err)
	}
	// Skip everything testable: start checked out the middle (c1).
	if step.Next == "" {
		t.Fatal("no candidate")
	}
	step, err = BisectMark(r, "skip", cas.Nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// c2 was marked bad at start (untestable pool excludes tested-bad),
	// c1 skipped, c0 tested-good: nothing testable remains -> range.
	if len(step.Range) == 0 {
		t.Fatalf("expected range report, got %+v", step)
	}
	if _, err := BisectReset(r); err != nil {
		t.Fatal(err)
	}
}

func TestRefLogFlow(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	h1 := commitAll(t, r, "first")
	w(t, r.Root, "f.txt", "v2\n")
	commitAll(t, r, "second line1\nsecond line2")
	ents, err := r.ReadReflog("main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("got %d entries", len(ents))
	}
	if ents[0].Action != "commit" || ents[0].Detail != "second line1" {
		t.Fatalf("newest=%+v", ents[0])
	}
	if ents[1].OldHex != store.ZeroHex || ents[1].NewHex != cas.Hex(h1) {
		t.Fatalf("oldest=%+v", ents[1])
	}
	// lastN slices newest-first.
	one, err := r.ReadReflog("main", 1)
	if err != nil || len(one) != 1 || one[0].Detail != "second line1" {
		t.Fatalf("last1=%+v", one)
	}
	// amend + reset append with their actions.
	if _, err := Amend(r, "amended"); err != nil {
		t.Fatal(err)
	}
	if err := ResetHard(r, h1); err != nil {
		t.Fatal(err)
	}
	ents, _ = r.ReadReflog("main", 0)
	if len(ents) != 4 || ents[0].Action != "reset --hard" || ents[1].Action != "amend" {
		t.Fatalf("actions: %+v", ents)
	}
	// Unknown branch reads empty, bad names error.
	empty, err := r.ReadReflog("nosuch", 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
	if _, err := r.ReadReflog("../evil", 0); err == nil {
		t.Fatal("expected bad-name error")
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "a.txt", "hello archive\n")
	w(t, r.Root, "sub/b.txt", "nested\n")
	// Chunked file (over the chunk threshold) exercises streaming.
	w(t, r.Root, "big.bin", strings.Repeat("0123456789abcdef", 20000))
	tip, _, _ := r.HeadCommit()
	_ = tip
	h := commitAll(t, r, "arch")
	// tar.gz
	tgz := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := Archive(r, h, "tar.gz", tgz); err != nil {
		t.Fatal(err)
	}
	got := readTarGz(t, tgz)
	if got["a.txt"] != "hello archive\n" || got["sub/b.txt"] != "nested\n" {
		t.Fatalf("tgz=%v", keysOf(got))
	}
	if len(got["big.bin"]) != 320000 {
		t.Fatalf("big.bin=%d bytes", len(got["big.bin"]))
	}
	// zip
	zp := filepath.Join(t.TempDir(), "out.zip")
	if err := Archive(r, h, "zip", zp); err != nil {
		t.Fatal(err)
	}
	got = readZip(t, zp)
	if got["a.txt"] != "hello archive\n" || len(got["big.bin"]) != 320000 {
		t.Fatalf("zip bad: %v", keysOf(got))
	}
	// plain tar + format guess + bad format.
	tp := filepath.Join(t.TempDir(), "out.tar")
	if err := Archive(r, h, "tar", tp); err != nil {
		t.Fatal(err)
	}
	if GuessFormat("x.TGZ") != "tar.gz" || GuessFormat("x.zip") != "zip" || GuessFormat("x.tar") != "tar" || GuessFormat("x.7z") != "" {
		t.Fatal("guess broken")
	}
	if err := Archive(r, h, "rar", tp); err == nil {
		t.Fatal("expected bad-format error")
	}
}

func readTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(raw)
	}
	return out
}

func readZip(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(raw)
	}
	return out
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestShortlog(t *testing.T) {
	r := mkRepo(t)
	w(t, r.Root, "f.txt", "v1\n")
	commitAll(t, r, "a1")
	if err := r.SetUser("bob"); err != nil {
		t.Fatal(err)
	}
	w(t, r.Root, "f.txt", "v2\n")
	commitAll(t, r, "b1")
	w(t, r.Root, "f.txt", "v3\n")
	commitAll(t, r, "b2")
	tip, _, _ := r.HeadCommit()
	rows, err := Shortlog(r, tip, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Author != "bob" || rows[0].Commits != 2 || rows[1].Author != "tester" {
		t.Fatalf("rows=%+v", rows)
	}
	rows, err = Shortlog(r, tip, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("limited=%+v", rows)
	}
}
