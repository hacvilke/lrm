package store

import (
	"os"
	"path/filepath"
	"testing"
)

// LRM keeps machine-wide state in ~/.lrm and repositories in <repo>/.lrm.
// findRoot used to accept any directory containing a .lrm entry, so a home
// directory looked like a repository -- and so did every non-repo folder
// inside it, because the walk reaches $HOME before giving up.
//
// Reported on Windows:
//
//	lrm dashboard
//	error: read config: open C:\Users\brandon\.lrm\config.json:
//	       The system cannot find the file specified.
func TestFindRootIgnoresMachineStateDirectory(t *testing.T) {
	home := t.TempDir()

	// Machine-wide state: a .lrm directory with no repository config.
	machine := filepath.Join(home, ".lrm")
	if err := os.MkdirAll(machine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(machine, "node.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := findRoot(home); err == nil {
		t.Error("the home directory was accepted as a repository root")
	}

	// And every non-repo folder inside it, which is the common case.
	docs := filepath.Join(home, "Documents")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := findRoot(docs)
	if err == nil {
		t.Fatal("a folder inside the home directory was accepted as a repository")
	}
	// The message must explain what was found rather than failing later
	// on a missing config.json.
	if msg := err.Error(); !contains(msg, "machine-wide") {
		t.Errorf("error does not explain the machine-state directory: %q", msg)
	}
}

// A real repository must still be found, including from a nested
// directory and when it lives inside a home directory that also holds
// machine state.
func TestFindRootFindsRealRepositories(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".lrm"), 0o755); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(home, "code", "project")
	deep := filepath.Join(repo, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	repoLrm := filepath.Join(repo, ".lrm")
	if err := os.MkdirAll(repoLrm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoLrm, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{repo, deep} {
		got, err := findRoot(start)
		if err != nil {
			t.Fatalf("findRoot(%s): %v", start, err)
		}
		if got != repo {
			t.Errorf("findRoot(%s) = %s, want %s", start, got, repo)
		}
	}
}

func TestIsRepoRootRequiresConfig(t *testing.T) {
	dir := t.TempDir()
	if isRepoRoot(dir) {
		t.Error("an empty directory is not a repo root")
	}
	lrm := filepath.Join(dir, ".lrm")
	if err := os.MkdirAll(lrm, 0o755); err != nil {
		t.Fatal(err)
	}
	if isRepoRoot(dir) {
		t.Error("a .lrm directory without config.json is machine state, not a repo")
	}
	if err := os.WriteFile(filepath.Join(lrm, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isRepoRoot(dir) {
		t.Error("a .lrm directory with config.json is a repo root")
	}

	// A *file* called .lrm must not count.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, ".lrm"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isRepoRoot(other) {
		t.Error("a file named .lrm is not a repository")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
