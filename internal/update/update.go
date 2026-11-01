// Package update implements LRM's self-updater.
//
// It asks the GitHub releases API for the newest tag, compares it with the
// version this binary was stamped with, downloads the asset for this
// platform, verifies it against the release's SHA256SUMS.txt, and swaps it
// into place.
//
// Two rules shape the design:
//
//   - Nothing is installed that was not hash-verified against the release
//     manifest. A truncated download or a swapped asset is refused, not
//     "retried next time".
//   - The running binary is never overwritten in place. On Windows a running
//     .exe cannot be written to or deleted; even on Unix, replacing a binary
//     mid-run is how you get a half-written executable if anything fails.
//     The new file is written beside the old one, then renamed over it — and
//     on Windows the old file is first renamed aside and removed afterwards.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is where releases live.
const DefaultRepo = "hacvilke/lrm"

// maxDownload bounds an asset download (the binaries are ~8 MiB).
const maxDownload = 128 << 20

// Release is the subset of the GitHub release payload we need.
type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
	Body    string  `json:"body"`
}

// Asset is one downloadable file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Check describes the update decision.
type Check struct {
	Current     string // version this binary was built as
	Latest      string // newest published tag
	Available   bool
	AssetName   string
	AssetURL    string
	ReleaseURL  string
	ReleaseNote string
}

// Fetcher abstracts the network so tests do not need GitHub.
type Fetcher interface {
	LatestRelease(ctx context.Context, repo string) (*Release, error)
	Get(ctx context.Context, url string, w io.Writer) (int64, error)
}

// HTTPFetcher talks to the real API.
type HTTPFetcher struct {
	Client *http.Client
	// BaseURL overrides https://api.github.com (tests).
	BaseURL string
}

func (f *HTTPFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// LatestRelease fetches /repos/<repo>/releases/latest.
func (f *HTTPFetcher) LatestRelease(ctx context.Context, repo string) (*Release, error) {
	base := f.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release lookup: HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("release lookup: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release lookup: no tag in response")
	}
	return &rel, nil
}

// Get downloads url into w.
func (f *HTTPFetcher) Get(ctx context.Context, url string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	return io.Copy(w, io.LimitReader(resp.Body, maxDownload))
}

// AssetNameFor returns the release asset for a GOOS/GOARCH pair.
func AssetNameFor(goos, goarch string) string {
	return assetName("", goos, goarch)
}

// AssetNameForFlavor returns the asset for a build flavor. Flavor "desktop" is
// the double-clickable app; "" is the lrm CLI. They are the same program with
// a different entry point, so the updater must replace itself with the same
// flavor — installing lrm.exe over lrm-desktop.exe would leave the user with
// a binary that prints usage when double-clicked.
func AssetNameForFlavor(flavor, goos, goarch string) string {
	return assetName(flavor, goos, goarch)
}

func assetName(flavor, goos, goarch string) string {
	name := "lrm"
	if flavor != "" {
		name += "_" + flavor
	}
	name += "_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// FlavorOf guesses the build flavor from the running binary's name.
func FlavorOf(exePath string) string {
	// Split on both separators: a Windows path can be inspected on any host
	// (tests, cross-built tooling), and filepath.Base only knows the local one.
	if i := strings.LastIndexAny(exePath, `/\\`); i >= 0 {
		exePath = exePath[i+1:]
	}
	base := strings.ToLower(exePath)
	base = strings.TrimSuffix(base, ".exe")
	if strings.HasPrefix(base, "lrm-desktop") || strings.HasPrefix(base, "lrm_desktop") {
		return "desktop"
	}
	return ""
}

// NormalizeVersion strips a leading v so "v0.3.6" and "0.3.6" compare equal.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

// CompareVersions returns -1, 0 or 1 comparing dotted numeric versions.
// Non-numeric suffixes ("0.3.6-rc1") are compared as strings after the
// numeric part, which is enough for release tags of this shape.
func CompareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var av, bv int
		if i < len(an) {
			av = an[i]
		}
		if i < len(bn) {
			bv = bn[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1 // release beats pre-release
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

func splitVersion(v string) ([]int, string) {
	v = NormalizeVersion(v)
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	return nums, pre
}

// Plan decides whether an update is available for this platform.
func Plan(current string, rel *Release, goos, goarch string) Check {
	return PlanFlavor(current, rel, goos, goarch, "")
}

// PlanFlavor is Plan for a specific build flavor ("" = CLI, "desktop" = app).
func PlanFlavor(current string, rel *Release, goos, goarch, flavor string) Check {
	c := Check{Current: current, Latest: rel.TagName, ReleaseURL: rel.HTMLURL, ReleaseNote: rel.Body}
	if CompareVersions(rel.TagName, current) <= 0 {
		return c
	}
	want := AssetNameForFlavor(flavor, goos, goarch)
	for _, a := range rel.Assets {
		if a.Name == want {
			c.Available = true
			c.AssetName = a.Name
			c.AssetURL = a.URL
			return c
		}
	}
	// A release without our platform is not an update we can install.
	return c
}

// ParseSums reads a SHA256SUMS.txt body into a name→hex map.
func ParseSums(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "<hex>  <name>" (binary mode marker optional).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		name = strings.TrimPrefix(name, "./")
		out[filepath.Base(name)] = strings.ToLower(fields[0])
	}
	return out
}

// Options configures Apply.
type Options struct {
	Repo    string
	Current string
	// Dir is the directory to install into (default: dir of the running exe).
	Dir string
	// ExePath overrides the running binary (tests / odd launchers).
	ExePath string
	GOOS    string
	GOARCH  string
	// Flavor selects which artifact to install: "" for the CLI,
	// "desktop" for the app. Defaults to whatever the running binary is.
	Flavor  string
	Out     io.Writer
	Fetcher Fetcher
}

// Result reports what happened.
type Result struct {
	Updated     bool
	From, To    string
	InstalledAt string
	Note        string
}

// Apply runs the whole update: check, download, verify, swap.
func Apply(ctx context.Context, opts Options) (*Result, error) {
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	f := opts.Fetcher
	if f == nil {
		f = &HTTPFetcher{}
	}
	say := func(format string, a ...any) { fmt.Fprintf(opts.Out, format+"\n", a...) }

	exe := opts.ExePath
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate running binary: %w", err)
		}
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		exe = self
	}
	flavor := opts.Flavor
	if flavor == "" && opts.ExePath == "" {
		flavor = FlavorOf(exe)
	}

	rel, err := f.LatestRelease(ctx, opts.Repo)
	if err != nil {
		return nil, err
	}
	plan := PlanFlavor(opts.Current, rel, opts.GOOS, opts.GOARCH, flavor)
	res := &Result{From: opts.Current, To: rel.TagName}
	if !plan.Available {
		res.Note = "already up to date"
		return res, nil
	}
	if plan.AssetURL == "" {
		res.Note = fmt.Sprintf("release %s has no %s asset", rel.TagName, AssetNameForFlavor(flavor, opts.GOOS, opts.GOARCH))
		return res, fmt.Errorf("%s", res.Note)
	}

	dir := opts.Dir
	if dir == "" {
		dir = filepath.Dir(exe)
	}

	sumsURL := ""
	for _, a := range rel.Assets {
		if a.Name == "SHA256SUMS.txt" {
			sumsURL = a.URL
		}
	}
	if sumsURL == "" {
		return nil, fmt.Errorf("release %s has no SHA256SUMS.txt — refusing to install unverified", rel.TagName)
	}

	// 1. expected hash
	sums := ""
	{
		var sb strings.Builder
		if _, err := f.Get(ctx, sumsURL, &sb); err != nil {
			return nil, fmt.Errorf("fetch checksums: %w", err)
		}
		sums = sb.String()
	}
	wantHash := ParseSums(sums)[plan.AssetName]
	if wantHash == "" {
		return nil, fmt.Errorf("checksum manifest has no entry for %s", plan.AssetName)
	}

	// 2. download beside the target, hashing as it lands
	tmp := filepath.Join(dir, "."+plan.AssetName+".new")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", tmp, err)
	}
	h := sha256.New()
	n, err := f.Get(ctx, plan.AssetURL, io.MultiWriter(out, h))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("download: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantHash {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("checksum mismatch for %s: got %s want %s — nothing installed", plan.AssetName, got[:16], wantHash[:16])
	}
	say("downloaded %s (%d bytes)", plan.AssetName, n)
	say("verified sha256 %s", got)

	// 3. swap
	if err := swap(exe, tmp, opts.GOOS); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	res.Updated = true
	res.InstalledAt = exe
	res.Note = "restart lrm to use the new version"
	say("installed %s -> %s", rel.TagName, exe)
	return res, nil
}

// swap replaces target with src, working around Windows' refusal to replace a
// running executable: the old file is renamed aside (allowed), the new file
// takes the name, and the old one is removed. On failure the old file is put
// back.
func swap(target, src, goos string) error {
	if goos != "windows" {
		if err := os.Rename(src, target); err != nil {
			return fmt.Errorf("replace %s: %w", target, err)
		}
		return nil
	}
	old := target + ".old"
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("move running binary aside: %w", err)
	}
	if err := os.Rename(src, target); err != nil {
		// Put the original back — a failed update must not lose the binary.
		_ = os.Rename(old, target)
		return fmt.Errorf("install new binary: %w", err)
	}
	// Best effort: a running exe cannot be deleted until the process exits,
	// so a leftover .old is normal and harmless, and cleaned next run.
	if err := os.Remove(old); err != nil {
		_ = os.WriteFile(target+".old.leftover", []byte(time.Now().Format(time.RFC3339)), 0o644)
	}
	return nil
}

// CleanLeftovers removes "*.old" files a Windows swap could not delete while
// the previous process was still running.
func CleanLeftovers(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".old") || strings.HasSuffix(name, ".old.leftover") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// SortedAssets is a helper for stable output in tests and messages.
func SortedAssets(rel *Release) []string {
	out := make([]string, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}
