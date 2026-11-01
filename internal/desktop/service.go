// Package desktop is LRM's app: a local control panel plus a self-updater.
//
// It is deliberately NOT a reimplementation of the CLI. Every operation that
// touches keys, the mesh or the network runs the same `lrm` binary the user
// typed — the app runs itself with a different subcommand and shows what it
// printed. There is exactly one implementation of share/join/clone/daemon,
// which is the only way a UI can stay honest as the protocol changes.
//
// The panel is local-only:
//   - bound to 127.0.0.1, never 0.0.0.0
//   - Host header checked, so DNS rebinding from a web page cannot reach it
//   - every mutating form carries a per-run CSRF token
//   - it refuses to display .lrm/ contents (identity.key lives there)
package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/identity"
	"github.com/lrm-project/lrm/internal/store"
)

// Env is everything the service needs to know about where it runs.
type Env struct {
	Bin     string // path to the lrm binary (usually os.Executable())
	RepoDir string // repo to manage; may not exist yet (first run)
	Home    string // LRM_HOME
	Version string
}

// Runner executes the lrm binary. Swapped in tests.
type Runner interface {
	Run(ctx context.Context, env []string, args ...string) (string, error)
}

// ExecRunner is the real one.
type ExecRunner struct{ Bin string }

// Run executes bin with args and returns combined output.
func (e ExecRunner) Run(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// Service is the app's non-HTTP core.
type Service struct {
	Env    Env
	Runner Runner
	// UpdateFn performs the update; injected so tests never hit the network.
	UpdateFn func(ctx context.Context) (string, error)

	mu     sync.Mutex
	daemon *exec.Cmd
}

// RepoInfo describes the workspace the panel is looking at.
type RepoInfo struct {
	Exists        bool
	Path          string
	User          string
	Port          int
	Workspace     string
	PeerID        string
	Fingerprint   string
	HeadBranch    string
	HeadCommit    string
	Commits       int
	NeedsInit     bool
	RefusalReason string
}

// State is what the page renders.
type State struct {
	Version   string
	GOOS      string
	GOARCH    string
	Home      string
	Repo      RepoInfo
	Daemon    DaemonInfo
	LastError string
	Flash     string
	FlashKind string // ok | err
	Update    UpdateInfo
}

// DaemonInfo is the background sync process.
type DaemonInfo struct {
	Running bool
	PID     int
	Port    int
	LogTail []string
}

// UpdateInfo is the self-updater card.
type UpdateInfo struct {
	Current string
	Latest  string
	Checked bool
	Result  string
}

// Inspect reads repo state without changing anything.
func (s *Service) Inspect() RepoInfo {
	info := RepoInfo{Path: s.Env.RepoDir}
	if s.Env.RepoDir == "" {
		info.RefusalReason = "no repository selected"
		return info
	}
	r, err := store.Open(s.Env.RepoDir)
	if err != nil {
		info.NeedsInit = true
		if !errors.Is(err, os.ErrNotExist) {
			info.RefusalReason = err.Error()
		}
		return info
	}
	defer r.Close()
	info.Exists = true
	info.User = r.Config.User
	info.Port = r.Config.Port
	info.Workspace = r.Config.Workspace
	if r.Identity != nil {
		info.PeerID = r.Identity.ShortID()
		info.Fingerprint = identityFingerprint(r.Identity)
	}
	if tip, name, err := r.HeadCommit(); err == nil && tip != cas.Nil {
		info.HeadBranch = name
		info.HeadCommit = shortHex(cas.Hex(tip))
	}
	if n, err := countCommits(r); err == nil {
		info.Commits = n
	}
	return info
}

func countCommits(r *store.Repo) (int, error) {
	// Cheap enough for a panel: walk first-parent from HEAD.
	tip, _, err := r.HeadCommit()
	if err != nil || tip == cas.Nil {
		return 0, err
	}
	n := 0
	cur := tip
	for n < 100000 {
		c, err := r.DAG.Get(cur)
		if err != nil {
			return n, err
		}
		n++
		if len(c.Parents) == 0 {
			return n, nil
		}
		cur, err = cas.ParseHex(c.Parents[0])
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func identityFingerprint(id *identity.Identity) string {
	if id == nil {
		return ""
	}
	hexID := id.HexID()
	if len(hexID) >= 16 {
		return hexID[:4] + " " + hexID[4:8] + " " + hexID[8:12] + " " + hexID[12:16]
	}
	return hexID
}

func shortHex(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// run executes the CLI in the repo directory.
func (s *Service) run(ctx context.Context, args ...string) (string, error) {
	if s.Runner == nil {
		return "", fmt.Errorf("no runner configured")
	}
	env := []string{"LRM_HOME=" + s.Env.Home}
	out, err := s.Runner.Run(ctx, env, args...)
	return out, err
}

// withRepo runs the CLI with an explicit repo path, so the app can act on a
// repo that is not the process's working directory.
func (s *Service) withRepo(ctx context.Context, args ...string) (string, error) {
	all := append([]string{"-C", s.Env.RepoDir}, args...)
	return s.run(ctx, all...)
}

// Init creates a workspace here.
func (s *Service) Init(ctx context.Context, dir, user string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("choose a folder first")
	}
	if strings.TrimSpace(user) == "" {
		return "", fmt.Errorf("a name is required — commits are signed with it")
	}
	abs, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	out, err := s.run(ctx, "-C", abs, "init", "--user", user)
	if err != nil {
		return out, err
	}
	s.Env.RepoDir = abs
	return out, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// Clone joins a teammate's workspace from a Port Key.
func (s *Service) Clone(ctx context.Context, key, dir, user string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("paste the Port Key your teammate gave you")
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("choose a folder to put the workspace in")
	}
	abs, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	args := []string{"clone", key, abs}
	if strings.TrimSpace(user) != "" {
		args = append(args, "--user", strings.TrimSpace(user))
	}
	out, err := s.run(ctx, args...)
	if err != nil {
		return out, err
	}
	s.Env.RepoDir = abs
	return out, nil
}

// SetUser renames the identity used for commits.
func (s *Service) SetUser(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name cannot be empty")
	}
	return s.withRepo(ctx, "config", "user", name)
}

// Share mints a Port Key. lan=true mints the same-Wi-Fi key, which is the one
// that actually dials when both machines are on one router.
func (s *Service) Share(ctx context.Context, lan bool, port int) (string, error) {
	args := []string{"share"}
	if lan {
		args = append(args, "--lan")
	}
	if port > 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	return s.withRepo(ctx, args...)
}

// SyncOnce forces one sync round.
func (s *Service) SyncOnce(ctx context.Context) (string, error) {
	return s.withRepo(ctx, "sync")
}

// Status is the CLI's own status output.
func (s *Service) Status(ctx context.Context) (string, error) {
	return s.withRepo(ctx, "status")
}

// daemonPidFile is where the panel records the daemon it started.
func (s *Service) daemonPidFile() string {
	return filepath.Join(s.Env.RepoDir, ".lrm", "desktop-daemon.pid")
}

func (s *Service) daemonLogFile() string {
	return filepath.Join(s.Env.RepoDir, ".lrm", "desktop-daemon.log")
}

// StartDaemon runs `lrm daemon` in the background, detached from the panel so
// closing the window does not stop sync.
func (s *Service) StartDaemon(port int) (DaemonInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info := s.daemonInfo(); info.Running {
		return info, fmt.Errorf("daemon already running (pid %d)", info.PID)
	}
	if !s.Inspect().Exists {
		return DaemonInfo{}, fmt.Errorf("no workspace here yet — create or clone one first")
	}
	args := []string{"-C", s.Env.RepoDir, "daemon"}
	if port > 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	cmd := exec.Command(s.Env.Bin, args...)
	cmd.Env = append(os.Environ(), "LRM_HOME="+s.Env.Home)
	logPath := s.daemonLogFile()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return DaemonInfo{}, err
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return DaemonInfo{}, err
	}
	logf.Close()
	startedPort := port
	if startedPort <= 0 {
		startedPort = s.Inspect().Port
	}
	_ = os.WriteFile(s.daemonPidFile(), []byte(fmt.Sprintf("%d %d", cmd.Process.Pid, startedPort)), 0o644)
	s.daemon = cmd
	// A daemon that dies instantly (bad flag, workspace mismatch) must not be
	// reported as running — give it a moment, then check.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		_ = os.Remove(s.daemonPidFile())
		tail := readTail(logPath, 8)
		msg := "daemon exited immediately"
		if err != nil {
			msg += ": " + err.Error()
		}
		if len(tail) > 0 {
			msg += "\n" + strings.Join(tail, "\n")
		}
		return DaemonInfo{}, fmt.Errorf("%s", msg)
	case <-time.After(1200 * time.Millisecond):
	}
	go func() { <-done }()
	return s.daemonInfo(), nil
}

// StopDaemon stops the daemon this panel started (or the one in its pid file).
func (s *Service) StopDaemon() (DaemonInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid, _, ok := s.readPid()
	if !ok {
		return s.daemonInfo(), fmt.Errorf("no daemon pid recorded — it was not started from here")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return s.daemonInfo(), err
	}
	if err := terminate(proc); err != nil {
		return s.daemonInfo(), err
	}
	_ = os.Remove(s.daemonPidFile())
	// Wait for the port to actually close, not just for the signal to land.
	for i := 0; i < 40; i++ {
		if !s.daemonInfo().Running {
			return s.daemonInfo(), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return s.daemonInfo(), fmt.Errorf("daemon pid %d did not exit", pid)
}

// daemonInfo reports liveness from the pid file plus a process probe.
func (s *Service) daemonInfo() DaemonInfo {
	// The daemon may listen on a port other than the workspace default
	// (--port), so report the port it was actually started with.
	cfgPort := s.Inspect().Port
	info := DaemonInfo{Port: cfgPort}
	if pid, port, ok := s.readPid(); ok && alive(pid) {
		info.Running = true
		info.PID = pid
		if port > 0 {
			info.Port = port
		}
	}
	info.LogTail = readTail(s.daemonLogFile(), 12)
	return info
}

// Daemon returns current daemon state.
func (s *Service) Daemon() DaemonInfo { return s.daemonInfo() }

// readPid reads "<pid> <port>" from the pid file. A bare pid (older file) is
// still accepted; the port then comes from the workspace config.
func (s *Service) readPid() (pid, port int, ok bool) {
	b, err := os.ReadFile(s.daemonPidFile())
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, 0, false
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	if len(fields) > 1 {
		port, _ = strconv.Atoi(fields[1])
	}
	return pid, port, true
}

func readTail(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Check runs the environment sanity checks the panel shows on first run.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Doctor returns the check list.
func (s *Service) Doctor() []Check {
	var out []Check
	out = append(out, Check{"Operating system", true, runtime.GOOS + "/" + runtime.GOARCH})
	info := s.Inspect()
	out = append(out, Check{"Workspace", info.Exists, orDefault(info.Path, "not created yet")})
	if info.Exists {
		out = append(out, Check{"User name set", info.User != "", orDefault(info.User, "empty — set it below")})
		out = append(out, Check{"Private key present", info.PeerID != "", "peer " + info.PeerID})
		out = append(out, Check{"Port", info.Port > 0, strconv.Itoa(info.Port)})
		out = append(out, Check{"Workspace ID", len(info.Workspace) == 32, orDefault(info.Workspace, "missing")})
	}
	if s.Env.Home != "" {
		out = append(out, Check{"LRM_HOME", true, s.Env.Home})
	}
	return out
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
