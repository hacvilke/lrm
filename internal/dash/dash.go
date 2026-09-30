// Package dash renders a local, read-only view of one LRM workspace: the
// live mesh (presence, discovery, syncs, refusals) and the workspace's own
// files, browsable in a browser like a repository page.
//
// It has three parts, all local:
//
//	status.json  — written by `lrm daemon`, read by everything here
//	Render       — one self-contained HTML page (no JS, no external assets)
//	Files        — a read-only browser over the working tree, confined to
//	               the repository root: it can never read outside it
//
// Nothing in this package talks to the network, and the file browser
// refuses the repo's own .lrm directory outright — identity.key lives
// there, and a local page must never become a way to read it.
package dash

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/merkle"
)

// maxPreview is how much of a file the browser will render inline. Larger
// files are offered as a download instead — a page is not a file manager.
const maxPreview = 256 << 10

// PeerView is one peer as shown in the dashboard. IDs are full 64-hex so
// they can be copy-pasted straight into `lrm share` / pairing flows.
type PeerView struct {
	ID       string `json:"id"`
	User     string `json:"user,omitempty"`
	Node     string `json:"node,omitempty"`
	Addr     string `json:"addr,omitempty"`
	State    string `json:"state,omitempty"`
	Source   string `json:"source,omitempty"`
	Since    string `json:"since,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
	RTTms    int64  `json:"rtt_ms,omitempty"`
	WS       string `json:"ws,omitempty"` // empty unless this peer shares our workspace
}

// SyncEntry is one completed sync.
type SyncEntry struct {
	Time     string   `json:"time"`
	Peer     string   `json:"peer"`
	User     string   `json:"user,omitempty"`
	Message  string   `json:"message"`
	Fetched  int      `json:"fetched"`
	Pushed   int      `json:"pushed"`
	Outbound bool     `json:"outbound"`
	Files    []string `json:"files,omitempty"` // changed paths, capped
}

// EventEntry is one daemon activity line (peer live, resync, refusal...).
type EventEntry struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// RejectEntry is one refused connection or refused operation.
type RejectEntry struct {
	Time   string `json:"time"`
	Peer   string `json:"peer,omitempty"`
	Reason string `json:"reason"`
}

// FileView is one entry in a browsed directory.
type FileView struct {
	Name    string `json:"name"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mod_time,omitempty"`
	IsDir   bool   `json:"is_dir"`
}

// Status is the whole snapshot the daemon publishes, plus the file listing
// the page needs. It contains only information already local to the
// machine; publishing is what `lrm dashboard` reads.
type Status struct {
	Updated   string        `json:"updated"`
	User      string        `json:"user"`
	PeerID    string        `json:"peer_id"`
	NodeID    string        `json:"node_id,omitempty"`
	Port      int           `json:"port"`
	Workspace string        `json:"workspace,omitempty"`
	Branch    string        `json:"branch,omitempty"`
	Head      string        `json:"head,omitempty"`
	Peers     []PeerView    `json:"peers"` // connected right now
	Known     []PeerView    `json:"known"` // seen on the LAN
	Syncs     []SyncEntry   `json:"syncs"`
	Rejects   []RejectEntry `json:"rejects"`
	Events    []EventEntry  `json:"events"`

	// View fields (never serialized): filled per request by the browser.
	Dir      string     `json:"-"`
	Entries  []FileView `json:"-"`
	File     string     `json:"-"` // file being previewed, if any
	FileBody string     `json:"-"`
	FileSize int64      `json:"-"`
	FileBig  bool       `json:"-"`
	FileBin  bool       `json:"-"`
	Parents  []Crumb    `json:"-"`
}

// Crumb is one breadcrumb segment.
type Crumb struct {
	Name string
	Path string
}

// Load reads a status file. A missing file is not an error: it just means
// the daemon has not written one yet.
func Load(statusPath string) (Status, error) {
	var s Status
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("status file %s is unreadable: %w", statusPath, err)
	}
	return s, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// esc escapes untrusted text for HTML. Peer user names, addresses, and
// file names arrive from the network, so every field that reaches the page
// goes through here — a crafted name must render as text, never markup.
func esc(s string) string { return html.EscapeString(s) }

func clock(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Format("15:04:05")
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

const pageCSS = `<style>
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;background:#0d1117;color:#e6edf3;font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.wrap{max-width:900px;margin:0 auto;padding:24px 20px 60px}
h1{font-size:18px;margin:0 0 2px;font-weight:600}
a{color:#79c0ff;text-decoration:none}a:hover{text-decoration:underline}
.sub{color:#8b949e;font-size:12px;margin-bottom:20px}
.card{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:14px 16px;margin:0 0 14px}
.card h2{font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:#8b949e;margin:0 0 10px;font-weight:600}
.row{display:flex;justify-content:space-between;gap:12px;padding:5px 0;border-top:1px solid #21262d}
.row:first-of-type{border-top:0}
.ok{color:#3fb950}.warn{color:#d29922}.bad{color:#f85149}.dim{color:#6e7681}
.pill{display:inline-block;border:1px solid #30363d;border-radius:999px;padding:1px 8px;font-size:11px;color:#8b949e}
.empty{color:#6e7681;padding:6px 0}
.id{color:#79c0ff}
table{width:100%;border-collapse:collapse}
td{padding:5px 0;vertical-align:top;border-top:1px solid #21262d}
tr:first-child td{border-top:0}
td.t{color:#8b949e;white-space:nowrap;width:1%;padding-right:12px}
td.n{text-align:right;white-space:nowrap;color:#8b949e}
td.sz{text-align:right;white-space:nowrap;color:#8b949e;width:1%;padding-left:12px}
.crumbs{color:#8b949e;margin-bottom:10px;font-size:13px}
pre{border:1px solid #30363d;border-radius:8px;padding:12px;overflow:auto;max-height:60vh;font-size:12.5px;line-height:1.45;white-space:pre}
.footer{color:#6e7681;font-size:11px;margin-top:18px}
</style>`

func pageHead(title string) string {
	return `<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + esc(title) + `</title>` + pageCSS
}

func navBar(active string) string {
	link := func(href, label string) string {
		if label == active {
			return `<b>` + label + `</b>`
		}
		return `<a href="` + href + `">` + label + `</a>`
	}
	return `<div class="crumbs">` + link("/", "mesh") + ` · ` + link("/files/", "files") + `</div>`
}

// Render returns the complete dashboard page: one file, inline CSS, no
// external resources, safe to open directly from disk.
func Render(s Status) []byte {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en"><head>`)
	b.WriteString(pageHead("LRM — " + s.User))
	b.WriteString(`</head><body><div class="wrap">`)

	b.WriteString(`<h1>LRM workspace</h1><div class="sub">`)
	b.WriteString(esc(s.User) + ` · <span class="id">` + esc(short(s.PeerID)) + `</span> · port ` + esc(fmt.Sprint(s.Port)))
	if s.Workspace != "" {
		b.WriteString(` · workspace <span class="id">` + esc(short(s.Workspace)) + `</span>`)
	}
	if s.Branch != "" {
		b.WriteString(` · on <b>` + esc(s.Branch) + `</b> @ ` + esc(s.Head))
	}
	b.WriteString(`</div>`)
	b.WriteString(navBar("mesh"))

	// Connected peers (presence)
	b.WriteString(`<div class="card"><h2>Connected peers <span class="pill">` + esc(fmt.Sprint(len(s.Peers))) + `</span></h2>`)
	if len(s.Peers) == 0 {
		b.WriteString(`<div class="empty">no peer connected right now</div>`)
	} else {
		for _, p := range s.Peers {
			b.WriteString(`<div class="row"><span><span class="ok">●</span> ` + esc(p.User))
			if p.State != "" {
				b.WriteString(` <span class="pill">` + esc(p.State) + `</span>`)
			}
			b.WriteString(`</span><span class="id">` + esc(short(p.ID)))
			if p.Addr != "" {
				b.WriteString(` <span class="dim">` + esc(p.Addr) + `</span>`)
			}
			if p.RTTms > 0 {
				b.WriteString(` <span class="dim">` + esc(fmt.Sprint(p.RTTms)) + `ms</span>`)
			}
			b.WriteString(`</span></div>`)
		}
	}
	b.WriteString(`</div>`)

	// Sync history with changed files
	b.WriteString(`<div class="card"><h2>Recent syncs</h2>`)
	if len(s.Syncs) == 0 {
		b.WriteString(`<div class="empty">nothing synced yet</div>`)
	} else {
		b.WriteString(`<table>`)
		for i := len(s.Syncs) - 1; i >= 0 && i >= len(s.Syncs)-12; i-- {
			e := s.Syncs[i]
			dir := "←"
			if e.Outbound {
				dir = "→"
			}
			who := e.User
			if who == "" {
				who = short(e.Peer)
			}
			b.WriteString(`<tr><td class="t">` + esc(clock(e.Time)) + `</td><td>` + dir + ` ` + esc(who) + ` ` + esc(e.Message))
			if len(e.Files) > 0 {
				b.WriteString(`<div class="dim">` + esc(strings.Join(e.Files, "  ")) + `</div>`)
			}
			b.WriteString(`</td><td class="n">fetched ` + esc(fmt.Sprint(e.Fetched)) + `, pushed ` + esc(fmt.Sprint(e.Pushed)) + `</td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	b.WriteString(`</div>`)

	// Refusals
	b.WriteString(`<div class="card"><h2>Refused</h2>`)
	if len(s.Rejects) == 0 {
		b.WriteString(`<div class="empty">nothing has been turned away</div>`)
	} else {
		b.WriteString(`<table>`)
		for i := len(s.Rejects) - 1; i >= 0 && i >= len(s.Rejects)-8; i-- {
			e := s.Rejects[i]
			b.WriteString(`<tr><td class="t">` + esc(clock(e.Time)) + `</td><td>`)
			if e.Peer != "" {
				b.WriteString(`<span class="id">` + esc(short(e.Peer)) + `</span> `)
			}
			b.WriteString(`<span class="bad">` + esc(e.Reason) + `</span></td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	b.WriteString(`</div>`)

	// Activity feed
	if len(s.Events) > 0 {
		b.WriteString(`<div class="card"><h2>Activity</h2><table>`)
		for i := len(s.Events) - 1; i >= 0 && i >= len(s.Events)-15; i-- {
			e := s.Events[i]
			cls := "dim"
			low := strings.ToLower(e.Text)
			if strings.Contains(low, "refus") || strings.Contains(low, "reject") || strings.Contains(low, "lost") || strings.Contains(low, "mismatch") {
				cls = "warn"
			}
			b.WriteString(`<tr><td class="t">` + esc(clock(e.Time)) + `</td><td class="` + cls + `">` + esc(e.Text) + `</td></tr>`)
		}
		b.WriteString(`</table></div>`)
	}

	// LAN peers, same workspace or not
	if len(s.Known) > 0 {
		b.WriteString(`<div class="card"><h2>On the LAN</h2><table>`)
		rows := append([]PeerView(nil), s.Known...)
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		for _, p := range rows {
			b.WriteString(`<tr><td>` + esc(p.User) + ` <span class="id">` + esc(short(p.ID)) + `</span> `)
			if p.WS == "" || p.WS != s.Workspace {
				b.WriteString(`<span class="pill warn">other workspace</span>`)
			}
			b.WriteString(`</td><td class="n dim">` + esc(p.Addr) + `</td></tr>`)
		}
		b.WriteString(`</table></div>`)
	}

	b.WriteString(`<div class="footer">updated ` + esc(clock(s.Updated)) +
		` · local view only, nothing leaves this machine · <a href="/files/">browse files</a></div>`)
	b.WriteString(`</div></body></html>`)
	return []byte(b.String())
}

// RenderFiles returns the file-browser page for a directory or a file.
func RenderFiles(s Status) []byte {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en"><head>`)
	if s.File != "" {
		b.WriteString(pageHead(path.Base(s.File) + " — LRM"))
	} else {
		b.WriteString(pageHead("files — LRM"))
	}
	b.WriteString(`</head><body><div class="wrap">`)
	b.WriteString(`<h1>` + esc(s.User) + ` / `)
	if s.Branch != "" {
		b.WriteString(`<span class="dim">` + esc(s.Branch) + `</span> `)
	}
	b.WriteString(`<span class="dim">@</span> ` + esc(s.Head) + `</h1>`)
	b.WriteString(`<div class="sub">workspace files · read-only · synced by LRM</div>`)
	b.WriteString(navBar("files"))

	// Breadcrumbs
	b.WriteString(`<div class="crumbs">`)
	for i, c := range s.Parents {
		if i > 0 {
			b.WriteString(` / `)
		}
		if i == len(s.Parents)-1 {
			b.WriteString(`<b>` + esc(c.Name) + `</b>`)
		} else {
			b.WriteString(`<a href="/files/` + esc(c.Path) + `">` + esc(c.Name) + `</a>`)
		}
	}
	if s.File != "" {
		b.WriteString(` / <b>` + esc(path.Base(s.File)) + `</b>`)
	}
	b.WriteString(`</div>`)

	if s.File != "" {
		b.WriteString(`<div class="card"><h2>` + esc(path.Base(s.File)) + ` <span class="pill">` + esc(human(s.FileSize)) + `</span></h2>`)
		switch {
		case s.FileBin:
			b.WriteString(`<div class="empty">binary file — not rendered.</div>`)
		case s.FileBig:
			b.WriteString(`<div class="empty">file is larger than the inline preview limit (` + esc(human(maxPreview)) + `).</div>`)
		default:
			b.WriteString(`<pre>` + esc(s.FileBody) + `</pre>`)
		}
		b.WriteString(`<div>`)
		b.WriteString(`<a href="/raw/` + esc(s.File) + `">download raw</a>`)
		b.WriteString(`</div></div>`)
	} else {
		b.WriteString(`<div class="card"><h2>Files <span class="pill">` + esc(fmt.Sprint(len(s.Entries))) + `</span></h2>`)
		if len(s.Entries) == 0 {
			b.WriteString(`<div class="empty">empty directory</div>`)
		} else {
			b.WriteString(`<table>`)
			for _, e := range s.Entries {
				link := e.Name
				href := "/files/" + esc(joinPath(s.Dir, e.Name))
				if e.IsDir {
					link = e.Name + "/"
					href = "/files/" + esc(joinPath(s.Dir, e.Name)) + "/"
				}
				b.WriteString(`<tr><td><a href="` + href + `">` + esc(link) + `</a></td><td class="sz">`)
				if !e.IsDir {
					b.WriteString(esc(human(e.Size)))
				}
				b.WriteString(`</td><td class="sz dim">` + esc(e.ModTime) + `</td></tr>`)
			}
			b.WriteString(`</table>`)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`<div class="footer">updated ` + esc(clock(s.Updated)) + ` · read-only view of the working tree</div>`)
	b.WriteString(`</div></body></html>`)
	return []byte(b.String())
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// browser serves the read-only file view, strictly confined to root.
type browser struct {
	root string // repo root
}

// cleanRel validates a request path for browsing and returns the slash
// path relative to the repo root. It refuses everything that could leave
// the repo: traversal, absolute paths, control characters, and the repo's
// own .lrm directory (identity.key lives there).
func (fb *browser) cleanRel(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	clean, err := merkle.CleanTreePath(p)
	if err != nil {
		return "", err
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".lrm" {
			return "", fmt.Errorf("the .lrm directory is not browsable")
		}
	}
	return clean, nil
}

// resolve maps a validated relative path to an absolute path inside root,
// refusing symlinks that point out of the repository.
func (fb *browser) resolve(rel string) (string, error) {
	abs := filepath.Join(fb.root, filepath.FromSlash(rel))
	// Resolve symlinks on the deepest existing ancestor and require the
	// result to stay inside the repo: a synced symlink must not become a
	// window onto the rest of the disk.
	probe := abs
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	realRoot, err := filepath.EvalSymlinks(fb.root)
	if err != nil {
		return "", err
	}
	realProbe, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	if realProbe != realRoot && !strings.HasPrefix(realProbe, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q leaves the repository", rel)
	}
	return abs, nil
}

func (fb *browser) list(rel string) ([]FileView, error) {
	abs, err := fb.resolve(rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]FileView, 0, len(des))
	for _, d := range des {
		name := d.Name()
		if name == ".lrm" {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		fv := FileView{Name: name, IsDir: d.IsDir(), ModTime: info.ModTime().Format("15:04 Jan 2")}
		if !d.IsDir() {
			fv.Size = info.Size()
		}
		out = append(out, fv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (fb *browser) preview(rel string) (body string, size int64, big, bin bool, err error) {
	abs, err := fb.resolve(rel)
	if err != nil {
		return "", 0, false, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, false, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, false, false, err
	}
	if info.IsDir() {
		return "", 0, false, false, fmt.Errorf("is a directory")
	}
	size = info.Size()
	buf := make([]byte, maxPreview)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", size, false, false, err
	}
	buf = buf[:n]
	if strings.IndexByte(string(buf), 0) >= 0 {
		return "", size, false, true, nil
	}
	if size > maxPreview {
		return "", size, true, false, nil
	}
	return string(buf), size, false, false, nil
}

// handler wires the mesh page, the file browser, and the raw download.
func handler(statusPath string, root string, s Refresh) http.Handler {
	fb := &browser{root: root}
	mux := http.NewServeMux()

	readOnly := func(w http.ResponseWriter) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
	}

	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := s()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		rel, err := fb.cleanRel(strings.TrimPrefix(r.URL.Path, "/files/"))
		if err != nil {
			http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
			return
		}
		abs, err := fb.resolve(rel)
		if err != nil {
			http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Breadcrumbs
		st.Parents = []Crumb{{Name: "files", Path: ""}}
		acc := ""
		for _, seg := range strings.Split(rel, "/") {
			if seg == "" {
				continue
			}
			acc = joinPath(acc, seg)
			st.Parents = append(st.Parents, Crumb{Name: seg, Path: acc})
		}
		if info.IsDir() {
			st.Dir = rel
			entries, err := fb.list(rel)
			if err != nil {
				http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
				return
			}
			st.Entries = entries
			readOnly(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(RenderFiles(st))
			return
		}
		body, size, big, bin, err := fb.preview(rel)
		if err != nil {
			http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
			return
		}
		st.File, st.FileBody, st.FileSize, st.FileBig, st.FileBin = rel, body, size, big, bin
		readOnly(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(RenderFiles(st))
	})

	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel, err := fb.cleanRel(strings.TrimPrefix(r.URL.Path, "/raw/"))
		if err != nil {
			http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
			return
		}
		abs, err := fb.resolve(rel)
		if err != nil {
			http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, err := os.Open(abs)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Always served as an opaque download: a synced .html file must
		// never be rendered as a page on this origin.
		readOnly(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(path.Base(rel), `"`, "")+`"`)
		w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
		_, _ = io.Copy(w, f) // streams; no buffering of the whole file
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		st, err := s()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		readOnly(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(Render(st))
	})
	return mux
}

// Refresh supplies the current status (publishing live state on each view).
type Refresh func() (Status, error)

// LoadRefresh returns a Refresh that reads the status file and refreshes
// the live timestamps. Views that need workspace files (branch, head) get
// them from the same snapshot the daemon publishes.
func LoadRefresh(statusPath string) Refresh {
	return func() (Status, error) { return Load(statusPath) }
}

// Handler serves the dashboard. It is read-only: GET / the mesh view,
// GET /files/ the browser, GET /raw/ a download. root is the repository
// root that the browser is confined to. auto adds a refresh header so a
// served page follows the daemon (skipped for on-disk snapshots).
func Handler(statusPath, root string, refresh Refresh, auto bool) http.Handler {
	s := refresh
	if s == nil {
		s = LoadRefresh(statusPath)
	}
	h := handler(statusPath, root, s)
	if !auto {
		return h
	}
	return autoRefresh(h)
}

// autoRefresh wraps a handler, injecting a 3-second refresh into served
// HTML pages so the view follows the daemon without a script.
func autoRefresh(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &respProxy{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		rec.flush()
	})
}

type respProxy struct {
	http.ResponseWriter
	status int
	body   []byte
	header http.Header
	wrote  bool
}

func (p *respProxy) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
		for k, v := range p.ResponseWriter.Header() {
			p.header[k] = v
		}
	}
	return p.header
}

func (p *respProxy) WriteHeader(code int) {
	if p.wrote {
		return
	}
	p.status, p.wrote = code, true
}

func (p *respProxy) Write(b []byte) (int, error) {
	if !p.wrote {
		p.wrote = true
	}
	p.body = append(p.body, b...)
	return len(b), nil
}

// flush sends the buffered response, injecting the refresh meta tag into
// HTML bodies only.
func (p *respProxy) flush() {
	h := p.Header()
	final := p.body
	if strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		marker := []byte(`<meta charset="utf-8">`)
		final = replaceOnce(final, marker, append(append([]byte{}, marker...), []byte(`<meta http-equiv="refresh" content="3">`)...))
	}
	for k, vs := range h {
		for _, v := range vs {
			p.ResponseWriter.Header().Set(k, v)
		}
	}
	if h.Get("Content-Length") != "" {
		p.ResponseWriter.Header().Del("Content-Length")
	}
	p.ResponseWriter.WriteHeader(p.status)
	_, _ = p.ResponseWriter.Write(final)
}

func replaceOnce(b, old, new []byte) []byte {
	i := strings.Index(string(b), string(old))
	if i < 0 {
		return b
	}
	out := make([]byte, 0, len(b)+len(new)-len(old))
	out = append(out, b[:i]...)
	out = append(out, new...)
	out = append(out, b[i+len(old):]...)
	return out
}
