// Package cas implements Immutable Content-Addressed Storage (CAS).
//
// Every object (blob, tree, commit, chunk) is named by its SHA-256 hash.
// Objects are stored under <objects>/ab/cdef... (Git-style fanout).
//
// ANTI-PATTERN GUARD: large objects are NEVER fully buffered in memory.
// PutReader streams from an io.Reader to a temp file while hashing, then
// atomically renames into place. Get returns an io.ReadCloser stream.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Hash is a SHA-256 content address.
type Hash = [32]byte

// Nil is the zero hash (means "no object").
var Nil Hash

// Store is a content-addressed object store rooted at dir.
type Store struct {
	dir string
}

// New creates (and ensures) a store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

// Path returns the filesystem path for a hash.
func (s *Store) Path(h Hash) string {
	hexStr := hex.EncodeToString(h[:])
	return filepath.Join(s.dir, hexStr[:2], hexStr[2:])
}

// Exists reports whether an object is present.
func (s *Store) Exists(h Hash) bool {
	_, err := os.Stat(s.Path(h))
	return err == nil
}

// PutReader streams r into the store, returning its hash and byte count.
//
// The stream is written to a temp file in <dir>/tmp while a SHA-256 is
// computed incrementally (32KB copy buffer, bounded memory). On success the
// temp file is atomically renamed to its content address.
func (s *Store) PutReader(r io.Reader) (Hash, int64, error) {
	if err := os.MkdirAll(filepath.Join(s.dir, "tmp"), 0o755); err != nil {
		return Nil, 0, err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "obj-*")
	if err != nil {
		return Nil, 0, err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on failure; on success the file is renamed away.
	defer func() { _ = os.Remove(tmpName) }()

	h := sha256.New()
	n, err := io.CopyBuffer(struct {
		io.Writer
	}{io.MultiWriter(tmp, h)}, r, make([]byte, 32*1024))
	if err != nil {
		_ = tmp.Close()
		return Nil, 0, fmt.Errorf("stream object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Nil, 0, err
	}
	var sum Hash
	copy(sum[:], h.Sum(nil))

	dst := s.Path(sum)
	if _, err := os.Stat(dst); err == nil {
		return sum, n, nil // already stored; temp removed by defer
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Nil, 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Another writer may have won the race; treat existing as success.
		if _, statErr := os.Stat(dst); statErr == nil {
			return sum, n, nil
		}
		return Nil, 0, fmt.Errorf("commit object: %w", err)
	}
	return sum, n, nil
}

// PutBytes stores a small byte slice (convenience wrapper).
func (s *Store) PutBytes(b []byte) (Hash, error) {
	h, _, err := s.PutReader(bytesReader(b))
	return h, err
}

// PutFile streams a filesystem file into the store.
func (s *Store) PutFile(path string) (Hash, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return Nil, 0, err
	}
	defer f.Close()
	return s.PutReader(f)
}

// Get opens an object as a read stream. Caller must Close.
func (s *Store) Get(h Hash) (io.ReadCloser, error) {
	f, err := os.Open(s.Path(h))
	if err != nil {
		return nil, fmt.Errorf("open object %s: %w", Hex(h), err)
	}
	return f, nil
}

// GetBytes reads a whole object (only for known-small objects such as
// trees, commits, manifests). Size is capped to guard against abuse.
func (s *Store) GetBytes(h Hash, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		maxSize = 64 << 20 // 64 MiB default cap
	}
	rc, err := s.Get(h)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSize+1))
}

// Stat returns the object size.
func (s *Store) Stat(h Hash) (int64, error) {
	fi, err := os.Stat(s.Path(h))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Delete removes an object (used by GC / tests).
func (s *Store) Delete(h Hash) error {
	return os.Remove(s.Path(h))
}

// Hex renders a hash as hex.
func Hex(h Hash) string { return hex.EncodeToString(h[:]) }

// Short renders the first 8 hex chars.
func Short(h Hash) string { return hex.EncodeToString(h[:4]) }

// Parse parses a hex hash (accepts full 64-char or unique short prefix by
// scanning the fanout dir — short-prefix resolution).
func (s *Store) Parse(shex string) (Hash, error) {
	var out Hash
	if len(shex) == 64 {
		b, err := hex.DecodeString(shex)
		if err != nil {
			return out, err
		}
		copy(out[:], b)
		return out, nil
	}
	// Short prefix: resolve via fanout scan.
	if len(shex) < 4 || len(shex) > 64 {
		return out, fmt.Errorf("invalid hash prefix %q", shex)
	}
	prefix := shex[:2]
	rest := shex[2:]
	entries, err := os.ReadDir(filepath.Join(s.dir, prefix))
	if err != nil {
		return out, fmt.Errorf("unknown object prefix %q", shex)
	}
	matches := 0
	for _, e := range entries {
		if len(e.Name()) >= len(rest) && e.Name()[:len(rest)] == rest {
			full := prefix + e.Name()
			b, err := hex.DecodeString(full)
			if err != nil || len(b) != 32 {
				continue
			}
			copy(out[:], b)
			matches++
		}
	}
	if matches == 0 {
		return out, fmt.Errorf("unknown object %q", shex)
	}
	if matches > 1 {
		return out, fmt.Errorf("ambiguous prefix %q (%d matches)", shex, matches)
	}
	return out, nil
}

// ParseHex parses a full 64-char hex string without store access.
func ParseHex(shex string) (Hash, error) {
	var out Hash
	b, err := hex.DecodeString(shex)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("hash must be 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// bytesReader avoids importing bytes in hot path callers (tiny adapter).
func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	o int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.o >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.o:])
	r.o += n
	return n, nil
}
