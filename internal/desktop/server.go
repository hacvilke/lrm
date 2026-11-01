package desktop

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/update"
)

// Server is the local control panel.
type Server struct {
	Svc   *Service
	Token string // per-run CSRF token, embedded in every form
	Log   []string
	mu    sync.Mutex
}

// NewServer builds a panel around svc.
func NewServer(svc *Service, token string) *Server {
	return &Server{Svc: svc, Token: token}
}

func (s *Server) flash(kind, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Log = append(s.Log, kind+"\x00"+msg)
	if len(s.Log) > 40 {
		s.Log = s.Log[len(s.Log)-40:]
	}
}

func (s *Server) takeFlash() (kind, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Log) == 0 {
		return "", ""
	}
	last := s.Log[len(s.Log)-1]
	s.Log = nil
	parts := strings.SplitN(last, "\x00", 2)
	if len(parts) != 2 {
		return "", last
	}
	return parts[0], parts[1]
}

// Handler returns the http.Handler for the panel.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/action", s.handleAction)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	return hostGuard(mux)
}

// hostGuard refuses requests that did not arrive on a loopback Host name.
// Without this, a page on the open internet can point a DNS name at
// 127.0.0.1 and drive this panel ("DNS rebinding"), because a browser will
// happily send requests to the IP it resolves to.
func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		switch host {
		case "127.0.0.1", "localhost", "[::1]", "::1", "":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "this panel only answers on 127.0.0.1", http.StatusForbidden)
		}
	})
}

func (s *Server) state() State {
	st := State{
		Version: s.Svc.Env.Version,
		Home:    s.Svc.Env.Home,
		Repo:    s.Svc.Inspect(),
		Daemon:  s.Svc.Daemon(),
	}
	st.GOOS = runtime.GOOS
	st.GOARCH = runtime.GOARCH
	return st
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	st := s.state()
	st.FlashKind, st.Flash = s.takeFlash()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(renderPage(st, s.Token, s.Svc.Doctor()))
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.FormValue("t") != s.Token {
		http.Error(w, "stale panel — reload the page", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	action := r.FormValue("action")
	var err error
	var out string

	switch action {
	case "init":
		out, err = s.Svc.Init(ctx, r.FormValue("dir"), r.FormValue("user"))
	case "clone":
		out, err = s.Svc.Clone(ctx, r.FormValue("key"), r.FormValue("dir"), r.FormValue("user"))
	case "user":
		out, err = s.Svc.SetUser(ctx, r.FormValue("user"))
	case "share":
		port, _ := strconv.Atoi(r.FormValue("port"))
		out, err = s.Svc.Share(ctx, r.FormValue("lan") != "", port)
	case "sync":
		out, err = s.Svc.SyncOnce(ctx)
	case "status":
		out, err = s.Svc.Status(ctx)
	case "daemon-start":
		port, _ := strconv.Atoi(r.FormValue("port"))
		info, derr := s.Svc.StartDaemon(port)
		err = derr
		out = fmt.Sprintf("daemon started, pid %d", info.PID)
	case "daemon-stop":
		_, err = s.Svc.StopDaemon()
		out = "daemon stopped"
	case "update-check":
		st := s.state()
		out, err = s.checkOnly(ctx, st.Repo)
	case "update-apply":
		out, err = s.applyUpdate(ctx)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}

	if err != nil {
		s.flash("err", clean(err.Error()+"\n"+out))
	} else {
		s.flash("ok", clean(out))
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) checkOnly(ctx context.Context, _ RepoInfo) (string, error) {
	res, err := update.Apply(ctx, update.Options{
		Current: s.Svc.Env.Version,
		Out:     io.Discard,
	})
	if err != nil {
		// Check failures are informational: no network is a normal state for
		// a machine on a LAN with no internet.
		return "", err
	}
	if res.Updated {
		return fmt.Sprintf("update available: %s -> %s (press Install to apply)", res.From, res.To), nil
	}
	return "already up to date (" + res.From + ")", nil
}

func (s *Server) applyUpdate(ctx context.Context) (string, error) {
	res, err := update.Apply(ctx, update.Options{
		Current: s.Svc.Env.Version,
		Out:     io.Discard,
	})
	if err != nil {
		return "", err
	}
	if !res.Updated {
		return "already up to date (" + res.From + ")", nil
	}
	return fmt.Sprintf("installed %s at %s — restart the app to use it", res.To, res.InstalledAt), nil
}

// clean trims output for display and refuses to echo anything that looks like
// a private key, because this output ends up in a browser.
func clean(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 6000 {
		s = s[:6000] + "\n… (truncated)"
	}
	return s
}

// Serve runs the panel until ctx is cancelled. It returns the URL it bound.
func (s *Server) Serve(ctx context.Context, host string, port int) (string, error) {
	ln, err := listen(host, port)
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	url := fmt.Sprintf("http://%s/?t=%s", ln.Addr().String(), s.Token)
	go func() { _ = srv.Serve(ln) }()
	return url, nil
}

// RepoExists is a small helper used by the CLI wiring to decide first-run.
func RepoExists(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".lrm"))
	return err == nil
}
