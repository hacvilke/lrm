package gitcompat

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// Archive writes ref's tree to outPath in tar|tar.gz|zip format.
// Files stream from CAS (chunk-aware) with bounded memory; entry mtimes
// come from the commit timestamp so repeated exports are stable.
func Archive(r *store.Repo, commit cas.Hash, format, outPath string) error {
	switch format {
	case "tar", "tar.gz", "tgz", "zip":
	default:
		return fmt.Errorf("unknown format %q (want tar|tar.gz|zip)", format)
	}
	flat, err := TreeAtCommit(r, commit)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	c, err := r.DAG.Get(commit)
	if err != nil {
		return err
	}
	mtime := time.Unix(0, c.Timestamp)
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if format == "zip" {
		return writeZip(r, flat, paths, mtime, f)
	}
	if format == "tar" {
		tw := tar.NewWriter(f)
		for _, p := range paths {
			if err := writeTarEntry(r, tw, flat, p, mtime); err != nil {
				_ = tw.Close()
				return fmt.Errorf("archive %s: %w", p, err)
			}
		}
		return tw.Close()
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		if err := writeTarEntry(r, tw, flat, p, mtime); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("archive %s: %w", p, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

func writeTarEntry(r *store.Repo, tw *tar.Writer, flat map[string]string, p string, mtime time.Time) error {
	h, err := cas.ParseHex(flat[p])
	if err != nil {
		return err
	}
	size, err := objectSize(r, h)
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: p, Mode: 0o644, Size: size, ModTime: mtime, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	return WriteObjectTo(r.CAS, h, tw)
}

func writeZip(r *store.Repo, flat map[string]string, paths []string, mtime time.Time, f *os.File) error {
	zw := zip.NewWriter(f)
	defer zw.Close()
	for _, p := range paths {
		h, err := cas.ParseHex(flat[p])
		if err != nil {
			return fmt.Errorf("archive %s: %w", p, err)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name: p, Method: zip.Deflate, Modified: mtime,
		})
		if err != nil {
			return err
		}
		if err := WriteObjectTo(r.CAS, h, w); err != nil {
			return fmt.Errorf("archive %s: %w", p, err)
		}
	}
	return zw.Close()
}

// objectSize returns true content bytes (manifest-aware).
func objectSize(r *store.Repo, h cas.Hash) (int64, error) {
	raw, err := r.CAS.GetBytes(h, 4<<20)
	if err != nil {
		return r.CAS.Stat(h)
	}
	var m struct {
		Version   int      `json:"version"`
		ChunkSize int      `json:"chunk_size"`
		TotalSize int64    `json:"total_size"`
		Chunks    []string `json:"chunks"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Version == 1 && len(m.Chunks) > 0 && m.ChunkSize > 0 {
		return m.TotalSize, nil
	}
	return int64(len(raw)), nil
}

// GuessFormat maps -o extensions to archive formats ("" if unknown).
func GuessFormat(outPath string) string {
	lower := strings.ToLower(outPath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	}
	return ""
}
