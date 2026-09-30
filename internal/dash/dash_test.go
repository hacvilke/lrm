package dash

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStatus(t *testing.T, dir string, st Status) string {
	t.Helper()
	p := filepath.Join(dir, "status.json")
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	for _, d := range []string{"docs", "src"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"README.md":      "# hello\nline two\n",
		"docs/guide.md":  "guide\n",
		"src/main.go":    "package main\n",
		"README copy.md": "duplicate\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The repo's own secrets: the browser must never reach these.
	if err := os.MkdirAll(filepath.Join(root, ".lrm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lrm", "identity.key"), []byte("SUPER-SECRET-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func serve(t *testing.T, root, statusPath, url string) *httptest.ResponseRecorder {
	t.Helper()
	h := Handler(statusPath, root, func() (Status, error) { return Load(statusPath) }, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	return rec
}

// A synced tree names files, and those names end up on a page: hostile
// markup must render as text.
func TestRenderEscapesHostileNames(t *testing.T) {
	st := Status{
		User:   `alice</title><script>alert(1)</script>`,
		PeerID: strings.Repeat("a", 64),
		Port:   8443,
		Peers: []PeerView{{
			ID: strings.Repeat("b", 64), User: `<img src=x onerror="alert(1)">`,
			Addr: `"><script>alert(2)</script>`, State: "<svg onload=alert(3)>",
		}},
		Known:   []PeerView{{ID: strings.Repeat("c", 64), User: "<b>bold</b>"}},
		Syncs:   []SyncEntry{{Time: "2026-09-30T10:00:00Z", Peer: strings.Repeat("d", 64), Message: "<script>x</script>", Files: []string{"<img src=y onerror=z>"}}},
		Rejects: []RejectEntry{{Time: "2026-09-30T10:00:00Z", Peer: strings.Repeat("e", 64), Reason: "<iframe src=//evil>"}},
	}
	page := string(Render(st))
	for _, bad := range []string{"<script", "<img ", "<svg ", "<iframe", "<b>bold</b>"} {
		if strings.Contains(page, bad) {
			t.Fatalf("unescaped %q in rendered dashboard", bad)
		}
	}
	if !strings.Contains(page, "&lt;script") {
		t.Fatal("escaped content missing")
	}

	st.File, st.FileBody, st.FileSize, st.FileBig, st.FileBin = "docs/<x>.md", "<script>alert(9)</script>", 30, false, false
	page = string(RenderFiles(st))
	if strings.Contains(page, "<script>alert(9)") {
		t.Fatal("file preview rendered raw markup")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("file preview escaped content missing")
	}
}

func TestRenderEmptyState(t *testing.T) {
	page := string(Render(Status{User: "alice", PeerID: strings.Repeat("a", 64), Port: 1, Workspace: strings.Repeat("f", 32)}))
	for _, want := range []string{"no peer connected right now", "nothing synced yet", "nothing has been turned away"} {
		if !strings.Contains(page, want) {
			t.Fatalf("empty-state text %q missing", want)
		}
	}
}

func TestLoadMissingAndGarbage(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || st.User != "" {
		t.Fatalf("missing status should load empty: %v", err)
	}
	p := filepath.Join(t.TempDir(), "status.json")
	_ = os.WriteFile(p, []byte("{nope"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("garbage status accepted")
	}
}

func TestBrowseDirectoriesAndFiles(t *testing.T) {
	root := testRepo(t)
	statusPath := writeStatus(t, t.TempDir(), Status{User: "alice", PeerID: strings.Repeat("a", 64), Head: "abc12345", Branch: "main"})

	// Root listing: directories first, .lrm hidden.
	rec := serve(t, root, statusPath, "/files/")
	if rec.Code != 200 {
		t.Fatalf("root listing: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"README.md", "docs/", "src/", "17 B"} {
		if !strings.Contains(body, want) {
			t.Fatalf("listing missing %q", want)
		}
	}
	// Directories sort before files.
	if strings.Index(body, "docs/") > strings.Index(body, "README.md") {
		t.Fatal("directories should list first")
	}
	if strings.Contains(body, ".lrm") {
		t.Fatal("SECURITY: .lrm directory listed")
	}

	// Subdirectory listing.
	body = serve(t, root, statusPath, "/files/docs/").Body.String()
	if !strings.Contains(body, "guide.md") {
		t.Fatal("subdirectory listing missing file")
	}

	// File preview.
	body = serve(t, root, statusPath, "/files/README.md").Body.String()
	if !strings.Contains(body, "# hello") || !strings.Contains(body, "line two") {
		t.Fatal("file preview missing content")
	}
	if !strings.Contains(body, "/raw/README.md") {
		t.Fatal("preview missing raw download link")
	}

	// Raw download streams the exact bytes as an opaque attachment.
	rec = serve(t, root, statusPath, "/raw/README.md")
	if rec.Code != 200 || rec.Body.String() != "# hello\nline two\n" {
		t.Fatalf("raw download wrong: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("raw served as %q — synced HTML must never render on this origin", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("raw content-disposition = %q", cd)
	}
}

// The browser is the one part of the product that reads the local disk;
// it must be incapable of reading anything outside the repository.
func TestBrowserTraversalRefused(t *testing.T) {
	root := testRepo(t)
	parent := filepath.Dir(root)
	secret := filepath.Join(parent, "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("NOT-YOURS"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusPath := writeStatus(t, t.TempDir(), Status{User: "alice"})

	for _, url := range []string{
		"/files/../outside-secret.txt",
		"/files/%2e%2e/outside-secret.txt",
		"/files/../../etc/passwd",
		"/raw/../outside-secret.txt",
		"/raw/%2e%2e%2Foutside-secret.txt",
		"/files/.lrm/",
		"/files/.lrm/identity.key",
		"/raw/.lrm/identity.key",
		"/files/.lrm%2Fidentity.key",
		"/files/a//b",
		"/files/..",
	} {
		rec := serve(t, root, statusPath, url)
		body := rec.Body.String()
		if rec.Code == 200 && (strings.Contains(body, "NOT-YOURS") || strings.Contains(body, "SUPER-SECRET-KEY") || strings.Contains(body, "root:")) {
			t.Fatalf("SECURITY: %s served a file outside the repo (code %d)", url, rec.Code)
		}
		if strings.Contains(body, "SUPER-SECRET-KEY") {
			t.Fatalf("SECURITY: identity key reachable at %s", url)
		}
	}

	// The lock holds for a direct request too (not just listings).
	rec := serve(t, root, statusPath, "/raw/.lrm/identity.key")
	if rec.Code == 200 {
		t.Fatalf("SECURITY: identity.key downloaded (code %d)", rec.Code)
	}
}

// A synced symlink pointing outside the repo must not become a window.
func TestBrowserSymlinkEscapeRefused(t *testing.T) {
	root := testRepo(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(root), filepath.Join(root, "updir")); err != nil {
		t.Fatal(err)
	}
	statusPath := writeStatus(t, t.TempDir(), Status{User: "alice"})
	for _, url := range []string{"/files/link.txt", "/raw/link.txt", "/files/updir/elsewhere.txt", "/raw/updir/"} {
		rec := serve(t, root, statusPath, url)
		if rec.Code == 200 && strings.Contains(rec.Body.String(), "OUTSIDE-DATA") {
			t.Fatalf("SECURITY: symlink escape served %s", url)
		}
	}
}

func TestPreviewCapsAndBinary(t *testing.T) {
	root := testRepo(t)
	big := strings.Repeat("A", maxPreview+100)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0x02, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	statusPath := writeStatus(t, t.TempDir(), Status{User: "alice"})

	body := serve(t, root, statusPath, "/files/big.txt").Body.String()
	if strings.Contains(body, big) {
		t.Fatal("oversized file rendered inline")
	}
	if !strings.Contains(body, "larger than the inline preview limit") {
		t.Fatal("oversized file has no explanation")
	}
	if !strings.Contains(body, "/raw/big.txt") {
		t.Fatal("oversized file cannot be downloaded")
	}

	body = serve(t, root, statusPath, "/files/bin.dat").Body.String()
	if !strings.Contains(body, "binary file") {
		t.Fatal("binary file not detected")
	}
	// And it still downloads byte-exact.
	rec := serve(t, root, statusPath, "/raw/bin.dat")
	if rec.Body.Len() != 4 {
		t.Fatalf("binary download truncated: %d bytes", rec.Body.Len())
	}
}

func TestMeshEndpoints(t *testing.T) {
	root := testRepo(t)
	st := Status{User: "alice", PeerID: strings.Repeat("a", 64), Port: 8443, Workspace: strings.Repeat("w", 32),
		Peers: []PeerView{{ID: strings.Repeat("b", 64), User: "bob", State: "live", Addr: "192.168.1.9:8443", RTTms: 12}},
		Syncs: []SyncEntry{{Time: "2026-09-30T10:00:00Z", Peer: strings.Repeat("b", 64), User: "bob", Message: "fast-forwarded", Fetched: 1, Files: []string{"docs/guide.md"}}},
	}
	statusPath := writeStatus(t, t.TempDir(), st)

	rec := serve(t, root, statusPath, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "bob") {
		t.Fatalf("mesh page wrong: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "docs/guide.md") {
		t.Fatal("sync entry does not list changed files")
	}
	rec = serve(t, root, statusPath, "/status.json")
	if rec.Code != 404 {
		t.Fatalf("raw status endpoint should not exist in this build: %d", rec.Code)
	}
}
