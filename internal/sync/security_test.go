package sync

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/vectorclock"
)

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	r, err := store.Init(filepath.Join(t.TempDir(), "proj"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// putAtAddress writes raw bytes at an explicit CAS address (bypassing the
// content check), which is how a malicious peer files hostile trees.
func putAtAddress(t *testing.T, r *store.Repo, h cas.Hash, raw []byte) {
	t.Helper()
	dst := r.CAS.Path(h)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// hostileTree builds a tree object (addressed correctly, so the CAS copy
// path is happy) whose single entry escapes the repository.
func hostileTree(t *testing.T, r *store.Repo, name string) cas.Hash {
	t.Helper()
	blob, err := r.CAS.PutBytes([]byte("payload written by a hostile peer"))
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":1,"entries":[{"name":"` + name + `","hash":"` + cas.Hex(blob) + `","mode":420,"size":32}]}`)
	th := merkle.TreeAddress(raw)
	putAtAddress(t, r, th, raw)
	return th
}

func commitTo(t *testing.T, r *store.Repo, tree cas.Hash) cas.Hash {
	t.Helper()
	ch, err := dag.New(r.CAS).Put(&dag.Commit{
		Version: 1, Tree: cas.Hex(tree), Author: "evil", PeerHex: strings.Repeat("a", 64),
		Timestamp: time.Now().UnixNano(), Message: "hostile", Clock: vectorclock.Clock{"evil": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// TestCheckoutRefusesHostileTree: a tree from a peer must never write
// outside the repository. The whole checkout is refused, and nothing is
// written — not the hostile file, not the safe ones either (no partial
// application of a hostile tree).
func TestCheckoutRefusesHostileTree(t *testing.T) {
	r := newRepo(t)
	for _, name := range []string{"../../pwned", "..", "/etc/pwned", "a/../../pwned"} {
		th := hostileTree(t, r, name)
		ch := commitTo(t, r, th)
		err := Checkout(r, ch)
		if err == nil {
			t.Fatalf("checkout of hostile tree entry %q succeeded", name)
		}
		if !strings.Contains(err.Error(), "hostile") && !strings.Contains(err.Error(), "refus") {
			t.Fatalf("error should say it refused the tree: %v", err)
		}
		for _, escaped := range []string{
			filepath.Join(r.Root, "../../pwned"),
			filepath.Join(filepath.Dir(r.Root), "pwned"),
			"/etc/pwned",
		} {
			if _, statErr := os.Stat(escaped); statErr == nil {
				t.Fatalf("SECURITY: hostile tree wrote %s", escaped)
			}
		}
	}
}

// TestCheckoutAcceptsHonestTree is the positive control: the gate must not
// break normal syncing.
func TestCheckoutAcceptsHonestTree(t *testing.T) {
	r := newRepo(t)
	flat := map[string]string{}
	b1, _ := r.CAS.PutBytes([]byte("hello\n"))
	b2, _ := r.CAS.PutBytes([]byte("deep\n"))
	flat["a.txt"] = cas.Hex(b1)
	flat["docs/b.txt"] = cas.Hex(b2)
	th, err := BuildTreeFromMap(r.CAS, flat)
	if err != nil {
		t.Fatal(err)
	}
	ch := commitTo(t, r, th)
	if err := Checkout(r, ch); err != nil {
		t.Fatalf("honest checkout refused: %v", err)
	}
	for name, want := range map[string]string{"a.txt": "hello\n", "docs/b.txt": "deep\n"} {
		got, err := os.ReadFile(filepath.Join(r.Root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v — want %q", name, got, err, want)
		}
	}
}

// TestBuildTreeRefusesHostileKeys: flattened maps can come from peer trees,
// and a key like "/x" used to send the builder into infinite recursion.
func TestBuildTreeRefusesHostileKeys(t *testing.T) {
	r := newRepo(t)
	b, _ := r.CAS.PutBytes([]byte("x"))
	done := make(chan error, 1)
	go func() {
		_, err := BuildTreeFromMap(r.CAS, map[string]string{"/etc/passwd": cas.Hex(b)})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hostile key accepted by tree builder")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tree builder hung on a hostile key (infinite recursion)")
	}
	for _, bad := range []string{"..", "a/../b", "a//b", "", "a\\b"} {
		if _, err := BuildTreeFromMap(r.CAS, map[string]string{bad: cas.Hex(b)}); err == nil {
			t.Fatalf("builder accepted hostile key %q", bad)
		}
	}
}

// TestCopyBlobRefusesPoisonedBytes: the byte-vs-address check. A peer picks
// both the bytes and the address they claim to belong to; unverified bytes
// must never be filed at a requested address.
func TestCopyBlobRefusesPoisonedBytes(t *testing.T) {
	r := newRepo(t)
	e := New(r)

	// Bytes that are NOT the object the address claims.
	got, err := r.CAS.PutBytes([]byte("innocent bytes"))
	if err != nil {
		t.Fatal(err)
	}
	want := merkle.TreeAddress([]byte(`{"version":1,"entries":[{"name":"/etc/pwned"}]}`))
	if r.CAS.Exists(want) {
		t.Fatal("precondition: address already occupied")
	}
	err = copyBlobToAddress(e, got, want)
	if err == nil {
		t.Fatal("SECURITY: poisoned bytes filed at a requested address")
	}
	if !strings.Contains(err.Error(), "poisoning") {
		t.Fatalf("error should name the attack: %v", err)
	}
	if r.CAS.Exists(want) {
		t.Fatal("SECURITY: address occupied after refusal")
	}
}

// TestCopyBlobAcceptsRealTrees: the verified copy must still work for the
// objects it exists to move (domain-separated trees).
func TestCopyBlobAcceptsRealTrees(t *testing.T) {
	r := newRepo(t)
	e := New(r)
	raw := []byte(`{"version":1,"entries":[]}`)
	want := merkle.TreeAddress(raw)
	got, err := r.CAS.PutBytes(raw) // stored at its *blob* address
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("precondition: blob and tree addresses collided")
	}
	if err := copyBlobToAddress(e, got, want); err != nil {
		t.Fatalf("verified copy refused a real tree: %v", err)
	}
	if !r.CAS.Exists(want) {
		t.Fatal("tree not filed at its address")
	}
	// And the file it produced is byte-identical to the original.
	rc, err := r.CAS.Get(want)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, len(raw))
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(raw) {
		t.Fatal("copied bytes differ from source")
	}
}
