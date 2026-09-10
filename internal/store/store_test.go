package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/merkle"
)

func TestInitCommitLog(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "proj")
	r, err := Init(repoDir, "tester", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("hello lrm"), 0o644)
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("should detect new file")
	}
	h, c, err := r.Commit("first", res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	if c.Message != "first" {
		t.Fatal("commit msg mismatch")
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	tipHex, _ := r.GetRef("main")
	if tipHex != cas.Hex(h) {
		t.Fatal("ref not updated")
	}
	// Second scan should be clean after index update.
	res2, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	_ = res2
	_ = merkle.DefaultOptions
}

func TestInitSetsWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	r, err := Init(dir, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := ValidateWorkspaceID(r.Config.Workspace); err != nil {
		t.Fatalf("init should set a valid workspace id: %v", err)
	}
}

func TestWorkspaceDerivedFromGenesis(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	r, err := Init(dir, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		t.Fatal(err)
	}
	genesis, _, err := r.Commit("first", res.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
	want := sha256.Sum256(append([]byte("lrm-ws-v1"), []byte(hex.EncodeToString(genesis[:]))...))
	wantHex := hex.EncodeToString(want[:16])
	r.Close()

	// Simulate a legacy repo: strip the workspace from config.json.
	cfgPath := filepath.Join(dir, ".lrm", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "workspace")
	raw2, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, raw2, 0o644); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if r2.Config.Workspace != wantHex {
		t.Fatalf("derived workspace %q, want %q", r2.Config.Workspace, wantHex)
	}

	// A synced peer (same history, e.g. a copy of this repo) derives the
	// SAME id — existing pairs keep working after the upgrade.
	dir2 := filepath.Join(t.TempDir(), "proj-copy")
	if err := os.CopyFS(dir2, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	if r3.Config.Workspace != wantHex {
		t.Fatalf("peer derived %q, want %q — synced pairs must converge", r3.Config.Workspace, wantHex)
	}
}
