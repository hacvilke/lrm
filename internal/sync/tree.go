package sync

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
)

// BuildTreeFromMap builds + stores a Merkle tree from a flat path→blob-hash map.
func BuildTreeFromMap(store *cas.Store, flat map[string]string) (cas.Hash, error) {
	return buildLevel(store, flat, "")
}

func buildLevel(store *cas.Store, flat map[string]string, prefix string) (cas.Hash, error) {
	// Partition into direct files and subdirs.
	type dirSet map[string]bool
	files := map[string]string{}
	subs := dirSet{}
	for p, h := range flat {
		rel := p
		if prefix != "" {
			if !strings.HasPrefix(p, prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(p, prefix+"/")
		}
		if idx := strings.Index(rel, "/"); idx >= 0 {
			subs[rel[:idx]] = true
		} else {
			files[rel] = h
		}
	}
	var entries []merkle.TreeEntry
	for name, h := range files {
		entries = append(entries, merkle.TreeEntry{Name: name, Hash: h, Mode: 0o644})
	}
	subNames := make([]string, 0, len(subs))
	for s := range subs {
		subNames = append(subNames, s)
	}
	sort.Strings(subNames)
	for _, s := range subNames {
		subPrefix := s
		if prefix != "" {
			subPrefix = prefix + "/" + s
		}
		sh, err := buildLevel(store, flat, subPrefix)
		if err != nil {
			return cas.Nil, err
		}
		entries = append(entries, merkle.TreeEntry{Name: s, Hash: cas.Hex(sh), Mode: 0o755, IsDir: true})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	t := merkle.Tree{Version: 1, Entries: entries}
	raw, err := json.Marshal(t)
	if err != nil {
		return cas.Nil, err
	}
	typed := append([]byte("lrm-tree-v1\n"), raw...)
	h := sha256.Sum256(typed)
	return h, putRawAt(store, h, raw)
}

func putRawAt(store *cas.Store, h cas.Hash, raw []byte) error {
	if store.Exists(h) {
		return nil
	}
	dst := store.Path(h)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmpDir := filepath.Join(store.Dir(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "tree-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// copyBlobToAddress files already-stored blob bytes (under got) at the
// domain-separated address want (for trees/commits whose address includes
// a prefix). Streams with bounded memory.
func copyBlobToAddress(e *Engine, got, want cas.Hash) error {
	if e.Repo.CAS.Exists(want) {
		return nil
	}
	rc, err := e.Repo.CAS.Get(got)
	if err != nil {
		return err
	}
	defer rc.Close()
	dst := e.Repo.CAS.Path(want)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmpDir := filepath.Join(e.Repo.CAS.Dir(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "obj-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.CopyBuffer(tmp, rc, make([]byte, 32*1024)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
