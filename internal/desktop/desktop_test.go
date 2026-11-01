package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRunner records the argv the app would have executed and returns canned
// output. The panel must never invent its own share/clone logic, so these
// assertions are about ARGUMENTS — that is the contract with the CLI.
type fakeRunner struct {
	got  [][]string
	envs [][]string
	out  string
	err  error
}

func (f *fakeRunner) Run(_ context.Context, env []string, args ...string) (string, error) {
	f.got = append(f.got, args)
	f.envs = append(f.envs, env)
	if f.out == "" {
		f.out = "ok"
	}
	return f.out, f.err
}

func newTestService(t *testing.T) (*Service, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{}
	return &Service{
		Env:    Env{Bin: "/nonexistent/lrm", RepoDir: t.TempDir(), Home: t.TempDir(), Version: "v1.2.3"},
		Runner: fr,
	}, fr
}

func TestInitValidatesInput(t *testing.T) {
	svc, fr := newTestService(t)
	if _, err := svc.Init(context.Background(), "", "ada"); err == nil {
		t.Fatal("empty folder must be refused")
	}
	if _, err := svc.Init(context.Background(), t.TempDir(), "  "); err == nil {
		t.Fatal("empty name must be refused — commits are signed with it")
	}
	if len(fr.got) != 0 {
		t.Fatalf("invalid input still reached the CLI: %v", fr.got)
	}
}

func TestInitRunsCLIInChosenFolder(t *testing.T) {
	svc, fr := newTestService(t)
	dir := filepath.Join(t.TempDir(), "project")
	if _, err := svc.Init(context.Background(), dir, "ada"); err != nil {
		t.Fatal(err)
	}
	want := []string{"-C", dir, "init", "--user", "ada"}
	if strings.Join(fr.got[0], " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", fr.got[0], want)
	}
	if svc.Env.RepoDir != dir {
		t.Fatalf("RepoDir = %q, want %q", svc.Env.RepoDir, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("folder was not created: %v", err)
	}
}

func TestClonePassesKeyDirAndUser(t *testing.T) {
	svc, fr := newTestService(t)
	dir := filepath.Join(t.TempDir(), "their-project")
	key := "[192.168.1.20]:[8443]"
	if _, err := svc.Clone(context.Background(), key, dir, "ada"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fr.got[0], " ")
	for _, want := range []string{"clone", key, dir, "--user ada"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
	if _, err := svc.Clone(context.Background(), "", dir, "ada"); err == nil {
		t.Fatal("empty key must be refused before spawning anything")
	}
}

func TestShareUsesLanFlag(t *testing.T) {
	svc, fr := newTestService(t)
	if _, err := svc.Share(context.Background(), true, 8443); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fr.got[0], " "); !strings.Contains(got, "--lan") || !strings.Contains(got, "--port 8443") {
		t.Fatalf("argv = %q, want --lan --port 8443", got)
	}
	if _, err := svc.Share(context.Background(), false, 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fr.got[1], " "); strings.Contains(got, "--lan") {
		t.Fatalf("public share must not pass --lan: %q", got)
	}
}

// The panel is local-only: a request whose Host is not loopback must be
// refused, or any web page could drive it via DNS rebinding.
func TestHostGuardRefusesForeignHosts(t *testing.T) {
	svc, _ := newTestService(t)
	srv := NewServer(svc, "tok")
	h := srv.Handler()

	for _, host := range []string{"evil.example.com", "192.168.1.5:7717"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("host %q got %d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:7717", "localhost:7717"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q got %d, want 200", host, rec.Code)
		}
	}
}

func TestActionRequiresToken(t *testing.T) {
	svc, fr := newTestService(t)
	srv := NewServer(svc, "secret")
	h := srv.Handler()

	form := url.Values{"action": {"sync"}}
	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing token got %d, want 403", rec.Code)
	}
	if len(fr.got) != 0 {
		t.Fatalf("unauthenticated request reached the CLI: %v", fr.got)
	}
}

func TestActionRoutesToCLI(t *testing.T) {
	svc, fr := newTestService(t)
	srv := NewServer(svc, "secret")
	h := srv.Handler()

	form := url.Values{"t": {"secret"}, "action": {"user"}, "user": {"grace"}}
	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303 redirect (result: %s)", rec.Code, rec.Body.String())
	}
	if got := strings.Join(fr.got[0], " "); !strings.Contains(got, "config user grace") {
		t.Fatalf("argv = %q, want config user grace", got)
	}
}

func TestPageEscapesUserContent(t *testing.T) {
	st := State{Version: "v1", Home: "/h", Repo: RepoInfo{Exists: true, User: `<script>alert(1)</script>`,
		Workspace: "abcdef", PeerID: "aa", Path: "/p"}}
	body := string(renderPage(st, "tok", nil))
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("user-controlled name was rendered as HTML")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("name should appear escaped")
	}
}

func TestFirstRunPageOffersCreateAndJoin(t *testing.T) {
	body := string(renderPage(State{Version: "v1", Home: "/h", Repo: RepoInfo{Path: "/empty"}}, "tok", nil))
	for _, want := range []string{"Create one here", "Join a teammate", `value="init"`, `value="clone"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("first-run page missing %q", want)
		}
	}
	// A first-run page must not offer "share" — there is nothing to share yet.
	if strings.Contains(body, "Same Wi-Fi key") {
		t.Fatal("share offered before a workspace exists")
	}
}

// The daemon liveness check must be honest: a pid file pointing at a dead
// process is not "running", and stopping must actually stop it.
func TestDaemonLivenessAndStop(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	svc, _ := newTestService(t)
	if err := os.MkdirAll(filepath.Join(svc.Env.RepoDir, ".lrm"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Dead pid: not running.
	if err := os.WriteFile(svc.daemonPidFile(), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if svc.Daemon().Running {
		t.Fatal("a dead pid was reported as a running daemon")
	}

	// Live process recorded in the pid file: running, then stoppable.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := os.WriteFile(svc.daemonPidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if !svc.Daemon().Running {
		t.Fatal("live process not reported as running")
	}
	if _, err := svc.StopDaemon(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if svc.Daemon().Running {
		t.Fatal("daemon still reported running after stop")
	}
	if _, err := os.Stat(svc.daemonPidFile()); !os.IsNotExist(err) {
		t.Fatal("pid file left behind after stop")
	}
}

func TestStartDaemonRefusesWithoutWorkspace(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.StartDaemon(0); err == nil {
		t.Fatal("starting a daemon with no workspace must fail with a clear message")
	} else if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestDoctorReportsMissingWorkspace(t *testing.T) {
	svc, _ := newTestService(t)
	var found bool
	for _, c := range svc.Doctor() {
		if c.Name == "Workspace" {
			found = true
			if c.OK {
				t.Fatal("doctor claims a workspace exists in an empty dir")
			}
		}
	}
	if !found {
		t.Fatal("doctor has no workspace check")
	}
}

func TestReadTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	_ = os.WriteFile(p, []byte("1\n2\n3\n4\n"), 0o644)
	got := readTail(p, 2)
	if len(got) != 2 || got[0] != "3" || got[1] != "4" {
		t.Fatalf("tail = %v", got)
	}
	_ = os.WriteFile(p, []byte("short\n"), 0o644)
	if got := readTail(p, 10); len(got) != 1 {
		t.Fatalf("tail of short file = %v", got)
	}
}

func TestDaemonInfoSurvivesStalePidFile(t *testing.T) {
	svc, _ := newTestService(t)
	_ = os.MkdirAll(filepath.Join(svc.Env.RepoDir, ".lrm"), 0o755)
	_ = os.WriteFile(svc.daemonPidFile(), []byte("not-a-number"), 0o644)
	if info := svc.Daemon(); info.Running {
		t.Fatalf("garbage pid file reported running: %+v", info)
	}
	time.Sleep(1 * time.Millisecond)
}
