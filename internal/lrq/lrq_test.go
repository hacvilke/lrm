package lrq

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

func mustParse(t *testing.T, src string) []Query {
	t.Helper()
	q, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return q
}

func mustParseFail(t *testing.T, src string) {
	t.Helper()
	if _, err := Parse(src); err == nil {
		t.Fatalf("expected parse error for %q", src)
	}
}

func TestParseAllKinds(t *testing.T) {
	qs := mustParse(t, `
# comment line
// c++ style too
peers;
STATUS;
branches;
tags;
commits on main limit 3;
files at HEAD path like "src/*.go" bigger than 1KB limit 10;
show v1.0;
find "todo" in dev;
`)
	if len(qs) != 8 {
		t.Fatalf("got %d queries", len(qs))
	}
	kinds := []string{"peers", "status", "branches", "tags", "commits", "files", "show", "find"}
	for i, k := range kinds {
		if qs[i].Kind != k {
			t.Fatalf("q%d kind=%s want %s", i, qs[i].Kind, k)
		}
	}
	if qs[4].Ref != "main" || qs[4].Limit != 3 {
		t.Fatalf("commits opts: %+v", qs[4])
	}
	if qs[5].Pattern != "src/*.go" || qs[5].Bigger != 1024 || qs[5].Limit != 10 {
		t.Fatalf("files opts: %+v", qs[5])
	}
	if qs[6].Ref != "v1.0" || qs[7].Pattern != "todo" || qs[7].Ref != "dev" {
		t.Fatalf("show/find opts: %+v %+v", qs[6], qs[7])
	}
}

func TestParseBareAndPlural(t *testing.T) {
	qs := mustParse(t, "from commits; from files;")
	if qs[0].Kind != "commits" || qs[1].Kind != "files" {
		t.Fatalf("got %+v", qs)
	}
	qs = mustParse(t, "commits; files;")
	if qs[0].Kind != "commits" || qs[1].Kind != "files" {
		t.Fatalf("got %+v", qs)
	}
}

func TestParseSizes(t *testing.T) {
	qs := mustParse(t, `files smaller than 1.5MB;`)[0]
	if qs.Small != int64(1.5*1024*1024) {
		t.Fatalf("small=%d", qs.Small)
	}
	qs2 := mustParse(t, `files bigger than 2GB;`)[0]
	if qs2.Bigger != 2*1024*1024*1024 {
		t.Fatalf("bigger=%d", qs2.Bigger)
	}
	mustParseFail(t, `files bigger than 10XB;`)
}

func TestParseErrors(t *testing.T) {
	mustParseFail(t, `commits`)               // no semicolon
	mustParseFail(t, `bogus;`)                // unknown table
	mustParseFail(t, `commits limit 0;`)      // limit floor
	mustParseFail(t, `commits limit 9999;`)   // limit ceiling
	mustParseFail(t, `commits limit abc;`)    // not a number
	mustParseFail(t, `show;`)                 // show needs ref
	mustParseFail(t, `find;`)                 // find needs substring
	mustParseFail(t, `files at;`)             // dangling option
	mustParseFail(t, `files path like abc;`)  // unquoted glob
	mustParseFail(t, `commits frobnicate 1;`) // unknown option
	mustParseFail(t, `"unterminated;`)        // bad string
}

func TestTableRender(t *testing.T) {
	tb := Table{Title: "demo", Header: []string{"a", "bb"}, Rows: [][]string{{"1", "22"}, {"333", "4"}}}
	out := tb.Render()
	if !strings.Contains(out, "demo") || !strings.Contains(out, "2 row(s)") {
		t.Fatalf("render:\n%s", out)
	}
	empty := Table{Title: "e", Header: []string{"x"}}
	if !strings.Contains(empty.Render(), "(empty)") {
		t.Fatalf("empty render:\n%s", empty.Render())
	}
}

// seedRepo builds a repo with 2 commits on main, a dev branch, and tag v1.
func seedRepo(t *testing.T, dir string) string {
	t.Helper()
	r, err := store.Init(dir, "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	commit := func(name, content, msg string) string {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
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
		_ = r.Index.Save()
		return cas.Hex(h)
	}
	h1 := commit("a.txt", "hello", "first")
	commit("b.txt", "world!", "second")
	tip, _ := r.GetRef("main")
	if err := r.SetRef("dev", tip); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTag("v1", h1); err != nil {
		t.Fatal(err)
	}
	return h1
}

func TestEngineQueries(t *testing.T) {
	dir := t.TempDir()
	h1 := seedRepo(t, dir)
	qf := filepath.Join(dir, "q.lrq")
	_ = os.WriteFile(qf, []byte(`
status;
branches;
tags;
commits on main limit 5;
commits on v1;
files at main limit 20;
files at v1 path like "*.txt";
show v1;
find "a" in main;
`), 0o644)
	var buf bytes.Buffer
	res := RunFile(qf, Options{WorkDir: dir, Out: &buf})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
	if len(res.Tables) != 9 {
		t.Fatalf("got %d tables", len(res.Tables))
	}
	out := buf.String()
	for _, want := range []string{"status", "main", "dev", "v1", "first", "second",
		"a.txt", "b.txt", h1[:12], "2 row(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Files at v1 (first commit) must list only a.txt (b.txt came later).
	filesV1 := res.Tables[6].Render()
	if !strings.Contains(filesV1, "a.txt") || strings.Contains(filesV1, "b.txt") {
		t.Fatalf("files@v1 wrong:\n%s", filesV1)
	}
	// Report file written with tables + PASS verdict.
	rep, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rep), "verdict: PASS") || !strings.Contains(string(rep), "== status") {
		t.Fatalf("report:\n%s", rep)
	}
}

func TestEngineBadRef(t *testing.T) {
	dir := t.TempDir()
	seedRepo(t, dir)
	qf := filepath.Join(dir, "bad.lrq")
	_ = os.WriteFile(qf, []byte(`show nosuchref;`), 0o644)
	res := RunFile(qf, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if res.Passed {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Error, "unknown ref") {
		t.Fatalf("err=%s", res.Error)
	}
	rep, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(rep), "verdict: FAIL") {
		t.Fatalf("report:\n%s", rep)
	}
}

func TestEngineNoRepo(t *testing.T) {
	dir := t.TempDir()
	qf := filepath.Join(dir, "q.lrq")
	_ = os.WriteFile(qf, []byte(`status;`), 0o644)
	res := RunFile(qf, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if res.Passed {
		t.Fatal("expected failure outside repo")
	}
	if res.ReportPath == "" {
		t.Fatal("expected report even on failure")
	}
}

func TestPeersEmpty(t *testing.T) {
	dir := t.TempDir()
	seedRepo(t, dir)
	qf := filepath.Join(dir, "p.lrq")
	_ = os.WriteFile(qf, []byte(`peers;`), 0o644)
	res := RunFile(qf, Options{WorkDir: dir, Out: &bytes.Buffer{}})
	if !res.Passed {
		t.Fatalf("res=%+v", res)
	}
}
