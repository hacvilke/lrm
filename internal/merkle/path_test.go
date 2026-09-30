package merkle

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// CleanTreePath is the single gate between peer-supplied paths and the
// filesystem, so it is tested exhaustively: everything here that is
// accepted can end up as a path under the working directory.
func TestCleanTreePathTable(t *testing.T) {
	good := []string{
		"a", "a/b.txt", "docs/index.html", "a b/c#d", ".hidden", ".lrmignore",
		"a/..b", "..a/b", "a/.b", "x/y/z/deep.txt", "weird ; name", "quote'name",
	}
	for _, p := range good {
		if got, err := CleanTreePath(p); err != nil || got != p {
			t.Errorf("CleanTreePath(%q) = %q, %v — want accepted unchanged", p, got, err)
		}
	}
	bad := []string{
		"", "/etc/passwd", "/", "..", "../x", "a/../b", "a/..", "..\\x",
		"a//b", "a/./b", "./a", "a/", "/a", "a\x00b", "\\", `a\b`,
		"....//....//x", "\u202e/tmp",
	}
	for _, p := range bad {
		if _, err := CleanTreePath(p); err == nil {
			t.Errorf("CleanTreePath(%q) accepted a hostile path", p)
		}
	}
}

func TestCleanTreePathsRefusesWholeSet(t *testing.T) {
	err := CleanTreePaths([]string{"ok/file.txt", "fine/other.txt", "../escape"})
	if err == nil {
		t.Fatal("hostile set accepted")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Fatalf("error should name the offender, got: %v", err)
	}
	if err := CleanTreePaths([]string{"a", "b/c", "d/e/f"}); err != nil {
		t.Fatalf("clean set refused: %v", err)
	}
}

// A tree object must never be confusable with a blob of the same bytes.
func TestTreeAddressIsDomainSeparated(t *testing.T) {
	raw := []byte(`{"version":1,"entries":[]}`)
	if TreeAddress(raw) == rawAddress(raw) {
		t.Fatal("tree address equals the plain blob address — domain separation broken")
	}
	if TreeAddress(raw) != TreeAddress(append([]byte{}, raw...)) {
		t.Fatal("tree addressing is not deterministic")
	}
}

// rawAddress is the plain content address of raw (what a blob would get).
func rawAddress(raw []byte) [32]byte { return sha256.Sum256(raw) }
