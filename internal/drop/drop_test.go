package drop

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func start(t *testing.T, opt Options) (*Server, string) {
	t.Helper()
	if opt.Inbox == "" {
		opt.Inbox = filepath.Join(t.TempDir(), "inbox")
	}
	if opt.Port == 0 {
		opt.Port = 0
	}
	s, url, err := Start(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, url
}

// uploadURL turns the page URL (…/?t=token) into the POST endpoint.
func uploadURL(url string) string {
	i := strings.Index(url, "?")
	base, query := strings.TrimSuffix(url[:i], "/"), url[i:]
	return base + "/u" + query
}

func upload(t *testing.T, url, field, filename, body string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	resp, err := http.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTokenIsRequired(t *testing.T) {
	_, url := start(t, Options{})
	base := url[:strings.Index(url, "?")]

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("page without token: %d, want 401", resp.StatusCode)
	}

	resp2, err := http.Get(base + "/?t=wrongwrongwrongw") // right length, wrong value
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("page with wrong token: %d, want 401", resp2.StatusCode)
	}

	resp3, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 || !strings.Contains(readAll(t, resp3), "Send files") {
		t.Fatalf("page with token: %d", resp3.StatusCode)
	}
}

func TestUploadLandsInInbox(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	_, url := start(t, Options{Inbox: inbox})
	up := uploadURL(url)

	resp := upload(t, up, "file", "notes.txt", "hello from the phone")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, readAll(t, resp))
	}
	var out struct {
		Saved []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		} `json:"saved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Saved) != 1 || out.Saved[0].Name != "notes.txt" || out.Saved[0].Bytes != 20 {
		t.Fatalf("unexpected result: %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(inbox, "notes.txt"))
	if err != nil || string(got) != "hello from the phone" {
		t.Fatalf("file not stored intact: %q %v", got, err)
	}
}

// Devices send names like "../evil.sh", "DCIM/Photo.jpg", or with control
// characters. Only a clean basename may survive, and nothing may be
// written outside the inbox.
func TestHostileNamesAreNeutralized(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	outside := filepath.Join(filepath.Dir(inbox), "escaped.txt")
	_, url := start(t, Options{Inbox: inbox})
	up := uploadURL(url)

	for _, name := range []string{"../escaped.txt", "..\\escaped.txt", "DCIM/Photo.jpg", "./x/y/photo.jpg"} {
		resp := upload(t, up, "file", name, "data")
		body := readAll(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upload %q failed: %d %s", name, resp.StatusCode, body)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("SECURITY: upload escaped the inbox")
	}
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.ContainsAny(e.Name(), `/\`) || strings.HasPrefix(e.Name(), "..") {
			t.Fatalf("hostile name stored: %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, "escaped.txt")); err != nil {
		t.Fatalf("basename should have been kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "Photo.jpg")); err != nil {
		t.Fatalf("basename from DCIM path should have been kept: %v", err)
	}
}

func TestSizeCapRejects(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	_, url := start(t, Options{Inbox: inbox, MaxBytes: 1024})
	up := uploadURL(url)

	resp := upload(t, up, "file", "big.bin", strings.Repeat("A", 4096))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: %d, want 413", resp.StatusCode)
	}
	entries, _ := os.ReadDir(inbox)
	for _, e := range entries {
		t.Fatalf("partial file left behind: %s", e.Name())
	}
}

func TestOnceModeClosesAfterUpload(t *testing.T) {
	_, url := start(t, Options{Once: true})
	up := uploadURL(url)

	resp := upload(t, up, "file", "one.txt", "x")
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}
	// The server should stop accepting; a follow-up request must fail.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := http.Get(url)
		if err != nil {
			return // closed: correct
		}
		if time.Now().After(deadline) {
			t.Fatal("--once server still accepting after an upload")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCollisionSuffix(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	_, url := start(t, Options{Inbox: inbox})
	up := uploadURL(url)

	for i := 0; i < 2; i++ {
		resp := upload(t, up, "file", "same.txt", "v")
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upload %d failed: %d", i, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, "same.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "same-1.txt")); err != nil {
		t.Fatal("second upload should be suffixed, not overwritten")
	}
}

func TestSanitizeName(t *testing.T) {
	good := map[string]string{
		"photo.jpg":        "photo.jpg",
		"DCIM/photo.jpg":   "photo.jpg",
		`C:\Users\a\b.txt`: "b.txt",
		".hidden":          ".hidden",
	}
	for in, want := range good {
		got, err := sanitizeName(in)
		if err != nil || got != want {
			t.Errorf("sanitizeName(%q) = %q, %v — want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "a\x00b", "x/\u202ey", "/"} {
		if got, err := sanitizeName(bad); err == nil {
			t.Errorf("sanitizeName(%q) accepted → %q", bad, got)
		}
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
