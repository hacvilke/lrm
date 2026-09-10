package sync

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/store"
)

// blk returns n bytes of a repeated marker byte — one distinctive chunk.
func blk(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }

// bigCommit writes big.bin (contents) to r and commits it.
func bigCommit(t *testing.T, r *store.Repo, contents []byte, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Root, "big.bin"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit(msg, res.RootHash); err != nil {
		t.Fatal(err)
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
}

// manifestOf returns (manifestHash, chunkHashes) of big.bin at r's HEAD.
func manifestOf(t *testing.T, r *store.Repo) (string, []string) {
	t.Helper()
	tip, _, err := r.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.DAG.Get(tip)
	if err != nil {
		t.Fatal(err)
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := merkle.LoadTree(r.CAS, th)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tree.Entries {
		if e.Name != "big.bin" {
			continue
		}
		if !e.Chunked {
			t.Fatal("big.bin not chunked — test premise broken")
		}
		mh, err := cas.ParseHex(e.Hash)
		if err != nil {
			t.Fatal(err)
		}
		m, err := chunker.LoadManifest(r.CAS, mh)
		if err != nil {
			t.Fatal(err)
		}
		return e.Hash, m.Chunks
	}
	t.Fatal("big.bin not found in HEAD tree")
	return "", nil
}

// syncPair runs one bob(dialer) <- alice(responder) sync round and
// returns bob's result.
func syncPair(t *testing.T, a, b *store.Repo) *SyncResult {
	t.Helper()
	pa, pb := net.Pipe()
	sessA := mux.NewSession(pa, false)
	sessB := mux.NewSession(pb, true)
	defer sessA.Close()
	defer sessB.Close()
	var wg stdsync.WaitGroup
	wg.Add(2)
	var res *SyncResult
	var err error
	go func() {
		defer wg.Done()
		_, _ = New(a).SyncWithSession(sessA, false, "")
	}()
	go func() {
		defer wg.Done()
		res, err = New(b).SyncWithSession(sessB, true, "")
	}()
	wg.Wait()
	if err != nil {
		t.Fatalf("initiator: %v", err)
	}
	return res
}

// regionBytes returns the i-th 64 KiB region of a chunkable file.
func regionBytes(file []byte, i int) []byte {
	start := i * (64 << 10)
	end := start + (64 << 10)
	if end > len(file) {
		end = len(file)
	}
	return file[start:end]
}

// TestSyncResumeChunkGranular proves transfer resume is CHUNK-granular:
// a peer that already holds some of a chunked file's 64 KiB blocks (an
// interrupted transfer) only fetches the missing chunks — never the
// whole file again.
func TestSyncResumeChunkGranular(t *testing.T) {
	a := mkRepo(t, "alice")
	// 300 KiB = four full 64 KiB chunks + one 44 KiB tail.
	file := bytes.Join([][]byte{
		blk('a', 64<<10), blk('b', 64<<10), blk('c', 64<<10),
		blk('d', 64<<10), blk('e', 44<<10),
	}, nil)
	bigCommit(t, a, file, "big file")

	_, chunks := manifestOf(t, a)
	if len(chunks) != 5 {
		t.Fatalf("want 5 chunks, got %d", len(chunks))
	}

	// Bob: an interrupted transfer left him with chunks 0 and 2 only
	// (nothing else — no commit, no tree, no manifest).
	b := mkRepo(t, "bob")
	for _, i := range []int{0, 2} {
		h, err := b.CAS.PutBytes(regionBytes(file, i))
		if err != nil {
			t.Fatal(err)
		}
		if cas.Hex(h) != chunks[i] {
			t.Fatalf("chunk %d hash mismatch: %s vs manifest %s", i, cas.Short(h), chunks[i])
		}
	}

	res := syncPair(t, a, b)

	// Integrity: the file materialized byte-identical.
	got, err := os.ReadFile(filepath.Join(b.Root, "big.bin"))
	if err != nil {
		t.Fatalf("bob missing big.bin: %v", err)
	}
	if !bytes.Equal(got, file) {
		t.Fatalf("big.bin corrupted: %d bytes vs %d", len(got), len(file))
	}

	// Resume granularity: fetched objects must be exactly the missing
	// set — commit + root tree + manifest + chunks 1, 3, 4 = 6 objects.
	// Chunks 0 and 2 (already in bob's CAS) must NOT be re-fetched.
	if res.Fetched != 6 {
		t.Fatalf("fetched %d objects, want 6 (resume must skip held chunks 0 and 2)", res.Fetched)
	}
	for i, ch := range chunks {
		h, err := cas.ParseHex(ch)
		if err != nil {
			t.Fatal(err)
		}
		if !b.CAS.Exists(h) {
			t.Fatalf("chunk %d missing after sync", i)
		}
	}
}

// TestSyncResumeSharedPrefix extends the scenario: after a FULL sync, an
// appended file re-uses all full 64 KiB prefix chunks; the incremental
// sync fetches only the changed tail chunks.
func TestSyncResumeSharedPrefix(t *testing.T) {
	a := mkRepo(t, "alice")
	v1 := bytes.Join([][]byte{
		blk('a', 64<<10), blk('b', 64<<10), blk('c', 64<<10),
		blk('d', 64<<10), blk('e', 44<<10),
	}, nil)
	bigCommit(t, a, v1, "v1")
	_, chunksV1 := manifestOf(t, a)
	if len(chunksV1) != 5 {
		t.Fatalf("v1 want 5 chunks, got %d", len(chunksV1))
	}

	b := mkRepo(t, "bob")
	if res := syncPair(t, a, b); res.Fetched == 0 {
		t.Fatal("first sync should fetch objects")
	}

	// v2: extend the tail to a full 64 KiB block and add one more chunk.
	// Chunks 0-3 (full 64 KiB prefixes) are byte-identical and MUST be
	// re-used; chunk 4 changes (44 KiB -> 64 KiB); chunk 5 is new.
	v2 := bytes.Join([][]byte{
		blk('a', 64<<10), blk('b', 64<<10), blk('c', 64<<10),
		blk('d', 64<<10), blk('e', 64<<10), blk('f', 64<<10),
	}, nil)
	bigCommit(t, a, v2, "v2")
	_, chunksV2 := manifestOf(t, a)
	if len(chunksV2) != 6 {
		t.Fatalf("v2 want 6 chunks, got %d", len(chunksV2))
	}
	for i := 0; i < 4; i++ {
		if chunksV2[i] != chunksV1[i] {
			t.Fatalf("chunk %d not shared between v1 and v2 — test premise broken", i)
		}
	}

	incr := syncPair(t, a, b)
	// Incremental fetch: commit + tree + manifest + 2 tail chunks = 5.
	// The 4 shared prefix chunks must be skipped.
	if incr.Fetched != 5 {
		t.Fatalf("incremental sync fetched %d objects, want 5 (4 shared prefix chunks must be skipped)", incr.Fetched)
	}
	got, err := os.ReadFile(filepath.Join(b.Root, "big.bin"))
	if err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("big.bin after incremental sync: err=%v len=%d", err, len(got))
	}
}
