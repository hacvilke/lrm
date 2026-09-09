package gitcompat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// Notes attach free-form annotations to commits without rewriting history
// (git-notes reversed to local standing: plain text files, never synced).
//
// Layout: .lrm/refs/notes/<full-commit-hex>, raw UTF-8 text.

func notesDir(r *store.Repo) string { return filepath.Join(r.LrmDir, "refs", "notes") }

// NoteAdd attaches text to commit. Refuses to overwrite unless force set.
func NoteAdd(r *store.Repo, commit cas.Hash, text string, force bool) error {
	if !r.DAG.Has(commit) {
		return fmt.Errorf("unknown commit %s", cas.Short(commit))
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty note (pass -m TEXT)")
	}
	if err := os.MkdirAll(notesDir(r), 0o755); err != nil {
		return err
	}
	p := filepath.Join(notesDir(r), cas.Hex(commit))
	if !force {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("note already exists for %s (remove it first or pass --force)", cas.Short(commit))
		}
	}
	return os.WriteFile(p, []byte(strings.TrimRight(text, "\n")+"\n"), 0o644)
}

// NoteShow returns the note text for commit, or an error when absent.
func NoteShow(r *store.Repo, commit cas.Hash) (string, error) {
	raw, err := os.ReadFile(filepath.Join(notesDir(r), cas.Hex(commit)))
	if err != nil {
		return "", fmt.Errorf("no note for %s", cas.Short(commit))
	}
	return strings.TrimRight(string(raw), "\n"), nil
}

// NoteEntry is one annotated commit.
type NoteEntry struct {
	Commit string // full hex
	Text   string
}

// NoteList returns all notes ordered by commit hex.
func NoteList(r *store.Repo) ([]NoteEntry, error) {
	ents, err := os.ReadDir(notesDir(r))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []NoteEntry
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if _, err := cas.ParseHex(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(notesDir(r), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, NoteEntry{Commit: e.Name(), Text: strings.TrimRight(string(raw), "\n")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Commit < out[j].Commit })
	return out, nil
}

// NoteRemove deletes the note on commit.
func NoteRemove(r *store.Repo, commit cas.Hash) error {
	p := filepath.Join(notesDir(r), cas.Hex(commit))
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("no note for %s", cas.Short(commit))
	}
	return os.Remove(p)
}
