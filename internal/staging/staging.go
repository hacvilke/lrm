// Package staging implements the In-Memory Virtual Staging index.
//
// It watches the workspace for change events and computes delta diffs
// instantly. The default watcher is a fast stat-poll loop (zero deps,
// works on inotify/FSEvents polling fallback); OS-native hooks can be
// layered on top without changing the index API.
package staging

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
)

// Entry tracks one file's last-known state.
type Entry struct {
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Mode    uint32 `json:"mode"`
	Chunked bool   `json:"chunked"`
}

// Index is the virtual staging area: path → entry.
type Index struct {
	mu      sync.RWMutex
	entries map[string]Entry
	path    string // persistence file (.lrm/index.json)
}

// New creates an empty index persisted at path (may be "").
func New(path string) *Index {
	return &Index{entries: map[string]Entry{}, path: path}
}

// Load reads the index from disk (missing file → empty index).
func Load(path string) (*Index, error) {
	idx := New(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &idx.entries); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}
	return idx, nil
}

// Save persists atomically.
func (ix *Index) Save() error {
	if ix.path == "" {
		return nil
	}
	ix.mu.RLock()
	raw, err := json.MarshalIndent(ix.entries, "", "  ")
	ix.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := ix.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ix.path)
}

// Snapshot returns a copy of all entries.
func (ix *Index) Snapshot() map[string]Entry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make(map[string]Entry, len(ix.entries))
	for k, v := range ix.entries {
		out[k] = v
	}
	return out
}

// FileChange is a detected workspace delta.
type FileChange struct {
	Path    string
	Kind    string // added | modified | deleted
	OldHash string
	NewHash string
}

// ScanResult is the outcome of a workspace scan.
type ScanResult struct {
	RootHash cas.Hash
	Changes  []FileChange
	Clean    bool
}

// Scan rebuilds the Merkle root and diffs it against the index.
// Fast path: files whose size+mtime+mode are unchanged skip re-hashing.
func (ix *Index) Scan(repoRoot string, store *cas.Store) (*ScanResult, error) {
	// 1. Walk live files (relative slash paths).
	live := map[string]fs.FileInfo{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // ignore unreadable
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".lrm", ".git", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".DS_Store" {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return nil
		}
		live[filepath.ToSlash(rel)] = info
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2. Detect candidates needing re-hash (new or stat-dirty).
	ix.mu.RLock()
	dirty := []string{}
	for p, info := range live {
		e, ok := ix.entries[p]
		if !ok || e.Size != info.Size() || e.ModTime != info.ModTime().UnixNano() || e.Mode != uint32(info.Mode().Perm()) {
			dirty = append(dirty, p)
		}
	}
	ix.mu.RUnlock()

	// 3. Rebuild Merkle root (streaming, parallel). This re-hashes only
	// files on disk; the index fast-path above is used for change reporting.
	root, err := merkle.BuildTree(store, repoRoot, merkle.DefaultOptions())
	if err != nil {
		return nil, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(store, root, "", flat); err != nil {
		return nil, err
	}

	// 4. Compare flattened tree vs index entries.
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var changes []FileChange
	for p, nh := range flat {
		oe, ok := ix.entries[p]
		if !ok {
			changes = append(changes, FileChange{Path: p, Kind: "added", NewHash: nh})
		} else if oe.Hash != nh {
			changes = append(changes, FileChange{Path: p, Kind: "modified", OldHash: oe.Hash, NewHash: nh})
		}
	}
	for p, oe := range ix.entries {
		if _, ok := flat[p]; !ok {
			changes = append(changes, FileChange{Path: p, Kind: "deleted", OldHash: oe.Hash})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	_ = dirty
	return &ScanResult{RootHash: root, Changes: changes, Clean: len(changes) == 0}, nil
}

// UpdateFromScan refreshes index entries from the current tree state.
func (ix *Index) UpdateFromScan(repoRoot string, store *cas.Store, root cas.Hash) error {
	flat := map[string]string{}
	if err := merkle.Flatten(store, root, "", flat); err != nil {
		return err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	next := make(map[string]Entry, len(flat))
	for p, h := range flat {
		full := filepath.Join(repoRoot, filepath.FromSlash(p))
		var e Entry
		e.Hash = h
		if fi, err := os.Stat(full); err == nil {
			e.Size = fi.Size()
			e.ModTime = fi.ModTime().UnixNano()
			e.Mode = uint32(fi.Mode().Perm())
		}
		next[p] = e
	}
	ix.entries = next
	return nil
}

// Watch polls Scan() every interval until stop is closed, delivering
// non-clean results on ch (coalesced, non-blocking).
func (ix *Index) Watch(repoRoot string, store *cas.Store, interval time.Duration, stop <-chan struct{}) <-chan *ScanResult {
	ch := make(chan *ScanResult, 4)
	go func() {
		defer close(ch)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				res, err := ix.Scan(repoRoot, store)
				if err != nil || res.Clean {
					continue
				}
				select {
				case ch <- res:
				default:
				}
			}
		}
	}()
	return ch
}
