// Package send implements lrm send — direct file handoff over an
// established LRM session (fam="send" envelope on a fresh mux stream).
//
// The sender streams the raw bytes after a small offer message; the
// receiver (the daemon) verifies size + SHA-256 and drops the file into
// <repo>/inbox/, where the workspace watcher auto-commits it. Like sync,
// send is workspace-scoped: a stranger with a valid key but a different
// workspace is refused before a single byte moves.
package send

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/store"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
)

// Fam is the envelope family for direct file transfer.
const Fam = "send"

// MaxFile caps a single transfer (1 GiB) — sanity bound, not a feature.
const MaxFile = 1 << 30

// Result describes a completed inbound transfer.
type Result struct {
	Name     string // final name in inbox/ (collision-suffixed)
	Size     int64
	Sha256   string
	Relayed  string
	FromPeer string
}

// SendFile offers and streams path to the peer over sess. ws is the
// sender's workspace id (the receiver refuses mismatches).
func SendFile(sess *mux.Session, path, ws string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxFile {
		return nil, fmt.Errorf("file too large for send (%d bytes > %d)", fi.Size(), MaxFile)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	name := filepath.Base(path)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("bad file name")
	}

	st, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	offer := lrmsync.Msg{
		Fam: Fam, Type: "offer",
		Extra: map[string]string{
			"name": name, "size": fmt.Sprint(fi.Size()),
			"sha256": sum, "ws": ws,
		},
	}
	if err := lrmsync.WriteMsg(st, offer); err != nil {
		return nil, err
	}
	// Reply gate: the receiver may refuse (workspace mismatch) before
	// we stream anything.
	m, err := lrmsync.ReadMsg(st)
	if err != nil {
		return nil, err
	}
	if m.Type == "error" {
		return nil, fmt.Errorf("remote refused: %s", m.Error)
	}
	if m.Type != "ready" {
		return nil, fmt.Errorf("unexpected send reply %q", m.Type)
	}
	// Length-prefixed raw bytes.
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(fi.Size()))
	if _, err := st.Write(lenBuf[:]); err != nil {
		return nil, err
	}
	if _, err := io.Copy(st, f); err != nil {
		return nil, err
	}
	done, err := lrmsync.ReadMsg(st)
	if err != nil {
		return nil, err
	}
	if done.Type == "error" {
		return nil, fmt.Errorf("remote rejected: %s", done.Error)
	}
	if done.Type != "done" {
		return nil, fmt.Errorf("unexpected send result %q", done.Type)
	}
	return &Result{Name: done.Extra["name"], Size: fi.Size(), Sha256: sum}, nil
}

// Serve handles an inbound fam=send offer on st: gates the workspace,
// streams the file to a temp store, verifies the hash, and places it in
// <repo>/inbox/ (collision-suffixed). The workspace watcher commits it.
func Serve(st *mux.Stream, m lrmsync.Msg, r *store.Repo) (*Result, error) {
	if m.Fam != Fam || m.Type != "offer" {
		return nil, fmt.Errorf("not a send offer")
	}
	name := filepath.Base(m.Extra["name"])
	if name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("bad file name")
	}
	var size int64
	if _, err := fmt.Sscanf(m.Extra["size"], "%d", &size); err != nil || size < 0 || size > MaxFile {
		return nil, fmt.Errorf("bad size")
	}
	wantSha := m.Extra["sha256"]
	// Workspace gate: same mesh-scoping rule as sync — strangers with a
	// different workspace never move bytes into this repo.
	if ws := m.Extra["ws"]; ws != "" && ws != r.Config.Workspace {
		_ = lrmsync.WriteMsg(st, lrmsync.Msg{Fam: Fam, Type: "error",
			Error: "workspace mismatch: cannot send into this workspace"})
		return nil, fmt.Errorf("workspace mismatch")
	}
	if err := lrmsync.WriteMsg(st, lrmsync.Msg{Fam: Fam, Type: "ready"}); err != nil {
		return nil, err
	}

	var lenBuf [8]byte
	if _, err := io.ReadFull(st, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int64(binary.BigEndian.Uint64(lenBuf[:]))
	if n != size || n > MaxFile {
		return nil, fmt.Errorf("size mismatch (offer %d, stream %d)", size, n)
	}
	inbox := filepath.Join(r.Root, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(inbox, ".send-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(st, n)); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantSha {
		return nil, fmt.Errorf("hash mismatch (want %s got %s)", wantSha[:12], got[:12])
	}
	final := filepath.Join(inbox, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		final = filepath.Join(inbox, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
	if err := os.Rename(tmpName, final); err != nil {
		return nil, err
	}
	_ = lrmsync.WriteMsg(st, lrmsync.Msg{Fam: Fam, Type: "done",
		Extra: map[string]string{"name": filepath.Base(final)}})
	return &Result{Name: filepath.Base(final), Size: n, Sha256: got}, nil
}
