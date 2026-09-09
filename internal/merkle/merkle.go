// Package merkle builds Instant Merkle Trees over the workspace.
//
// A workspace is hashed recursively: files → blobs (or chunk manifests),
// directories → tree objects, workspace → single root hash. If a single
// character changes anywhere, hashes mutate up to the root (Git model).
//
// Indexing is parallel (worker pool) and streaming (files never fully
// buffered beyond one chunk).
package merkle

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
)

// TreeEntry is one entry in a tree object.
type TreeEntry struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"` // hex: blob hash, manifest hash, or subtree hash
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	Chunked bool   `json:"chunked,omitempty"` // true if Hash is a chunk manifest
}

// Tree is a directory object.
type Tree struct {
	Version int         `json:"version"`
	Entries []TreeEntry `json:"entries"` // sorted by Name
}

// Change describes a diff between two trees.
type Change struct {
	Path    string
	OldHash string
	NewHash string
	Kind    string // "added", "modified", "deleted"
}

// Ignored top-level entries (never hashed).
func ignored(name string) bool {
	switch name {
	case ".lrm", ".git", ".svn", ".hg", "node_modules", "__pycache__", ".DS_Store":
		return true
	}
	return false
}

// BuildOptions tunes indexing.
type BuildOptions struct {
	ChunkSize int
	Workers   int
}

// DefaultOptions returns sane defaults.
func DefaultOptions() BuildOptions {
	return BuildOptions{ChunkSize: chunker.DefaultChunk, Workers: 8}
}

// BuildTree recursively indexes root into the CAS, returning the root hash.
// root itself is not included in entry paths (paths are slash-relative).
func BuildTree(store *cas.Store, root string, opt BuildOptions) (cas.Hash, error) {
	if opt.ChunkSize == 0 {
		opt.ChunkSize = chunker.DefaultChunk
	}
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	ch := chunker.New(opt.ChunkSize)
	return buildDir(store, root, root, ch, opt.Workers, LoadIgnore(root))
}

func buildDir(store *cas.Store, fsRoot, dir string, ch *chunker.Chunker, workers int, ign *Matcher) (cas.Hash, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return cas.Nil, err
	}
	type result struct {
		entry TreeEntry
		err   error
	}
	results := make([]result, 0, len(entries))
	// Directories are recursed synchronously (depth-first keeps memory flat);
	// files are hashed in parallel.
	var wg sync.WaitGroup
	resCh := make(chan result, len(entries))
	sem := make(chan struct{}, workers)

	for _, e := range entries {
		if ignored(e.Name()) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if rel, err := filepath.Rel(fsRoot, full); err == nil {
			if ign.Ignored(filepath.ToSlash(rel)) {
				continue
			}
		}
		if e.IsDir() {
			sub, err := buildDir(store, fsRoot, full, ch, workers, ign)
			if err != nil {
				return cas.Nil, err
			}
			results = append(results, result{entry: TreeEntry{
				Name: e.Name(), Hash: cas.Hex(sub), IsDir: true, Mode: 0o755,
			}})
			continue
		}
		// Regular file (symlinks followed; other types skipped).
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(name, full string, info fs.FileInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			h, _, single, err := ch.ChunkFileToStore(store, full)
			if err != nil {
				resCh <- result{err: fmt.Errorf("hash %s: %w", full, err)}
				return
			}
			resCh <- result{entry: TreeEntry{
				Name: name, Hash: cas.Hex(h), Mode: uint32(info.Mode().Perm()),
				Size: info.Size(), Chunked: !single,
			}}
		}(e.Name(), full, info)
	}
	wg.Wait()
	close(resCh)
	for r := range resCh {
		if r.err != nil {
			return cas.Nil, r.err
		}
		results = append(results, r)
	}

	treeEntries := make([]TreeEntry, 0, len(results))
	for _, r := range results {
		treeEntries = append(treeEntries, r.entry)
	}
	sort.Slice(treeEntries, func(i, j int) bool { return treeEntries[i].Name < treeEntries[j].Name })
	t := Tree{Version: 1, Entries: treeEntries}
	raw, err := json.Marshal(t)
	if err != nil {
		return cas.Nil, err
	}
	// Hash canonically: prefix domain separator so tree hashes never
	// collide with blob hashes of identical bytes.
	typed := append([]byte("lrm-tree-v1\n"), raw...)
	h := sha256.Sum256(typed)
	// Store under its own content address (store raw; address derived from typed).
	if err := putRawAt(store, h, raw); err != nil {
		return cas.Nil, err
	}
	return h, nil
}

// putRawAt stores raw bytes at an explicit address (used for domain-separated
// tree hashing: address = SHA256(prefix+raw)).
func putRawAt(store *cas.Store, h cas.Hash, raw []byte) error {
	if store.Exists(h) {
		return nil
	}
	// Write through temp + rename using the store dir layout.
	got, err := store.PutBytes(raw)
	if err != nil {
		return err
	}
	if got == h {
		return nil
	}
	// Content differs from address (domain separation): copy blob.
	rc, err := store.Get(got)
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeObjectAt(store.Dir(), h, rc)
}

func writeObjectAt(objectsDir string, h cas.Hash, r interface {
	Read([]byte) (int, error)
}) error {
	// Reuse cas path layout.
	hexStr := cas.Hex(h)
	dst := filepath.Join(objectsDir, hexStr[:2], hexStr[2:])
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(objectsDir, "tmp"), "tree-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				_ = tmp.Close()
				return werr
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// LoadTree loads a tree object by hash.
func LoadTree(store *cas.Store, h cas.Hash) (*Tree, error) {
	raw, err := store.GetBytes(h, 64<<20)
	if err != nil {
		return nil, err
	}
	var t Tree
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode tree %s: %w", cas.Short(h), err)
	}
	return &t, nil
}

// Flatten returns path→hash for all files under a tree (recursive).
func Flatten(store *cas.Store, root cas.Hash, prefix string, out map[string]string) error {
	if root == cas.Nil {
		return nil
	}
	t, err := LoadTree(store, root)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.IsDir {
			h, err := cas.ParseHex(e.Hash)
			if err != nil {
				return err
			}
			if err := Flatten(store, h, p, out); err != nil {
				return err
			}
			continue
		}
		out[p] = e.Hash
	}
	return nil
}

// Diff compares two root hashes, returning file-level changes.
func Diff(store *cas.Store, oldRoot, newRoot cas.Hash) ([]Change, error) {
	oldM := map[string]string{}
	newM := map[string]string{}
	if oldRoot != cas.Nil {
		if err := Flatten(store, oldRoot, "", oldM); err != nil {
			return nil, err
		}
	}
	if newRoot != cas.Nil {
		if err := Flatten(store, newRoot, "", newM); err != nil {
			return nil, err
		}
	}
	var changes []Change
	for p, nh := range newM {
		oh, ok := oldM[p]
		if !ok {
			changes = append(changes, Change{Path: p, NewHash: nh, Kind: "added"})
		} else if oh != nh {
			changes = append(changes, Change{Path: p, OldHash: oh, NewHash: nh, Kind: "modified"})
		}
	}
	for p, oh := range oldM {
		if _, ok := newM[p]; !ok {
			changes = append(changes, Change{Path: p, OldHash: oh, Kind: "deleted"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}
