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
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/merkle"
)

// maxControlObject caps tree/commit objects accepted through the verified
// copy path. Trees and commits are small by design; anything larger filed
// under a control address is either a bug or an attack.
const maxControlObject = 16 << 20

// BuildTreeFromMap builds + stores a Merkle tree from a flat path→blob-hash
// map. Keys must be clean tree paths: flattened maps can come from PEER trees,
// and a hostile key like "/x" would otherwise send buildLevel into infinite
// recursion (empty-name subdirectory) instead of failing.
func BuildTreeFromMap(store *cas.Store, flat map[string]string) (cas.Hash, error) {
	keys := make([]string, 0, len(flat))
	for p := range flat {
		keys = append(keys, p)
	}
	if err := merkle.CleanTreePaths(keys); err != nil {
		return cas.Nil, fmt.Errorf("refusing to build tree: %w", err)
	}
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
	h := merkle.TreeAddress(raw)
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
// explicit address want — after PROVING the bytes hash there under the
// repository's own addressing rules (blob, or domain-separated tree/commit).
//
// This is the last line of defence against CAS poisoning: a peer chooses
// both the bytes it sends and the address it claims they belong to. Filing
// unverified bytes at a requested address would let a hostile peer decide
// what "the tree of commit X" contains, so a mismatch is refused and the
// bytes are dropped. Streaming, bounded memory, hard size cap.
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

	// One pass, three candidate addresses: raw blob, tree, commit.
	hBlob := sha256.New()
	hTree := sha256.New()
	hCommit := sha256.New()
	_, _ = hTree.Write([]byte(merkle.TreeDomain))
	_, _ = hCommit.Write([]byte(dag.CommitDomain))
	var blob, tree, commit [32]byte
	mw := io.MultiWriter(tmp, hBlob, hTree, hCommit)
	if _, err := io.CopyBuffer(mw, io.LimitReader(rc, maxControlObject+1), make([]byte, 32*1024)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy object: %w", err)
	}
	if fi, err := tmp.Stat(); err == nil && fi.Size() > maxControlObject {
		_ = tmp.Close()
		return fmt.Errorf("object larger than control-object cap (%d bytes) — refusing", maxControlObject)
	}
	copy(blob[:], hBlob.Sum(nil))
	copy(tree[:], hTree.Sum(nil))
	copy(commit[:], hCommit.Sum(nil))
	if cas.Hash(blob) != want && cas.Hash(tree) != want && cas.Hash(commit) != want {
		_ = tmp.Close()
		return fmt.Errorf("object bytes do not hash to the requested address %s (possible poisoning) — object dropped", cas.Short(want))
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
