// Package patch builds human-readable unified patches between two Merkle
// roots (workdir trees or commit trees).
//
// Safety rails (LRM anti-patterns apply to the CLI too):
//   - Blobs are read with an explicit cap (default 1 MiB per file side).
//   - Binary files yield a "(binary differs)" note, never garbage.
//   - Chunk-manifest (large-file) entries yield a short "(large file)"
//     note instead of reassembling multi-GB content for display.
package patch

import (
	"encoding/json"
	"fmt"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/diff"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// DefaultMaxBytes caps how much of one file side is read for display.
const DefaultMaxBytes = 1 << 20 // 1 MiB

// FilePatch is one file's rendered change.
type FilePatch struct {
	Path    string
	Kind    string // added | modified | deleted | binary | large
	OldHash string
	NewHash string
	Patch   diff.Patch // populated for text added/modified/deleted
	Note    string     // populated for binary/large instead of Patch
}

// Build compares oldRoot → newRoot and renders patches for every changed file.
// Either root may be cas.Nil (empty tree side). maxBytes <= 0 selects the default.
func Build(r *store.Repo, oldRoot, newRoot cas.Hash, maxBytes int64) ([]FilePatch, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	changes, err := merkle.Diff(r.CAS, oldRoot, newRoot)
	if err != nil {
		return nil, err
	}
	out := make([]FilePatch, 0, len(changes))
	for _, ch := range changes {
		fp := FilePatch{Path: ch.Path, Kind: ch.Kind, OldHash: ch.OldHash, NewHash: ch.NewHash}
		oldText, oldKind, err := loadSide(r, ch.OldHash, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ch.Path, err)
		}
		newText, newKind, err := loadSide(r, ch.NewHash, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ch.Path, err)
		}
		if oldKind == "large" || newKind == "large" {
			fp.Kind = "large"
			fp.Note = "large file differs (content not shown)"
			out = append(out, fp)
			continue
		}
		if oldKind == "binary" || newKind == "binary" {
			fp.Kind = "binary"
			fp.Note = "binary file differs"
			out = append(out, fp)
			continue
		}
		fp.Patch = diff.DiffLines(ch.Path, ch.OldHash, ch.NewHash, oldText, newText)
		out = append(out, fp)
	}
	return out, nil
}

// loadSide reads one blob side as text. Returns ("", "missing") for the empty
// side of added/deleted files, ("", "binary"|"large") for non-text.
func loadSide(r *store.Repo, hexHash string, maxBytes int64) (string, string, error) {
	if hexHash == "" {
		return "", "missing", nil
	}
	h, err := cas.ParseHex(hexHash)
	if err != nil {
		return "", "", err
	}
	raw, err := r.CAS.GetBytes(h, maxBytes+1)
	if err != nil {
		return "", "", err
	}
	if isManifest(raw) {
		return "", "large", nil
	}
	if diff.IsBinary(raw) {
		return "", "binary", nil
	}
	if int64(len(raw)) > maxBytes {
		return "", "large", nil
	}
	return string(raw), "text", nil
}

func isManifest(raw []byte) bool {
	var m chunker.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Version == 1 && len(m.Chunks) > 0 && m.ChunkSize > 0
}
