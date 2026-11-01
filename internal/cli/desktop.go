package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lrm-project/lrm/internal/desktop"
	"github.com/lrm-project/lrm/internal/update"
)

// cmdDesktop opens the app: a local panel for this workspace plus a
// self-updater.
//
// It behaves like a GUI launcher, not another subcommand:

//   - no repository yet? It does not refuse. It opens the panel showing
//     "create" and "join", which is the whole first-run story.
//   - it never requires a browser to be installable, and the URL is printed so
//     a headless box can still be driven (port-forward the port yourself).
//   - the daemon it starts is detached: closing the panel does not stop sync.
func cmdDesktop(args []string) error {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	port := fs.Int("port", 7717, "panel port on 127.0.0.1")
	dir := fs.String("dir", "", "workspace to manage (default: current directory)")
	noOpen := fs.Bool("no-open", false, "print the URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil // --help already printed usage; not a failure
		}
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}

	repoDir := *dir
	if repoDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			repoDir = cwd
		}
		// Double-clicked from Windows Explorer or a desktop shortcut: the
		// working directory is wherever the shortcut points (often
		// C:\Windows\System32 or the Downloads folder), which is never where
		// someone wants a workspace. Fall back to ~/LRM instead of scattering
		// .lrm directories into system folders.
		if !desktop.RepoExists(repoDir) {
			if home, err := os.UserHomeDir(); err == nil {
				if repoDir == filepath.Dir(self) || repoDir == home {
					repoDir = filepath.Join(home, "LRM")
				}
			}
		}
	}
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}

	home := os.Getenv("LRM_HOME")
	if home == "" {
		home = defaultHome()
	}

	svc := &desktop.Service{
		Env:    desktop.Env{Bin: self, RepoDir: repoDir, Home: home, Version: Version},
		Runner: desktop.ExecRunner{Bin: self},
	}

	// Clean up a Windows swap that could not delete the old binary while it
	// was still running.
	update.CleanLeftovers(filepath.Dir(self))

	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	token := hex.EncodeToString(tok)

	srv := desktop.NewServer(svc, token)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	url, err := srv.Serve(ctx, "127.0.0.1", *port)
	if err != nil {
		// A stale panel on the default port is common; say what to do.
		return fmt.Errorf("%w\n  another panel or process is using port %d — try: lrm desktop --port %d", err, *port, *port+1)
	}

	fmt.Printf("LRM %s — panel on %s\n", Version, url)
	fmt.Printf("workspace: %s\n", orNone(svc.Env.RepoDir))
	if !desktop.RepoExists(repoDir) {
		fmt.Println("no workspace here yet — create or join one in the page")
	}
	fmt.Println("Ctrl-C closes the panel (a daemon started from it keeps running)")

	if !*noOpen {
		openBrowser(url)
	}
	<-ctx.Done()
	fmt.Println("\npanel closed")
	return nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func defaultHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".lrm")
	}
	return ".lrm"
}

// openBrowser opens url with the platform's default handler. Failure is not an
// error: the URL is already printed, and a headless machine has no browser to
// open — which is a normal way to run this.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
	go func() {
		time.Sleep(30 * time.Second)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
}
