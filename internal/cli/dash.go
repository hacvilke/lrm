package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/daemon"
	"github.com/lrm-project/lrm/internal/dash"
)

// cmdDashboard shows this workspace locally: the live mesh (who is
// connected, what synced, what was refused) and the workspace files, in a
// browser, like a repository page — except it is one process on your own
// machine, reading your own working tree.
//
// It is a window, not a service: localhost by default, read-only, no
// network calls of its own, and the file view is confined to the
// repository root.
func cmdDashboard(args []string) error {
	portStr, args := flagVal(args, "--port", "-p")
	host, args := flagVal(args, "--host")
	snap, _ := flagVal(args, "--snapshot")
	asJSON, _ := hasFlag(args, "--json")

	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	statusPath := filepath.Join(r.LrmDir, daemon.StatusFileName)

	if asJSON {
		raw, err := os.ReadFile(statusPath)
		if err != nil {
			fmt.Println(`{"error":"no status file yet — run ` + "`lrm daemon`" + ` first"}`)
			return nil
		}
		_, _ = os.Stdout.Write(raw)
		fmt.Println()
		return nil
	}

	// One snapshot is enough for every page: the status file holds the
	// daemon's view, and the file browser reads the working tree directly.
	refresh := func() (dash.Status, error) {
		st, err := dash.Load(statusPath)
		if err != nil {
			return st, err
		}
		// When no daemon is running the page still works: fill the local
		// facts we can see for ourselves.
		if st.User == "" {
			st.User = r.Config.User
			st.PeerID = r.Identity.HexID()
			st.Workspace = r.Config.Workspace
		}
		if st.Branch == "" {
			if br, err := r.HeadBranch(); err == nil {
				st.Branch = br
			}
		}
		if st.Head == "" {
			if h, _, err := r.HeadCommit(); err == nil {
				st.Head = cas.Short(h)
			}
		}
		return st, nil
	}

	if snap != "" {
		st, err := refresh()
		if err != nil {
			return err
		}
		page := dash.Render(st)
		if err := os.WriteFile(snap, page, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", snap, len(page))
		fmt.Println("tip: `lrm dashboard` serves the live view, including the file browser")
		return nil
	}

	if host == "" {
		host = "127.0.0.1" // local view: do not expose on the LAN
	}
	port := 8787
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	if _, err := os.Stat(statusPath); err != nil {
		fmt.Println("note: no status file yet — start `lrm daemon` to see live peers")
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	fmt.Printf("LRM dashboard   http://%s\n", addr)
	fmt.Printf("  mesh   http://%s/        peers, syncs, activity\n", addr)
	fmt.Printf("  files  http://%s/files/  browse the workspace (read-only)\n", addr)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		fmt.Println("warning: bound beyond localhost — anyone who can reach this port sees your workspace files")
	}
	fmt.Println("Ctrl-C to stop.")
	srv := &http.Server{
		Addr:              addr,
		Handler:           dash.Handler(statusPath, r.Root, refresh, true),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
