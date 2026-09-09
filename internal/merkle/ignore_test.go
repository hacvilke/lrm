package merkle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreMatcher(t *testing.T) {
	dir := t.TempDir()
	content := "# comment line\n\n*.log\nbuild/\n/doc/gen.txt\n!important.log\n"
	if err := os.WriteFile(filepath.Join(dir, ".lrmignore"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m := LoadIgnore(dir)
	cases := map[string]bool{
		"a.log": true, "x/y/z.log": true,
		"build": true, "build/o.o": true, "build/deep/x": true,
		"doc/gen.txt": true, "doc/keep.txt": false,
		"important.log": false, "sub/important.log": false,
		"keep.txt": false, "src/main.go": false,
	}
	for rel, want := range cases {
		if got := m.Ignored(rel); got != want {
			t.Errorf("%s: got %v want %v", rel, got, want)
		}
	}
	// Last match wins.
	m2 := &Matcher{rules: []IgnoreRule{
		{Pattern: "*.log"},
		{Pattern: "a.log", Negate: true},
		{Pattern: "a.log"},
	}}
	if !m2.Ignored("a.log") || m2.Ignored("b.log") == false {
		t.Fatal("last-wins broken")
	}
	// Missing file = match nothing, never nil-crash.
	if LoadIgnore(t.TempDir()).Ignored("a.log") {
		t.Fatal("empty matcher should ignore nothing")
	}
	var nilM *Matcher
	if nilM.Ignored("a.log") {
		t.Fatal("nil matcher should ignore nothing")
	}
}
