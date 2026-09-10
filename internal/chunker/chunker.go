// Package chunker splits large files into uniform blocks for P2P
// distribution (BitTorrent/IPFS model).
//
// Default target is 64 KiB per chunk (spec range 4KB–1MB). Chunking is
// strictly streaming: only one chunk is held in memory at a time.
package chunker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lrm-project/lrm/internal/cas"
)

const (
	// MinChunk is the smallest allowed chunk (4 KiB).
	MinChunk = 4 * 1024
	// DefaultChunk is the default chunk size (64 KiB).
	DefaultChunk = 64 * 1024
	// MaxChunk is the largest allowed chunk (1 MiB).
	MaxChunk = 1 * 1024 * 1024
)

// Manifest describes a chunked large file. The manifest itself is stored
// as a CAS object; file trees reference the manifest hash with the
// "chunked" flag so peers can fetch chunks in parallel from many peers.
type Manifest struct {
	Version   int      `json:"version"`
	ChunkSize int      `json:"chunk_size"`
	TotalSize int64    `json:"total_size"`
	Chunks    []string `json:"chunks"` // hex hashes
	FileHash  string   `json:"file_hash"`
}

// Chunker splits streams into fixed-size blocks.
type Chunker struct {
	Size int
}

// New returns a Chunker with a validated size (clamped to [Min, Max]).
func New(size int) *Chunker {
	if size < MinChunk {
		size = DefaultChunk
	}
	if size > MaxChunk {
		size = MaxChunk
	}
	return &Chunker{Size: size}
}

// Default returns a Chunker with DefaultChunk size.
func Default() *Chunker { return &Chunker{Size: DefaultChunk} }

// SplitReader streams r, invoking fn for each chunk (index 0..n).
// fn receives a buffer valid only for the call duration.
func (c *Chunker) SplitReader(r io.Reader, fn func(index int, chunk []byte) error) (total int64, err error) {
	buf := make([]byte, c.Size)
	idx := 0
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			// Copy because fn may retain; buffer is reused.
			cp := make([]byte, n)
			copy(cp, buf[:n])
			if err := fn(idx, cp); err != nil {
				return total, err
			}
			total += int64(n)
			idx++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// ChunkFileToStore streams a file into the CAS as individual chunk objects
// plus a manifest object. Returns (manifestHash, manifest, error).
//
// Small files (<= ChunkSize) are stored as a single blob WITHOUT a
// manifest; in that case manifestHash is the blob hash and manifest is nil
// with singleBlob=true.
func (c *Chunker) ChunkFileToStore(store *cas.Store, path string) (manifestHash cas.Hash, manifest *Manifest, singleBlob bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return cas.Nil, nil, false, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return cas.Nil, nil, false, err
	}

	// Fast path: small file → single blob object (still streamed).
	if fi.Size() <= int64(c.Size) {
		h, _, err := store.PutReader(f)
		if err != nil {
			return cas.Nil, nil, false, err
		}
		return h, nil, true, nil
	}

	fileH := sha256.New()
	var chunks []string
	var total int64
	_, err = c.SplitReader(io.TeeReader(f, fileH), func(_ int, chunk []byte) error {
		h, _, err := store.PutReader(bytesOf(chunk))
		if err != nil {
			return err
		}
		chunks = append(chunks, cas.Hex(h))
		total += int64(len(chunk))
		return nil
	})
	if err != nil {
		return cas.Nil, nil, false, fmt.Errorf("chunk file: %w", err)
	}
	m := &Manifest{
		Version:   1,
		ChunkSize: c.Size,
		TotalSize: total,
		Chunks:    chunks,
		FileHash:  fmt.Sprintf("%x", fileH.Sum(nil)),
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return cas.Nil, nil, false, err
	}
	mh, err := store.PutBytes(raw)
	if err != nil {
		return cas.Nil, nil, false, err
	}
	return mh, m, false, nil
}

// Reassemble streams the chunks of a manifest from the store into w.
// Chunks are fetched sequentially here; the P2P sync engine fetches them
// in parallel and then calls Reassemble with a local store.
func Reassemble(store *cas.Store, m *Manifest, w io.Writer) error {
	for _, ch := range m.Chunks {
		h, err := cas.ParseHex(ch)
		if err != nil {
			return err
		}
		rc, err := store.Get(h)
		if err != nil {
			return fmt.Errorf("missing chunk %s: %w", ch, err)
		}
		_, cpErr := io.Copy(w, rc)
		rc.Close()
		if cpErr != nil {
			return cpErr
		}
	}
	return nil
}

// LoadManifest loads a manifest object by hash.
func LoadManifest(store *cas.Store, h cas.Hash) (*Manifest, error) {
	raw, err := store.GetBytes(h, 16<<20)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func bytesOf(b []byte) io.Reader { return &br{b: b} }

type br struct {
	b []byte
	o int
}

func (r *br) Read(p []byte) (int, error) {
	if r.o >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.o:])
	r.o += n
	return n, nil
}
