package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake serves a canned release and assets, so the whole update path is
// exercised without a network or a real GitHub release.
type fake struct {
	rel   *Release
	files map[string][]byte
}

func (f *fake) LatestRelease(context.Context, string) (*Release, error) { return f.rel, nil }

func (f *fake) Get(_ context.Context, url string, w io.Writer) (int64, error) {
	b, ok := f.files[url]
	if !ok {
		return 0, os.ErrNotExist
	}
	n, err := w.Write(b)
	return int64(n), err
}

func setup(t *testing.T, assetName string, assetBody []byte) (*fake, string) {
	t.Helper()
	sum := sha256.Sum256(assetBody)
	sums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	rel := &Release{
		TagName: "v0.9.9",
		HTMLURL: "https://example.invalid/release",
		Assets: []Asset{
			{Name: assetName, URL: "https://example.invalid/" + assetName},
			{Name: "SHA256SUMS.txt", URL: "https://example.invalid/SHA256SUMS.txt"},
		},
	}
	f := &fake{rel: rel, files: map[string][]byte{
		"https://example.invalid/" + assetName:   assetBody,
		"https://example.invalid/SHA256SUMS.txt": []byte(sums),
	}}
	dir := t.TempDir()
	return f, dir
}

func TestUpdatesWhenNewerExists(t *testing.T) {
	body := []byte("#!fake-binary\n")
	f, dir := setup(t, AssetNameFor("linux", "amd64"), body)
	target := filepath.Join(dir, "lrm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), Options{
		Current: "v0.3.6", Dir: dir, ExePath: target,
		GOOS: "linux", GOARCH: "amd64", Fetcher: f,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Updated {
		t.Fatalf("expected update, got %+v", res)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed binary is not executable: %v %v", fi.Mode(), err)
	}
	// No temp file left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestNoUpdateWhenCurrent(t *testing.T) {
	f, dir := setup(t, AssetNameFor("linux", "amd64"), []byte("x"))
	res, err := Apply(context.Background(), Options{
		Current: "v0.9.9", Dir: dir, ExePath: filepath.Join(dir, "lrm"),
		GOOS: "linux", GOARCH: "amd64", Fetcher: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatalf("must not update when already newest: %+v", res)
	}
}

// A tampered download must be refused, and the working binary must survive.
func TestRefusesBadChecksum(t *testing.T) {
	body := []byte("good")
	f, dir := setup(t, AssetNameFor("linux", "amd64"), body)
	// Serve different bytes than the manifest describes.
	f.files["https://example.invalid/"+AssetNameFor("linux", "amd64")] = []byte("evil")
	target := filepath.Join(dir, "lrm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(context.Background(), Options{
		Current: "v0.3.6", Dir: dir, ExePath: target,
		GOOS: "linux", GOARCH: "amd64", Fetcher: f,
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum refusal, got %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("binary was modified despite failed verification: %q", got)
	}
}

// A release with no manifest is not installed at all.
func TestRefusesMissingManifest(t *testing.T) {
	f, dir := setup(t, AssetNameFor("linux", "amd64"), []byte("x"))
	f.rel.Assets = []Asset{{Name: AssetNameFor("linux", "amd64"), URL: "https://example.invalid/lrm"}}
	_, err := Apply(context.Background(), Options{
		Current: "v0.3.6", Dir: dir, ExePath: filepath.Join(dir, "lrm"),
		GOOS: "linux", GOARCH: "amd64", Fetcher: f,
	})
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("expected refusal without manifest, got %v", err)
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.3.6", "0.3.6", 0},
		{"v0.3.7", "v0.3.6", 1},
		{"v0.3.6", "v0.3.10", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.4", "v0.4.0", 0},
		{"v0.3.6-rc1", "v0.3.6", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetNames(t *testing.T) {
	if got := AssetNameFor("windows", "amd64"); got != "lrm_windows_amd64.exe" {
		t.Errorf("windows asset name = %q", got)
	}
	if got := AssetNameFor("darwin", "arm64"); got != "lrm_darwin_arm64" {
		t.Errorf("darwin asset name = %q", got)
	}
}

func TestParseSums(t *testing.T) {
	body := `# generated
abc123  lrm_linux_amd64
DEF456 *lrm_windows_amd64.exe
`
	m := ParseSums(body)
	if m["lrm_linux_amd64"] != "abc123" {
		t.Errorf("linux sum = %q", m["lrm_linux_amd64"])
	}
	if m["lrm_windows_amd64.exe"] != "def456" {
		t.Errorf("windows sum = %q (binary-mode star must be stripped, hex lowercased)", m["lrm_windows_amd64.exe"])
	}
}

// The app must update itself to the app, not to the CLI: installing lrm.exe
// over lrm-desktop.exe leaves a binary that prints usage when double-clicked.
func TestFlavorSelectsItsOwnAsset(t *testing.T) {
	if got := AssetNameForFlavor("desktop", "windows", "amd64"); got != "lrm_desktop_windows_amd64.exe" {
		t.Fatalf("desktop windows asset = %q", got)
	}
	if got := AssetNameForFlavor("", "windows", "amd64"); got != "lrm_windows_amd64.exe" {
		t.Fatalf("cli windows asset = %q", got)
	}
	for _, c := range []struct{ path, want string }{
		{"/usr/local/bin/lrm-desktop", "desktop"},
		{`C:\Users\ada\LRM\lrm_desktop_windows_amd64.exe`, "desktop"},
		{"/usr/local/bin/lrm", ""},
		{`C:\Users\ada\LRM\lrm_windows_amd64.exe`, ""},
	} {
		if got := FlavorOf(c.path); got != c.want {
			t.Errorf("FlavorOf(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	body := []byte("app-binary")
	f, dir := setup(t, "lrm_desktop_windows_amd64.exe", body)
	// The fake release must carry the desktop asset, not the CLI one.
	f.rel.Assets = []Asset{
		{Name: "lrm_desktop_windows_amd64.exe", URL: "https://example.invalid/lrm_desktop_windows_amd64.exe"},
		{Name: "SHA256SUMS.txt", URL: "https://example.invalid/SHA256SUMS.txt"},
	}
	f.files["https://example.invalid/lrm_desktop_windows_amd64.exe"] = body
	target := filepath.Join(dir, "lrm_desktop_windows_amd64.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), Options{
		Current: "v0.3.6", Dir: dir, ExePath: target,
		GOOS: "windows", GOARCH: "amd64", Flavor: "desktop", Fetcher: f,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Updated {
		t.Fatalf("desktop update did not run: %+v", res)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(body) {
		t.Fatalf("app binary not replaced: %q", got)
	}
}
