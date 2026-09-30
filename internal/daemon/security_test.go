package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lrm-project/lrm/internal/dash"
	"github.com/lrm-project/lrm/internal/store"
)

// startDaemon brings up a daemon on an ephemeral port with router and
// discovery work disabled, and returns a stop function.
func startDaemon(t *testing.T, repo *store.Repo) (addr string, stop func()) {
	t.Helper()
	d := New(repo, Config{Port: 0, WatchEvery: time.Hour, PeerRefresh: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for d.ListenAddr() == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("daemon did not start listening")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return d.ListenAddr(), func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	}
}

// The control socket answers presence queries and can nudge syncs; other
// local accounts must not be able to reach it.
func TestControlSocketIsOwnerOnly(t *testing.T) {
	repo, err := store.Init(filepath.Join(t.TempDir(), "alice"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, stop := startDaemon(t, repo)
	defer stop()

	sock := filepath.Join(repo.LrmDir, "daemon.sock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		fi, err := os.Stat(sock)
		if err == nil {
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Fatalf("control socket mode = %o, want 600", perm)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket never appeared: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The dashboard reads .lrm/status.json; the daemon must publish it, with
// the local facts a user needs, without ever writing it world-readable.
func TestStatusIsPublished(t *testing.T) {
	repo, err := store.Init(filepath.Join(t.TempDir(), "alice"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, stop := startDaemon(t, repo)
	defer stop()

	path := filepath.Join(repo.LrmDir, StatusFileName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status.json was never published")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("status.json mode = %o, want 600", perm)
	}
	st, err := dash.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.User != "alice" || st.PeerID == "" {
		t.Fatalf("status missing identity: %+v", st)
	}
	if st.Workspace == "" {
		t.Fatal("status missing workspace id")
	}
	// Empty lists must serialize as lists, not null: every consumer
	// (dashboard, scripts) should be able to iterate without special cases.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"peers": []`, `"syncs": []`, `"rejects": []`, `"events": []`} {
		if !contains(string(raw), field) {
			t.Fatalf("status field %s missing/null in: %s", field, raw[:min(len(raw), 400)])
		}
	}
}

// A refused sync (workspace mismatch) must leave a visible record — the
// dashboard's whole point is showing what was turned away.
func TestRefusedSyncIsRecorded(t *testing.T) {
	repo, err := store.Init(filepath.Join(t.TempDir(), "alice"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	d := New(repo, Config{Port: 0, WatchEvery: time.Hour, PeerRefresh: time.Hour})
	d.noteSync(nil, errString("workspace mismatch — sync refused"), false, "deadbeef", "", nil)
	st, err := dash.Load(filepath.Join(repo.LrmDir, StatusFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Rejects) != 1 || !contains(st.Rejects[0].Reason, "refused") {
		t.Fatalf("refusal not recorded: %+v", st.Rejects)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
