package send

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/store"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
)

const wsA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func mkRepo(t *testing.T, user, ws string) *store.Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	r, err := store.InitWithWorkspace(dir, user, 0, ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// serveOnce accepts one connection, reads the first message, and serves
// it as a send offer (the daemon's dispatch, distilled).
func serveOnce(t *testing.T, ln *transport.Listener, r *store.Repo, out chan<- *Result) {
	t.Helper()
	sc, err := ln.Accept()
	if err != nil {
		return
	}
	defer sc.Close()
	sess := mux.NewSession(sc, false)
	defer sess.Close()
	st, err := sess.AcceptStream()
	if err != nil {
		return
	}
	m, err := lrmsync.ReadMsg(st)
	if err != nil {
		return
	}
	if m.Fam != Fam {
		return
	}
	res, err := Serve(st, m, r)
	if err != nil {
		t.Errorf("Serve: %v", err)
	}
	out <- res
}

func TestSendRoundTrip(t *testing.T) {
	recv := mkRepo(t, "robert", wsA)
	sendr := mkRepo(t, "sara", wsA)

	ln, err := transport.Listen("127.0.0.1:0", sendr.Identity, sendr.Identity.Pub)
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer ln.Close()
	out := make(chan *Result, 1)
	go serveOnce(t, ln, recv, out)

	src := filepath.Join(t.TempDir(), "deck.pdf")
	payload := []byte("%PDF-1.4 lrm send test payload — not really a pdf but who's checking\n")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc, err := transport.Dial(ctx, ln.Addr().String(), recv.Identity, recv.Identity.Pub, sendr.Identity.PeerID[:])
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	sess := mux.NewSession(sc, true)
	defer sess.Close()

	res, err := SendFile(sess, src, wsA)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Fatalf("size %d want %d", res.Size, len(payload))
	}
	got, err := os.ReadFile(filepath.Join(recv.Root, "inbox", "deck.pdf"))
	if err != nil {
		t.Fatalf("receiver file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload corrupted")
	}

	// Collision: same file again -> deck-1.pdf, original untouched.
	out2 := make(chan *Result, 1)
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			return
		}
		defer sc.Close()
		sess := mux.NewSession(sc, false)
		st, _ := sess.AcceptStream()
		m, err := lrmsync.ReadMsg(st)
		if err != nil {
			return
		}
		res, err := Serve(st, m, recv)
		if err != nil {
			t.Errorf("Serve2: %v", err)
		}
		out2 <- res
		_ = sess.Close()
	}()
	sc2, err := transport.Dial(ctx, ln.Addr().String(), recv.Identity, recv.Identity.Pub, sendr.Identity.PeerID[:])
	if err != nil {
		t.Fatal(err)
	}
	sess2 := mux.NewSession(sc2, true)
	res2, err := SendFile(sess2, src, wsA)
	if err != nil {
		t.Fatalf("SendFile2: %v", err)
	}
	if res2.Name != "deck-1.pdf" {
		t.Fatalf("collision name %q want deck-1.pdf", res2.Name)
	}
	if _, err := os.Stat(filepath.Join(recv.Root, "inbox", "deck-1.pdf")); err != nil {
		t.Fatalf("collision file missing: %v", err)
	}
	_ = sess2.Close()
	_ = sc2.Close()
}

func TestSendWorkspaceRefused(t *testing.T) {
	recv := mkRepo(t, "robert", wsA)
	stranger := mkRepo(t, "mallory", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	ln, err := transport.Listen("127.0.0.1:0", stranger.Identity, stranger.Identity.Pub)
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer ln.Close()
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			return
		}
		defer sc.Close()
		sess := mux.NewSession(sc, false)
		st, _ := sess.AcceptStream()
		m, err := lrmsync.ReadMsg(st)
		if err != nil {
			return
		}
		_, _ = Serve(st, m, recv) // must refuse
		_ = sess.Close()
	}()

	src := filepath.Join(t.TempDir(), "evil.txt")
	if err := os.WriteFile(src, []byte("hello robert"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc, err := transport.Dial(ctx, ln.Addr().String(), recv.Identity, recv.Identity.Pub, stranger.Identity.PeerID[:])
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	sess := mux.NewSession(sc, true)
	defer sess.Close()
	if _, err := SendFile(sess, src, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("stranger workspace send must be refused")
	}
	if _, err := os.Stat(filepath.Join(recv.Root, "inbox", "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("no file may land on refusal")
	}
}
