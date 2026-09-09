package gitcompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// MaxReadBytes caps single-file reads for blame/grep (streaming stays in
// the sync engine; these are inspection tools).
const MaxReadBytes = 2 << 20

// ReadObjectBytes returns the full content of a blob or chunked file,
// bounded by maxBytes (>0). Manifest detection mirrors sync/checkout.
func ReadObjectBytes(cs *cas.Store, h cas.Hash, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxReadBytes
	}
	raw, err := cs.GetBytes(h, 4<<20)
	if err != nil {
		// Object bigger than the peek window: size-gate via Stat.
		if sz, serr := cs.Stat(h); serr == nil && sz > maxBytes {
			return nil, fmt.Errorf("object too large (%d bytes > %d cap)", sz, maxBytes)
		}
		return nil, err
	}
	var m chunker.Manifest
	if json.Unmarshal(raw, &m) == nil && m.Version == 1 && len(m.Chunks) > 0 && m.ChunkSize > 0 {
		if m.TotalSize > maxBytes {
			return nil, fmt.Errorf("file too large (%d bytes > %d cap)", m.TotalSize, maxBytes)
		}
		var buf bytes.Buffer
		if err := chunker.Reassemble(cs, &m, &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("object too large (%d bytes > %d cap)", len(raw), maxBytes)
	}
	return raw, nil
}

// FileAtCommit reads path (slash-separated, repo-relative) at a commit.
func FileAtCommit(r *store.Repo, commit cas.Hash, path string) ([]byte, error) {
	c, err := r.DAG.Get(commit)
	if err != nil {
		return nil, err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return nil, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return nil, err
	}
	hx, ok := flat[path]
	if !ok {
		return nil, fmt.Errorf("no such file %q at %s", path, cas.Short(commit))
	}
	h, err := cas.ParseHex(hx)
	if err != nil {
		return nil, err
	}
	return ReadObjectBytes(r.CAS, h, MaxReadBytes)
}

// TreeAtCommit flattens a commit's tree to path → hashHex.
func TreeAtCommit(r *store.Repo, commit cas.Hash) (map[string]string, error) {
	c, err := r.DAG.Get(commit)
	if err != nil {
		return nil, err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return nil, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(r.CAS, th, "", flat); err != nil {
		return nil, err
	}
	return flat, nil
}

// WriteObjectTo streams a blob or chunked file to w (no size cap —
// the archive path stays streaming no matter how large the file).
func WriteObjectTo(cs *cas.Store, h cas.Hash, w io.Writer) error {
	raw, err := cs.GetBytes(h, 4<<20)
	if err != nil {
		// Too big for the peek window to be a manifest: raw stream.
		rc, err := cs.Get(h)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(w, rc)
		return err
	}
	var m chunker.Manifest
	if json.Unmarshal(raw, &m) == nil && m.Version == 1 && len(m.Chunks) > 0 && m.ChunkSize > 0 {
		return chunker.Reassemble(cs, &m, w)
	}
	_, err = w.Write(raw)
	return err
}
