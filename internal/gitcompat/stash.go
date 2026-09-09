package gitcompat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/diff"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// findStashEntry resolves "" (newest), "N" or "stash@{N}" against a
// time-ordered list. Returns nil when there is no match.
func findStashEntry(entries []StashEntry, name string) *StashEntry {
	if len(entries) == 0 {
		return nil
	}
	if name == "" {
		return &entries[0]
	}
	norm := strings.TrimPrefix(name, "stash@{")
	norm = strings.TrimSuffix(norm, "}")
	for i, e := range entries {
		if e.Name == name || e.Name == "stash@{"+norm+"}" {
			return &entries[i]
		}
	}
	return nil
}

// shelfFileName strips "stash@{N}" back to the shelf file name "N".
func shelfFileName(entryName string) string {
	s := strings.TrimPrefix(entryName, "stash@{")
	return strings.TrimSuffix(s, "}")
}

// StashApply restores a shelf's tree into the workdir WITHOUT dropping the
// shelf (pop's non-destructive sibling). Empty name = newest.
func StashApply(r *store.Repo, name string) (*StashEntry, error) {
	entries, err := StashList(r)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no stash entries found")
	}
	pick := findStashEntry(entries, name)
	if pick == nil {
		return nil, fmt.Errorf("unknown stash %q", name)
	}
	if err := restoreShelfFiles(r, pick); err != nil {
		return nil, err
	}
	return pick, nil
}

// restoreShelfFiles writes a shelf's full tree into the workdir WITHOUT
// touching the index/refs, so `status` shows the restored delta.
func restoreShelfFiles(r *store.Repo, pick *StashEntry) error {
	h, err := cas.ParseHex(pick.Commit)
	if err != nil {
		return err
	}
	c, err := r.DAG.Get(h)
	if err != nil {
		return err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return err
	}
	for p, hx := range flat {
		oh, err := cas.ParseHex(hx)
		if err != nil {
			return err
		}
		if err := writeObjectToFile(r, oh, filepath.Join(r.Root, filepath.FromSlash(p))); err != nil {
			return fmt.Errorf("restore %s: %w", p, err)
		}
	}
	return nil
}

// --- show ---

// StashFileDiff is one parent→shelf file change.
type StashFileDiff struct {
	Path  string
	Kind  byte // 'A' added, 'M' modified, 'D' deleted
	Patch diff.Patch
}

// StashShowResult is the shelf's parent-vs-shelf diff.
type StashShowResult struct {
	Entry StashEntry
	Files []StashFileDiff // sorted by path
}

// StashShow diffs a shelf against the HEAD it was pushed from (its parent
// commit). Empty name = newest.
func StashShow(r *store.Repo, name string) (*StashShowResult, error) {
	entries, err := StashList(r)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no stash entries found")
	}
	pick := findStashEntry(entries, name)
	if pick == nil {
		return nil, fmt.Errorf("unknown stash %q", name)
	}
	sh, err := cas.ParseHex(pick.Commit)
	if err != nil {
		return nil, err
	}
	sc, err := r.DAG.Get(sh)
	if err != nil {
		return nil, err
	}
	if len(sc.Parents) == 0 {
		return nil, fmt.Errorf("stash %s has no parent to diff against", pick.Name)
	}
	ph, err := cas.ParseHex(sc.Parents[0])
	if err != nil {
		return nil, err
	}
	oldFlat, err := TreeAtCommit(r, ph)
	if err != nil {
		return nil, fmt.Errorf("parent tree: %w", err)
	}
	newFlat, err := TreeAtCommit(r, sh)
	if err != nil {
		return nil, fmt.Errorf("shelf tree: %w", err)
	}
	paths := map[string]bool{}
	for p := range oldFlat {
		paths[p] = true
	}
	for p := range newFlat {
		paths[p] = true
	}
	res := &StashShowResult{Entry: *pick}
	for p := range paths {
		oh, onOk := oldFlat[p]
		nh, nwOk := newFlat[p]
		var kind byte
		switch {
		case onOk && !nwOk:
			kind = 'D'
		case !onOk && nwOk:
			kind = 'A'
		case oh != nh:
			kind = 'M'
		default:
			continue
		}
		fd := StashFileDiff{Path: p, Kind: kind}
		fd.Patch = stashFilePatch(r, p, oh, onOk, nh, nwOk)
		res.Files = append(res.Files, fd)
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	return res, nil
}

// stashFilePatch builds the unified patch for one path; unreadable or
// oversized sides degrade to a Binary-marked patch (never an error).
func stashFilePatch(r *store.Repo, path, oh string, onOk bool, nh string, nwOk bool) diff.Patch {
	oldText, newText := "", ""
	if onOk {
		if h, err := cas.ParseHex(oh); err == nil {
			if raw, err := ReadObjectBytes(r.CAS, h, MaxReadBytes); err == nil && !diff.IsBinary(raw) {
				oldText = string(raw)
			} else {
				return diff.Patch{Path: path, OldHash: oh, NewHash: nh, Binary: true}
			}
		}
	}
	if nwOk {
		if h, err := cas.ParseHex(nh); err == nil {
			if raw, err := ReadObjectBytes(r.CAS, h, MaxReadBytes); err == nil && !diff.IsBinary(raw) {
				newText = string(raw)
			} else {
				return diff.Patch{Path: path, OldHash: oh, NewHash: nh, Binary: true}
			}
		}
	}
	return diff.DiffLines(path, oh, nh, oldText, newText)
}

// --- drop / clear ---

// StashDrop deletes one shelf (empty name = newest) and compacts the
// remaining numeric names to a dense 0..n-1 in age order (git parity:
// dropping stash@{1} renumbers the rest).
func StashDrop(r *store.Repo, name string) (*StashEntry, error) {
	entries, err := StashList(r)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no stash entries found")
	}
	pick := findStashEntry(entries, name)
	if pick == nil {
		return nil, fmt.Errorf("unknown stash %q", name)
	}
	if err := os.Remove(filepath.Join(shelfDir(r), shelfFileName(pick.Name))); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := compactShelves(r); err != nil {
		return nil, err
	}
	return pick, nil
}

// StashClear deletes every shelf. Returns the number removed.
func StashClear(r *store.Repo) (int, error) {
	ents, err := os.ReadDir(shelfDir(r))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(shelfDir(r), e.Name())); err != nil && !os.IsNotExist(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

// compactShelves renames numeric shelf files to dense 0..n-1 ordered
// oldest-first (by shelf commit time, mtime fallback). Non-numeric files
// are left untouched.
func compactShelves(r *store.Repo) error {
	dir := shelfDir(r)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type shelf struct {
		name string
		time int64
	}
	var numeric []shelf
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		tm := int64(0)
		if fi, err := e.Info(); err == nil {
			tm = fi.ModTime().UnixNano()
		}
		if raw, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			if h, err := cas.ParseHex(strings.TrimSpace(string(raw))); err == nil {
				if c, err := r.DAG.Get(h); err == nil {
					tm = c.Timestamp
				}
			}
		}
		numeric = append(numeric, shelf{e.Name(), tm})
	}
	sort.Slice(numeric, func(i, j int) bool {
		if numeric[i].time != numeric[j].time {
			return numeric[i].time < numeric[j].time
		}
		return numeric[i].name < numeric[j].name
	})
	// Two-phase rename via temp names to avoid collisions.
	tmps := make([]string, len(numeric))
	for i, sh := range numeric {
		tmp := filepath.Join(dir, fmt.Sprintf(".compact-%d.tmp", i))
		if err := os.Rename(filepath.Join(dir, sh.name), tmp); err != nil {
			return err
		}
		tmps[i] = tmp
	}
	for i, tmp := range tmps {
		if err := os.Rename(tmp, filepath.Join(dir, strconv.Itoa(i))); err != nil {
			return err
		}
	}
	return nil
}
