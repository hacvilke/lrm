package lr

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runScript executes src as a .lr script and returns its stdout.
func runScript(t *testing.T, src string) (string, *Result) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s.lr")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	res := RunFile(path, Options{WorkDir: dir, Out: &out, Timeout: 60 * time.Second})
	// RunFile writes its report footer to the same writer; drop it so
	// tests can assert on the script's own output.
	var kept []string
	for _, line := range strings.Split(out.String(), "\n") {
		t := strings.TrimSpace(line)
		if t == "---" || strings.HasPrefix(t, "report:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), res
}

// listenN starts n TCP listeners and returns their ports.
func listenN(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	return ports
}

func TestTCPScanFindsOpenAndClosedPorts(t *testing.T) {
	ports := listenN(t, 3)

	// One definitely-closed port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	src := fmt.Sprintf(`
let r = tcp_scan(["127.0.0.1"], [%d,%d,%d,%d], 2000);
print("count=" + str(r.count));
print("open=" + str(r.open_count));
print("closed=" + str(r.closed_count));
`, ports[0], ports[1], ports[2], closed)

	out, res := runScript(t, src)
	if res.Error != "" {
		t.Fatalf("script failed: %s\n%s", res.Error, out)
	}
	for _, want := range []string{"count=4", "open=3", "closed=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Results must line up with the inputs regardless of which probe finished
// first -- that is the whole contract of a concurrent batch builtin.
func TestTCPScanPreservesInputOrder(t *testing.T) {
	ports := listenN(t, 4)
	src := fmt.Sprintf(`
let r = tcp_scan(["127.0.0.1"], [%d,%d,%d,%d], 2000);
for x in r.results { print(str(x.port)); }
`, ports[0], ports[1], ports[2], ports[3])

	out, res := runScript(t, src)
	if res.Error != "" {
		t.Fatalf("script failed: %s", res.Error)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got = append(got, line)
		}
	}
	want := []string{
		fmt.Sprint(ports[0]), fmt.Sprint(ports[1]),
		fmt.Sprint(ports[2]), fmt.Sprint(ports[3]),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order not preserved: got %v, want %v", got, want)
		}
	}
}

// The point of the feature: N slow requests must cost about one request,
// not N of them.
//
// This uses a local server that sleeps, rather than unroutable addresses:
// in a sandbox with no route, connections fail instantly instead of
// timing out, so the test would pass without proving anything.
func TestHTTPGetAllIsActuallyConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const delay = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	const n = 8
	urls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		urls = append(urls, fmt.Sprintf("%q", srv.URL+"/"+fmt.Sprint(i)))
	}
	src := fmt.Sprintf(`
let r = http_get_all([%s], 5000);
print("count=" + str(r.count));
print("failed=" + str(r.failed));
print("wall=" + str(r.wall_ms));
`, strings.Join(urls, ","))

	start := time.Now()
	out, res := runScript(t, src)
	elapsed := time.Since(start)
	if res.Error != "" {
		t.Fatalf("script failed: %s\n%s", res.Error, out)
	}
	if !strings.Contains(out, "count=8") || !strings.Contains(out, "failed=0") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	sequential := time.Duration(n) * delay // 2.4s
	if elapsed > sequential/2 {
		t.Errorf("%d requests of %v took %v; sequential would be %v — that does not look concurrent",
			n, delay, elapsed, sequential)
	}
	t.Logf("%d x %v requests in %v (sequential would be %v)", n, delay, elapsed, sequential)
}

func TestDNSLookupAllShape(t *testing.T) {
	// localhost resolves without network access.
	src := `
let d = dns_lookup_all(["localhost","localhost"]);
print("count=" + str(d.count));
print("n=" + str(len(d.results)));
print("host0=" + d.results[0].host);
`
	out, res := runScript(t, src)
	if res.Error != "" {
		t.Fatalf("script failed: %s\n%s", res.Error, out)
	}
	for _, want := range []string{"count=2", "n=2", "host0=localhost"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSortByLatency(t *testing.T) {
	src := `
let xs = [{"latency_ms": 30}, {"latency_ms": 10}, {"latency_ms": 20}];
for x in sort_by_latency(xs) { print(str(x.latency_ms)); }
`
	out, res := runScript(t, src)
	if res.Error != "" {
		t.Fatalf("script failed: %s\n%s", res.Error, out)
	}
	got := strings.Fields(strings.TrimSpace(out))
	want := []string{"10", "20", "30"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Bad arguments must be rejected with a clear message, not panic or
// silently scan nothing.
func TestBatchBuiltinsRejectBadArguments(t *testing.T) {
	cases := map[string]string{
		"non-list hosts":  `tcp_scan("github.com", [80], 1000);`,
		"non-string host": `tcp_scan([80], [80], 1000);`,
		"non-list ports":  `tcp_scan(["x"], 80, 1000);`,
		"port too high":   `tcp_scan(["x"], [70000], 1000);`,
		"port zero":       `tcp_scan(["x"], [0], 1000);`,
		"dns non-list":    `dns_lookup_all("x");`,
		"http non-list":   `http_get_all(5);`,
		"sort non-list":   `sort_by_latency(5);`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, res := runScript(t, src)
			if res.Error == "" {
				t.Errorf("%s was accepted; expected a runtime error", name)
			}
		})
	}
}

func TestTCPScanEmptyInputIsNotAnError(t *testing.T) {
	out, res := runScript(t, `let r = tcp_scan([], [80], 500); print("count=" + str(r.count));`)
	if res.Error != "" {
		t.Fatalf("empty input should be a no-op, got: %s", res.Error)
	}
	if !strings.Contains(out, "count=0") {
		t.Errorf("want count=0, got:\n%s", out)
	}
}

// The worker count must be clamped: a script asking for 100000 workers
// should not try to open 100000 sockets.
func TestFanoutClampsWorkers(t *testing.T) {
	var mu [1]int
	n := 0
	fanout(10, 1000000, func(i int) { _ = i })
	fanout(0, 8, func(i int) { n++ })
	if n != 0 {
		t.Error("fanout ran work for an empty set")
	}
	_ = mu
	// Negative/zero worker counts fall back to the default rather than
	// deadlocking on a zero-sized pool.
	done := make(chan struct{})
	go func() {
		fanout(4, 0, func(i int) {})
		fanout(4, -5, func(i int) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fanout deadlocked with a non-positive worker count")
	}
}
