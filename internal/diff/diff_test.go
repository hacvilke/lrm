package diff

import (
	"strings"
	"testing"
)

func TestDiffLines(t *testing.T) {
	old := "a\nb\nc\nd"
	new := "a\nB\nc\nd\ne"
	p := DiffLines("f.txt", "o", "n", old, new)
	u := p.Unified()
	if !strings.Contains(u, "-b") || !strings.Contains(u, "+B") || !strings.Contains(u, "+e") {
		t.Fatalf("bad unified diff:\n%s", u)
	}
}

func TestBinary(t *testing.T) {
	p := DiffLines("b.bin", "o", "n", "a\x00b", "a\x00c")
	if !p.Binary {
		t.Fatal("should detect binary")
	}
}
