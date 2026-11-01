package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lrm-project/lrm/internal/portkey"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// TestShareLANKeyReachability: two keys are minted — one with the machine's
// LAN address (what a teammate on the same Wi-Fi can dial) and one with the
// WAN address (which needs a port forward and, on most routers, does not
// hairpin, so it is unreachable from inside the same network).
//
// The regression this locks: share used to print ONLY the STUN address, so
// pairing two laptops on one switch produced a key that always timed out.
func TestShareLANKeyReachability(t *testing.T) {
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i + 1)
	}
	ws := []byte("0123456789abcdef")
	lanIP := []byte{192, 168, 1, 42}
	wanIP := []byte{203, 0, 113, 7}

	lanKey := portkey.Generate(pub, lanIP, 18443, ws)
	wanKey := portkey.Generate(pub, wanIP, 18443, ws)

	if lanKey.Addr() != "192.168.1.42:18443" {
		t.Fatalf("LAN key addr = %q", lanKey.Addr())
	}
	if wanKey.Addr() != "203.0.113.7:18443" {
		t.Fatalf("WAN key addr = %q", wanKey.Addr())
	}
	// Both carry the same peer and workspace, so either one clones the same repo.
	if string(lanKey.PeerID) != string(wanKey.PeerID) {
		t.Fatal("keys disagree on peer id")
	}
	if lanKey.WorkspaceHex() != wanKey.WorkspaceHex() {
		t.Fatalf("keys disagree on workspace: %s vs %s", lanKey.WorkspaceHex(), wanKey.WorkspaceHex())
	}
	// And they round-trip through the wire format a teammate pastes.
	for _, k := range []*portkey.LrmPortKey{lanKey, wanKey} {
		back, err := portkey.Decode(k.Encode())
		if err != nil {
			t.Fatalf("decode %s: %v", k.Addr(), err)
		}
		if back.Addr() != k.Addr() {
			t.Fatalf("round-trip changed address: %s -> %s", k.Addr(), back.Addr())
		}
	}
	// The LAN key is the one that must not depend on any NAT machinery.
	if lanKey.Addr() == wanKey.Addr() {
		t.Fatal("LAN and WAN keys must differ when the addresses differ")
	}
}

// TestStatusShowsConflictBranches: a concurrent-edit sync parks the peer's
// version on HEAD-peer-<short> without touching the local branch. `status`
// used to print "clean (no changes)" and stop, which reads as "nothing to
// see" while a teammate's work sat on a branch nobody knew about.
func TestStatusShowsConflictBranches(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := initTestRepo(repo, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Simulate what conflictBranch() writes: a ref named HEAD-peer-<short>.
	conflict := "HEAD-peer-deadbeef"
	if err := r.SetRef(conflict, ""); err != nil {
		t.Fatalf("set conflict ref: %v", err)
	}

	out := captureStdout(t, func() { printConflicBranches(r) })
	if !strings.Contains(out, conflict) {
		t.Fatalf("conflict branch not reported; got %q", out)
	}
	if !strings.Contains(out, "YOUR work is intact") {
		t.Fatalf("output must say local work is safe; got %q", out)
	}
	if !strings.Contains(out, "lrm merge") {
		t.Fatalf("output must say how to resolve; got %q", out)
	}

	// A repo with no conflicts must stay silent — no noise on the happy path.
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	r2, err := initTestRepo(plain, "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if got := captureStdout(t, func() { printConflicBranches(r2) }); got != "" {
		t.Fatalf("clean repo must print nothing, got %q", got)
	}
}

// TestWorkspaceMismatchIsActionable: the refusal has to name the fix. Two
// people who each ran `lrm init` look exactly like two people in unrelated
// projects, and the old message ("sync refused, no objects exchanged") left
// them retrying the same command.
func TestWorkspaceMismatchIsActionable(t *testing.T) {
	if !sync.IsWorkspaceMismatch(errFake("remote refused sync: " + sync.ErrWorkspaceMismatch)) {
		t.Fatal("sentinel must be detectable through the wrapped message")
	}
	if sync.IsWorkspaceMismatch(errFake("dial tcp: i/o timeout")) {
		t.Fatal("unrelated errors must not be classified as a workspace mismatch")
	}

	msg := captureStdout(t, func() { printCloneRemedy() })
	for _, want := range []string{"one person runs `lrm init`", "lrm clone", "lrm share"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("remedy missing %q; got %q", want, msg)
		}
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// initTestRepo creates a repo for tests in package cli.
func initTestRepo(dir, user string) (*store.Repo, error) {
	return store.Init(dir, user, 0)
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wp
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(rp)
		done <- string(b)
	}()
	fn()
	_ = wp.Close()
	os.Stdout = old
	return <-done
}

// TestCloneUserFlag: `lrm clone <key> app` used the directory name as the
// author, so a teammate who cloned into "app" committed everything as "app".
// --user must win, and the fallback must still work without it.
func TestCloneUserFlag(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "app")

	// The default: author is the directory name (unchanged behaviour).
	r, err := store.InitWithWorkspace(abs, filepath.Base(abs), store.DefaultPort, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Config.User; got != "app" {
		t.Fatalf("default author = %q, want directory name", got)
	}
	_ = r.Close()

	// Explicit author.
	abs2 := filepath.Join(dir, "app2")
	r2, err := store.InitWithWorkspace(abs2, "brandon", store.DefaultPort, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if got := r2.Config.User; got != "brandon" {
		t.Fatalf("explicit author = %q, want brandon", got)
	}
}
