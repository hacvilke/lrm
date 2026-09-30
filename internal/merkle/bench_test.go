package merkle

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
)

// benchTree writes n files of size bytes into a temp dir and returns the
// path, so the benchmarks measure the real walk + hash + store path.
func benchTree(b *testing.B, n, size int) string {
	b.Helper()
	dir := b.TempDir()
	blob := make([]byte, size)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	for i := 0; i < n; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("pkg%02d", i%16))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("file%03d.txt", i)), blob, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

func BenchmarkBuildTree_1kFiles_4KiB(b *testing.B) {
	dir := benchTree(b, 1000, 4096)
	storeDir := b.TempDir()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		store, err := cas.New(storeDir)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := BuildTree(store, dir, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildTree_1kFiles_64KiB(b *testing.B) {
	dir := benchTree(b, 1000, 64*1024)
	storeDir := b.TempDir()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		store, err := cas.New(storeDir)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := BuildTree(store, dir, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

// No-change rebuilds are the daemon's steady state (~2s watch loop): the
// CAS dedupe path must make them nearly free.
func BenchmarkBuildTree_WarmRebuild(b *testing.B) {
	dir := benchTree(b, 500, 8*1024)
	storeDir := b.TempDir()
	store, err := cas.New(storeDir)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := BuildTree(store, dir, DefaultOptions()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := BuildTree(store, dir, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFlatten(b *testing.B) {
	dir := benchTree(b, 1000, 1024)
	store, err := cas.New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	root, err := BuildTree(store, dir, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := make(map[string]string, 1024)
		if err := Flatten(store, root, "", out); err != nil {
			b.Fatal(err)
		}
	}
}
