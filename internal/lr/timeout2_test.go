package lr

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTimeoutBlockingBuiltin locks the deadline semantics for blocking
// builtins: a single long sleep/http/dial must never outlive the run
// timeout, even as the LAST statement (statement-level ticks never fire).
func TestTimeoutBlockingBuiltin(t *testing.T) {
	dir := t.TempDir()

	// sleep as the last statement: run must FAIL with timeout, fast.
	script := filepath.Join(dir, "sleep.lr")
	_ = os.WriteFile(script, []byte(`sleep_ms(4000);`), 0o644)
	start := time.Now()
	res := RunFile(script, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 500 * time.Millisecond})
	if res.Passed {
		t.Fatalf("sleep past deadline must fail: %+v", res)
	}
	if !strings.Contains(res.Error, "timeout") {
		t.Fatalf("want timeout error, got %q", res.Error)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("run took %s — sleep was not capped to the deadline", time.Since(start))
	}

	// A sleep that FITS inside the deadline still works.
	script2 := filepath.Join(dir, "nap.lr")
	_ = os.WriteFile(script2, []byte(`sleep_ms(50); print("napped");`), 0o644)
	res2 := RunFile(script2, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 5 * time.Second})
	if !res2.Passed {
		t.Fatalf("short sleep must pass: %+v err=%s", res2, res2.Error)
	}

	// tcp_connect is capped too: 60s requested, 400ms deadline, dead port.
	// The trailing sleep guarantees we cross the deadline (a capped dial
	// alone can finish a hair before it).
	script3 := filepath.Join(dir, "tcp.lr")
	_ = os.WriteFile(script3, []byte(`tcp_connect("10.255.255.1", 65001, 60000); sleep_ms(300);`), 0o644)
	start = time.Now()
	res3 := RunFile(script3, Options{WorkDir: dir, Out: &bytes.Buffer{}, Timeout: 400 * time.Millisecond})
	if res3.Passed {
		t.Fatalf("tcp_connect past deadline must fail: %+v", res3)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("run took %s — dial was not capped to the deadline", time.Since(start))
	}
}

// TestLRMSyncDialFailureHonest locks the lrm_sync contract: when every
// target fails to dial, the builtin must return {ok:false} — never a
// silent ok:true with zero objects moved.
func TestLRMSyncDialFailureHonest(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, repo) // lrm_sync needs a repo; the dial failure must dominate
	script := filepath.Join(dir, "sync.lr")
	// Port 1 on loopback: nothing listens there.
	_ = os.WriteFile(script, []byte(`
let s = lrm_sync("127.0.0.1:1");
assert(s.ok == false, "all-dials-failed sync must not be ok");
assert(contains(str(s.error), "sync failed"), "error carries the reason");
`), 0o644)
	res := RunFile(script, Options{WorkDir: repo, Out: &bytes.Buffer{}, Timeout: 30 * time.Second})
	if !res.Passed {
		t.Fatalf("res=%+v err=%s", res, res.Error)
	}
}
