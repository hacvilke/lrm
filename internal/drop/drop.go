// Package drop serves a tiny, token-guarded upload page on the LAN so a
// phone (or any device with only a browser) can send files into an LRM
// repository without installing anything.
//
// Design rules, in order of importance:
//
//   - It is a door with one lock: every request must carry the token that
//     `lrm receive` printed. Without it, the page reveals nothing.
//   - Uploads land in the repository's inbox/ directory under a sanitized
//     basename; the daemon's watcher commits them like any other change,
//     so a phone upload becomes a normal, synced commit.
//   - Bounded: request size cap, file count cap, and an optional --once
//     mode that shuts the server down after the first successful upload.
//   - No listing, no download, no path input: the server can only ever
//     create files inside one directory.
package drop

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/merkle"
)

// Options configure a receive server.
type Options struct {
	Inbox    string // where uploads land (created if missing)
	Token    string // required secret; generated when empty
	MaxBytes int64  // per-request cap (default 2 GiB)
	// TmpDir stages a part while it streams in. It must be OUTSIDE the
	// working tree (repos pass .lrm/tmp): the daemon watches the tree, and
	// a half-written temp file inside it would be committed as noise — or
	// worse, as a truncated file. Defaults to a sibling .lrm/tmp when one
	// exists, else to Inbox (throwaway use, tests).
	TmpDir   string
	Hostname string // shown in the friendly name; informational
	Once     bool   // stop after the first successful upload
	Port     int    // 0 = ephemeral
	Host     string // "" = all interfaces
	OnResult func(path string, size int64)
	Logf     func(format string, args ...any)
}

// Server is a running receive endpoint.
type Server struct {
	opt  Options
	ln   net.Listener
	srv  *http.Server
	once sync.Once
	done chan struct{}
}

// NewToken mints a URL token: 16 hex chars from 8 random bytes.
func NewToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Start launches the server and returns it with the URL the user should
// open (printed by the CLI, and usually sent to the phone by chat).
func Start(opt Options) (*Server, string, error) {
	if opt.Token == "" {
		t, err := NewToken()
		if err != nil {
			return nil, "", err
		}
		opt.Token = t
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = 2 << 30 // 2 GiB
	}
	if opt.Inbox == "" {
		opt.Inbox = "inbox"
	}
	if err := os.MkdirAll(opt.Inbox, 0o755); err != nil {
		return nil, "", err
	}
	if opt.TmpDir == "" {
		candidate := filepath.Join(filepath.Dir(opt.Inbox), ".lrm", "tmp")
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			opt.TmpDir = candidate
		} else {
			opt.TmpDir = opt.Inbox
		}
	}
	if err := os.MkdirAll(opt.TmpDir, 0o755); err != nil {
		return nil, "", err
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	s := &Server{opt: opt, done: make(chan struct{})}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", opt.Host, opt.Port))
	if err != nil {
		return nil, "", err
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/u", s.handleUpload)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return s, s.URL(), nil
}

// URL is the address to open on the phone (token included).
func (s *Server) URL() string {
	host := s.opt.Hostname
	if host == "" {
		host = bestLANIP()
	}
	port := s.Port()
	return fmt.Sprintf("http://%s:%d/?t=%s", host, port, s.opt.Token)
}

// Port returns the bound port (useful when the OS picked one).
func (s *Server) Port() int {
	if a, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Wait blocks until the server is closed (or --once fires).
func (s *Server) Wait() { <-s.done }

// Close stops the server and unblocks Wait. Idempotent, and it shuts the
// HTTP listener itself — `--once` must stop serving even if nobody is
// blocked in Wait.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.srv.Close()
	})
}

func (s *Server) authed(r *http.Request) bool {
	got := r.URL.Query().Get("t")
	if got == "" {
		got = r.Header.Get("X-LRM-Token")
	}
	if len(got) != len(s.opt.Token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) == 1
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "invalid or missing token — use the URL printed by `lrm receive`", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Send to %s</title><style>
:root{color-scheme:dark}
body{margin:0;background:#0d1117;color:#e6edf3;font:15px/1.5 -apple-system,system-ui,sans-serif}
.wrap{max-width:520px;margin:0 auto;padding:40px 22px}
h1{font-size:20px;margin:0 0 4px}p{color:#8b949e;font-size:13px}
form{border:1px dashed #30363d;border-radius:12px;padding:22px;margin-top:18px;text-align:center}
input[type=file]{color:#e6edf3;margin-bottom:14px;width:100%%}
button{background:#238636;color:#fff;border:0;border-radius:8px;padding:11px 18px;font-size:15px;width:100%%}
.ok{color:#3fb950}.dim{color:#6e7681;font-size:12px;margin-top:14px}
</style></head><body><div class="wrap">
<h1>Send files to %s</h1>
<p>Pick a file (or several) — it lands in the repository's <b>inbox/</b> and gets committed and synced like any other change.</p>
<form method="post" action="/u?t=%s" enctype="multipart/form-data">
<input type="file" name="file" multiple>
<button type="submit">Send</button>
</form>
<div class="dim">single-use link if the receiver was started with --once · files are capped at %s</div>
</div></body></html>`,
		html.EscapeString(s.opt.Hostname), html.EscapeString(s.opt.Hostname),
		html.EscapeString(s.opt.Token), human(s.opt.MaxBytes))
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "invalid or missing token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.opt.MaxBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart upload", http.StatusBadRequest)
		return
	}
	var saved []map[string]any
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if tooLarge(err) {
				http.Error(w, "file too large (cap "+human(s.opt.MaxBytes)+")", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		name, err := sanitizeName(part.FileName())
		if err != nil {
			_ = part.Close()
			http.Error(w, "refused file name: "+err.Error(), http.StatusBadRequest)
			return
		}
		dst, written, err := s.saveOne(name, part)
		_ = part.Close()
		if err != nil {
			if tooLarge(err) {
				http.Error(w, "file too large (cap "+human(s.opt.MaxBytes)+")", http.StatusRequestEntityTooLarge)
				return
			}
			code := http.StatusInternalServerError
			if os.IsExist(err) {
				code = http.StatusConflict
			}
			http.Error(w, "save failed: "+err.Error(), code)
			return
		}
		s.opt.Logf("received %s (%s)", filepath.Base(dst), human(written))
		if s.opt.OnResult != nil {
			s.opt.OnResult(dst, written)
		}
		saved = append(saved, map[string]any{"name": filepath.Base(dst), "bytes": written})
	}
	if len(saved) == 0 {
		http.Error(w, "no file in the request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"saved": saved})
	if s.opt.Once {
		// Shut the door AFTER the response reaches the device: closing
		// first turns a successful upload into a connection reset.
		go func() {
			time.Sleep(250 * time.Millisecond)
			s.Close()
		}()
	}
}

// saveOne streams the part to a temp file (outside the working tree when
// the caller supplied a staging dir), then renames it into place
// (collision-suffixed). Streaming: the file never sits in memory, whatever
// its size, and the rename is atomic, so the watcher only ever sees a
// complete file.
func (s *Server) saveOne(name string, part io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(s.opt.TmpDir, ".up-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	n, err := io.Copy(tmp, part)
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	final := filepath.Join(s.opt.Inbox, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(name)
		final = filepath.Join(s.opt.Inbox, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext))
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, err
	}
	_ = os.Chmod(final, 0o644)
	return final, n, nil
}

// sanitizeName reduces any uploaded name to a safe basename: no
// directories, no traversal, no control characters, and never empty.
// Devices report names like "DCIM/Photo.jpg" or "../evil.sh"; only the
// last component survives, exactly as `lrm send` does.
func sanitizeName(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	base := pathBase(raw)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("empty name")
	}
	if _, err := merkle.CleanTreePath(base); err != nil {
		return "", err
	}
	if len(base) > 200 {
		ext := filepath.Ext(base)
		base = base[:200-len(ext)] + ext
	}
	return base, nil
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// bestLANIP returns the most likely reachable LAN address for the URL.
func bestLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "localhost"
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return "localhost"
}

// tooLarge reports whether err is the size cap talking (the body reader
// aborts mid-stream, so the failure can surface inside a copy, not just at
// the multipart boundary).
func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}

func human(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
}
