package cli

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/lrm-project/lrm/internal/drop"
	"github.com/lrm-project/lrm/internal/mdns"
)

// cmdReceive starts a phone-friendly upload page: any device on the same
// Wi-Fi opens the printed link in a browser (no app, no install) and sends
// files, which land in the repository's inbox/ and are committed and synced
// like any other change.
//
// Safety: the link carries a random token, uploads can only ever create
// files inside inbox/, names are reduced to safe basenames, size and count
// are capped, and --once shuts the door after the first successful upload.
func cmdReceive(args []string) error {
	portStr, args := flagVal(args, "--port", "-p")
	host, args := flagVal(args, "--host")
	sizeStr, args := flagVal(args, "--max-size")
	once, _ := hasFlag(args, "--once")
	dir, _ := flagVal(args, "--dir")

	r, err := openRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inbox := filepath.Join(r.Root, "inbox")
	if dir != "" {
		inbox = dir
	}
	maxBytes := int64(2 << 30)
	if sizeStr != "" {
		mb, err := strconv.Atoi(sizeStr)
		if err != nil || mb <= 0 {
			return fmt.Errorf("--max-size takes MiB, e.g. --max-size 500")
		}
		maxBytes = int64(mb) << 20
	}
	if host == "" {
		host = "0.0.0.0" // reachable from the phone; token still required
	}
	port := 8788
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("--port takes a port number")
		}
		port = n
	}

	srv, url, err := drop.Start(drop.Options{
		Inbox:    inbox,
		TmpDir:   filepath.Join(r.LrmDir, "tmp"), // never inside the watched tree
		Host:     host,
		Port:     port,
		Hostname: firstLANIP(),
		MaxBytes: maxBytes,
		Once:     once,
		Logf: func(format string, a ...any) {
			fmt.Printf("  "+format+"\n", a...)
		},
		OnResult: func(path string, size int64) {
			fmt.Printf("  -> %s\n", path)
		},
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	fmt.Println("LRM receive — send files from any device on this Wi-Fi")
	fmt.Println()
	fmt.Printf("  open this on the phone:   %s\n", url)
	fmt.Printf("  saving into:              %s\n", inbox)
	if once {
		fmt.Println("  mode:                     one upload, then the server stops")
	}
	fmt.Println()
	fmt.Println("The link carries a secret token — anyone who has it can send files")
	fmt.Println("(nothing is ever readable: this door only takes files in).")
	fmt.Println("The daemon commits inbox/ automatically, so uploads sync on.")
	fmt.Println("Ctrl-C to stop.")
	srv.Wait()
	return nil
}

// firstLANIP is the address the phone should use (first non-loopback IPv4).
func firstLANIP() string {
	addrs := mdns.LocalAddrs()
	if len(addrs) > 0 {
		return addrs[0]
	}
	return "localhost"
}
