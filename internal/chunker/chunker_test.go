package chunker

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
)

func TestSmallFileSingleBlob(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "small.txt")
	_ = os.WriteFile(f, []byte("tiny"), 0o644)
	store, _ := cas.New(filepath.Join(dir, "objects"))
	h, m, single, err := Default().ChunkFileToStore(store, f)
	if err != nil {
		t.Fatal(err)
	}
	if !single || m != nil {
		t.Fatal("small file should be single blob")
	}
	if !store.Exists(h) {
		t.Fatal("blob missing")
	}
}

func TestLargeFileManifest(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.bin")
	big := make([]byte, 200*1024)
	for i := range big {
		big[i] = byte(i * 7)
	}
	_ = os.WriteFile(f, big, 0o644)
	store, _ := cas.New(filepath.Join(dir, "objects"))
	mh, m, single, err := Default().ChunkFileToStore(store, f)
	if err != nil {
		t.Fatal(err)
	}
	if single || m == nil {
		t.Fatal("large file should produce manifest")
	}
	if len(m.Chunks) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(m.Chunks))
	}
	loaded, err := LoadManifest(store, mh)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Reassemble(store, loaded, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), big) {
		t.Fatal("reassemble mismatch")
	}
}
