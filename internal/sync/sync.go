// Package sync implements the Distributed Collaboration (Sync) Engine.
//
// Responsibilities:
//   - Graph negotiation (have/want) using LCA to compute minimal fetch sets.
//   - Parallel object fetch (commits, trees, blobs, chunks) over mux streams.
//   - Automated conflict branching (HEAD-peer-<short>) on concurrent edits.
//   - Fast-forward + 3-way auto-merge when histories diverge cleanly.
//
// All object transfers stream with bounded buffers — no whole-file reads.
package sync

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/lca"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/replog"
	"github.com/lrm-project/lrm/internal/store"
)

// Msg is a control-stream JSON message.
//
// Envelope: new messages carry Fam (family). Absent fam = "sync" — the
// legacy pre-envelope wire format, still spoken by v1 peers unchanged.
type Msg struct {
	Fam     string            `json:"fam,omitempty"` // message family ("" = sync/legacy)
	Type    string            `json:"type"`
	PV      int               `json:"pv,omitempty"`   // protocol version offered (hello only)
	Fams    string            `json:"fams,omitempty"` // families offered, comma-sep (hello only)
	Peer    string            `json:"peer,omitempty"`
	Branch  string            `json:"branch,omitempty"`
	Heads   []string          `json:"heads,omitempty"`
	WS      string            `json:"ws,omitempty"` // workspace id (mesh scoping)
	Commits []json.RawMessage `json:"commits,omitempty"`
	Want    []string          `json:"want,omitempty"`
	Objects []string          `json:"objects,omitempty"`
	Error   string            `json:"error,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// Protocol / envelope versioning.
const (
	// ProtoVersion is the highest protocol version this build speaks.
	ProtoVersion = 2
	// FamSync is the implicit family of all legacy sync messages.
	FamSync = "sync"
)

// Engine syncs one repo with peers.
type Engine struct {
	Repo *store.Repo
	// NodeHex/NodePub identify the MACHINE (pairing layer), advertised in
	// the hello extras. Optional — CLI one-shot syncs leave them empty.
	NodeHex string
	NodePub string
	// Ctl is the dialer's control stream, kept after SyncWithSession
	// returns so the daemon can run keepalive rounds on the same stream
	// (the responder serves pings in its read loop). Responder side: nil.
	Ctl *mux.Stream
	// fetchedObjects counts trees/blobs/chunks moved via fetchObjects;
	// merged into SyncResult.Fetched so resume granularity is visible.
	fetchedObjects int

	// OneShot marks a CLI one-shot responder: the initiator is told (via
	// the hello) to FIN the control stream after the sync round instead
	// of keeping the session for keepalive — the CLI exits either way.
	OneShot bool

	// KeepCtl leaves the control stream open after the sync completes
	// (daemon mode: the session stays live for keepalive rounds). One-shot
	// callers leave it false: the stream is FIN'd so the responder's serve
	// loop unblocks instead of waiting for a ping that never comes.
	KeepCtl bool
	// OnHello, when set, fires the moment the remote hello is read (both
	// roles) — lets the daemon fill presence fields (user, device) before
	// the sync completes, which matters on persistent sessions where
	// SyncWithSession doesn't return until the peer hangs up.
	OnHello func(user, node string)
	// AdvertiseFams adds families to our hello beyond "sync" (e.g. the
	// daemon advertises "relay" and "punch").
	AdvertiseFams []string
	// SessionVer is the negotiated session protocol version (filled from
	// the remote hello; 1 when the peer is legacy).
	SessionVer int
	// RemoteFams holds the families the remote advertised.
	RemoteFams map[string]bool
	// Handlers, when set, receive non-sync-family messages on the
	// responder loop (envelope dispatch). Unhandled families are ignored.
	Handlers map[string]func(m Msg, ctl *mux.Stream, sess *mux.Session) error
}

// New creates an Engine.
func New(r *store.Repo) *Engine { return &Engine{Repo: r} }

// SyncResult summarizes a sync session.
type SyncResult struct {
	RemotePeer     string
	RemoteUser     string
	RemoteNode     string // remote device node PeerID (pairing layer, may be "")
	RemoteNodePub  string
	Fetched        int
	Pushed         int
	MergedCommit   string
	ConflictBranch string
	FastForwarded  bool
	// PeerHasNews: the peer advertised a tip we don't have (keepalive
	// divergence detection) — the session was dropped so the daemon can
	// re-dial and fetch it.
	PeerHasNews bool
	// SessionVer is the negotiated protocol version for this session.
	SessionVer int
	// RemoteFams is the comma-separated family list the remote offered.
	RemoteFams string
	Message    string
}

// SyncWithSession runs a full bidirectional sync over an established mux
// session. initiator=true for the dialer side.
func (e *Engine) SyncWithSession(sess *mux.Session, initiator bool, remoteBranch string) (*SyncResult, error) {
	res := &SyncResult{}
	// Control stream handshake doubles as hello exchange.
	var ctl *mux.Stream
	var err error
	if initiator {
		ctl, err = sess.OpenStream()
		if err != nil {
			return nil, err
		}
		e.Ctl = ctl // kept for post-sync keepalive rounds
		if err := e.writeHello(ctl); err != nil {
			return nil, err
		}
		hello, err := readMsg(ctl)
		if err != nil {
			return nil, err
		}
		if hello.Type == "error" {
			return res, fmt.Errorf("remote refused sync: %s", hello.Error)
		}
		res.RemotePeer = hello.Peer
		res.RemoteUser = hello.Extra["user"]
		res.RemoteNode = hello.Extra["node"]
		res.RemoteNodePub = hello.Extra["nodepub"]
		e.negotiate(hello, res)
		if e.OnHello != nil {
			e.OnHello(res.RemoteUser, res.RemoteNode)
		}
		if ok, err := e.workspaceGate(ctl, hello, res); err != nil || !ok {
			return res, err
		}
		if err := e.serveResponder(ctl, sess, hello, res); err != nil {
			return res, err
		}
		// One-shot either way: we are one-shot (CLI), or the responder
		// advertised oneshot (a CLI punch-join) — FIN the control stream
		// so their serve loop unblocks (it keeps serving after "done"
		// for persistent daemon sessions otherwise).
		remoteOneshot := hello.Extra["oneshot"] == "1"
		if !e.KeepCtl || remoteOneshot {
			_ = ctl.Close()
		}
	} else {
		ctl, err = sess.AcceptStream()
		if err != nil {
			return nil, err
		}
		hello, err := readMsg(ctl)
		if err != nil {
			return nil, err
		}
		return e.ServeInbound(ctl, sess, hello)
	}
	res.Fetched += e.fetchedObjects // commits + trees/blobs/chunks moved
	_ = remoteBranch
	return res, nil
}

// ServeInbound runs the responder path with a pre-read first message.
// The daemon uses this to dispatch the first control message by family
// (relay / punch sessions never send a sync hello) before committing to
// a sync.
func (e *Engine) ServeInbound(ctl *mux.Stream, sess *mux.Session, hello Msg) (*SyncResult, error) {
	res := &SyncResult{}
	res.RemotePeer = hello.Peer
	res.RemoteUser = hello.Extra["user"]
	res.RemoteNode = hello.Extra["node"]
	res.RemoteNodePub = hello.Extra["nodepub"]
	e.negotiate(hello, res)
	if e.OnHello != nil {
		e.OnHello(res.RemoteUser, res.RemoteNode)
	}
	if ok, err := e.workspaceGate(ctl, hello, res); err != nil || !ok {
		return res, err
	}
	if err := e.writeHello(ctl); err != nil {
		return nil, err
	}
	if err := e.serveInitiatorRequests(ctl, sess, hello, res); err != nil {
		return res, err
	}
	return res, nil
}

// workspaceGate enforces mesh scoping: a sync only proceeds when both sides
// belong to the same workspace. This is what keeps strangers on shared
// Wi-Fi from cross-pollinating each other's repos — before it existed, any
// two LRM daemons on the same LAN would sync, and disjoint histories landed
// as HEAD-peer-* conflict refs in both object stores.
//
// Rules:
//   - both non-empty and different  → refuse (error msg, zero objects moved)
//   - local empty, remote non-empty → adopt the remote workspace (the
//     v1-Port-Key join flow: an untethered repo takes the sharer's identity)
//   - anything else                 → allowed (legacy peers, new join)
func (e *Engine) workspaceGate(ctl *mux.Stream, hello Msg, res *SyncResult) (bool, error) {
	local := e.Repo.Config.Workspace
	remote := hello.WS
	if remote != "" && local == "" {
		if err := e.Repo.SetWorkspace(remote); err == nil {
			if err := e.Repo.SaveConfig(); err != nil {
				return false, fmt.Errorf("persist adopted workspace: %w", err)
			}
			local = remote
		}
	}
	if local != "" && remote != "" && local != remote {
		res.Message = fmt.Sprintf("workspace mismatch (%s ≠ %s) — sync refused, no objects exchanged",
			shortHexStr(local), shortHexStr(remote))
		_ = writeMsg(ctl, Msg{Type: "error", Error: "workspace mismatch: peer belongs to a different workspace"})
		return false, nil
	}
	return true, nil
}

// PingRound sends one keepalive ping on the dialer's control stream and
// waits up to timeout for the pong. Returns the peer's advertised tip
// ("" when they have none). The peer reacts to OUR tip per the ping
// handler (dropping the session if we have news). Requires a completed
// SyncWithSession as the dialer (e.Ctl set).
func (e *Engine) PingRound(timeout time.Duration) (string, error) {
	if e.Ctl == nil {
		return "", fmt.Errorf("no control stream for keepalive")
	}
	br, _ := e.Repo.HeadBranch()
	tipHex, _ := e.Repo.GetRef(br)
	heads := []string{}
	if tipHex != "" {
		heads = append(heads, tipHex)
	}
	if err := writeMsg(e.Ctl, Msg{Type: "ping", Heads: heads}); err != nil {
		return "", err
	}
	type pong struct {
		tip string
		err error
	}
	ch := make(chan pong, 1)
	go func() {
		m, err := readMsg(e.Ctl)
		if err != nil {
			ch <- pong{"", err}
			return
		}
		if m.Type != "pong" {
			ch <- pong{"", fmt.Errorf("expected pong, got %q", m.Type)}
			return
		}
		if len(m.Heads) > 0 {
			ch <- pong{m.Heads[0], nil}
		} else {
			ch <- pong{"", nil}
		}
	}()
	select {
	case r := <-ch:
		return r.tip, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("keepalive pong timeout")
	}
}

// negotiate records the session protocol version and remote families
// from a remote hello. Version = min(ours, theirs); a hello without pv is
// a v1 peer (legacy envelope-less wire format).
func (e *Engine) negotiate(hello Msg, res *SyncResult) {
	e.SessionVer = 1
	if hello.PV > 1 {
		v := ProtoVersion
		if hello.PV < v {
			v = hello.PV
		}
		e.SessionVer = v
	}
	e.RemoteFams = map[string]bool{}
	for _, f := range strings.Split(hello.Fams, ",") {
		if f = strings.TrimSpace(f); f != "" {
			e.RemoteFams[f] = true
		}
	}
	res.SessionVer = e.SessionVer
	res.RemoteFams = hello.Fams
}

// writeHello advertises our branch heads.
func (e *Engine) writeHello(w io.Writer) error {
	branches, _ := e.Repo.ListBranches()
	var heads []string
	br, _ := e.Repo.HeadBranch()
	tipHex, _ := e.Repo.GetRef(br)
	if tipHex != "" {
		heads = append(heads, tipHex)
	}
	_ = branches
	extra := map[string]string{"user": e.Repo.Config.User}
	if e.NodeHex != "" {
		extra["node"] = e.NodeHex
	}
	if e.NodePub != "" {
		extra["nodepub"] = e.NodePub
	}
	if e.OneShot {
		extra["oneshot"] = "1"
	}
	fams := FamSync
	for _, f := range e.AdvertiseFams {
		if f != "" && f != FamSync {
			fams += "," + f
		}
	}
	return writeMsg(w, Msg{
		Type: "hello", Peer: e.Repo.Identity.HexID(), Branch: br, Heads: heads,
		PV: ProtoVersion, Fams: fams, WS: e.Repo.Config.Workspace,
		Extra: extra,
	})
}

// --- initiator side (dialer drives fetch, then serves) ---

func (e *Engine) serveResponder(ctl *mux.Stream, sess *mux.Session, hello Msg, res *SyncResult) error {
	// We are the dialer: decide what we need from the remote.
	remoteHeads := hello.Heads
	localTip, localBranch, _ := e.Repo.HeadCommit()
	var remoteTip cas.Hash
	if len(remoteHeads) > 0 {
		h, err := cas.ParseHex(remoteHeads[0])
		if err == nil {
			remoteTip = h
		}
	}
	if remoteTip == cas.Nil {
		// Remote is empty: push our history instead.
		if localTip != cas.Nil {
			if err := e.pushToRemote(ctl, sess, localTip, res); err != nil {
				return err
			}
			if res.Pushed > 0 && res.Message == "" {
				res.Message = fmt.Sprintf("pushed %d commit(s) to peer", res.Pushed)
			}
			return nil
		}
		return writeMsg(ctl, Msg{Type: "done"})
	}
	// Fetch remote commits we lack. We don't have remote's graph yet, so
	// iteratively request: want=[remoteTip], then parents until known.
	needed, err := e.fetchGraph(ctl, sess, remoteTip, res)
	if err != nil {
		return err
	}
	_ = needed
	// Integrate: fast-forward / merge / conflict-branch.
	if err := e.integrate(remoteTip, hello.Peer, localTip, localBranch, res); err != nil {
		return err
	}
	// Now push anything they lack (bidirectional).
	if localTip != cas.Nil {
		pushedBefore := res.Pushed
		if err := e.pushToRemote(ctl, sess, localTip, res); err != nil {
			return err
		}
		if n := res.Pushed - pushedBefore; n > 0 && (res.Message == "" || res.Message == "already up to date") {
			res.Message = fmt.Sprintf("pushed %d commit(s) to peer", n)
		}
	}
	return writeMsg(ctl, Msg{Type: "done"})
}

// fetchGraph pulls commits + all referenced objects for tip.
func (e *Engine) fetchGraph(ctl *mux.Stream, sess *mux.Session, tip cas.Hash, res *SyncResult) ([]cas.Hash, error) {
	var fetched []cas.Hash
	queue := []cas.Hash{tip}
	// Request commits level-by-level.
	for len(queue) > 0 {
		// Filter already-have.
		var want []cas.Hash
		for _, h := range queue {
			if !e.Repo.DAG.Has(h) {
				want = append(want, h)
			}
		}
		queue = nil
		if len(want) == 0 {
			break
		}
		wantHex := make([]string, 0, len(want))
		for _, h := range want {
			wantHex = append(wantHex, cas.Hex(h))
		}
		if err := writeMsg(ctl, Msg{Type: "want", Want: wantHex}); err != nil {
			return fetched, err
		}
		resp, err := readMsg(ctl)
		if err != nil {
			return fetched, err
		}
		if resp.Type == "error" {
			return fetched, fmt.Errorf("peer error: %s", resp.Error)
		}
		if resp.Type != "have" {
			return fetched, fmt.Errorf("expected have, got %s", resp.Type)
		}
		for _, raw := range resp.Commits {
			var c dag.Commit
			if err := json.Unmarshal(raw, &c); err != nil {
				continue
			}
			h := c.Hash()
			if _, err := e.Repo.DAG.Put(&c); err != nil {
				return fetched, err
			}
			fetched = append(fetched, h)
			res.Fetched++
			for _, p := range c.Parents {
				if ph, err := cas.ParseHex(p); err == nil && !e.Repo.DAG.Has(ph) {
					queue = append(queue, ph)
				}
			}
		}
	}
	// Fetch missing trees/blobs/chunks for all fetched commits.
	// Multi-round: fetching a tree unlocks descent to its children.
	for round := 0; round < 12; round++ {
		objs, err := e.missingObjects(fetched)
		if err != nil {
			return fetched, err
		}
		if len(objs) == 0 {
			break
		}
		if err := e.fetchObjects(ctl, sess, objs); err != nil {
			return fetched, err
		}
	}
	if objs, _ := e.missingObjects(fetched); len(objs) > 0 {
		return fetched, fmt.Errorf("sync incomplete: %d object(s) still missing (peer may lack them)", len(objs))
	}
	return fetched, nil
}

// missingObjects walks commit trees to find objects we lack.
func (e *Engine) missingObjects(commits []cas.Hash) ([]cas.Hash, error) {
	var out []cas.Hash
	seen := map[cas.Hash]bool{}
	var walkTree func(h cas.Hash) error
	walkTree = func(h cas.Hash) error {
		if h == cas.Nil || seen[h] {
			return nil
		}
		seen[h] = true
		if !e.Repo.CAS.Exists(h) {
			out = append(out, h)
			return nil // can't descend into missing tree yet; fetch then retry
		}
		t, err := merkle.LoadTree(e.Repo.CAS, h)
		if err != nil {
			return nil // might be a blob referenced as tree; ignore
		}
		for _, en := range t.Entries {
			eh, err := cas.ParseHex(en.Hash)
			if err != nil {
				continue
			}
			if en.IsDir {
				if err := walkTree(eh); err != nil {
					return err
				}
				continue
			}
			if seen[eh] {
				continue
			}
			seen[eh] = true
			if !e.Repo.CAS.Exists(eh) {
				out = append(out, eh)
				continue
			}
			// If chunked manifest, check chunks.
			if en.Chunked {
				m, err := chunker.LoadManifest(e.Repo.CAS, eh)
				if err == nil {
					for _, ch := range m.Chunks {
						chh, err := cas.ParseHex(ch)
						if err != nil || seen[chh] {
							continue
						}
						seen[chh] = true
						if !e.Repo.CAS.Exists(chh) {
							out = append(out, chh)
						}
					}
				}
			}
		}
		return nil
	}
	for _, ch := range commits {
		c, err := e.Repo.DAG.Get(ch)
		if err != nil {
			continue
		}
		th, err := cas.ParseHex(c.Tree)
		if err != nil {
			continue
		}
		// Single wave: missing trees block descent; the caller (fetchGraph)
		// re-invokes after each fetch round until no objects are missing.
		if err := walkTree(th); err != nil {
			return nil, err
		}
	}
	// Multi-round expansion: after fetching, trees become available. The
	// fetchObjects caller handles one level; integrate() verifies completeness.
	// For robustness, expand transitively for locally-available trees only
	// (missing subtrees will surface as fetch errors with clear hashes).
	return out, nil
}

// fetchObjects bulk-transfers objects over a dedicated mux stream.
// Request on ctl: {"type":"want-objects","objects":[...]}.
// Data on new stream: for each object: [64B hex][8B size BE][size bytes].
func (e *Engine) fetchObjects(ctl *mux.Stream, sess *mux.Session, objs []cas.Hash) error {
	if len(objs) == 0 {
		return nil
	}
	hexes := make([]string, 0, len(objs))
	for _, h := range objs {
		hexes = append(hexes, cas.Hex(h))
	}
	if err := writeMsg(ctl, Msg{Type: "want-objects", Objects: hexes}); err != nil {
		return err
	}
	// Count every object moved over this stream (trees, blobs, chunks) —
	// resume granularity is visible in the fetched count.
	e.fetchedObjects += len(objs)
	// Responder opens its own stream to us; accept it.
	// (Simpler than addressing streams by id across sides.)
	resp, err := sess.AcceptStream()
	if err != nil {
		return err
	}
	defer resp.Close()
	for _, want := range hexes {
		hdr := make([]byte, 72)
		if _, err := io.ReadFull(resp, hdr); err != nil {
			return fmt.Errorf("object stream ended: %w", err)
		}
		gotHex := string(hdr[:64])
		size := binary.BigEndian.Uint64(hdr[64:72])
		if size == ^uint64(0) {
			return fmt.Errorf("peer missing object %s", want)
		}
		if size > 1<<31 {
			return fmt.Errorf("object %s absurd size %d", gotHex, size)
		}
		h, err := cas.ParseHex(gotHex)
		if err != nil {
			return err
		}
		// Stream directly into CAS temp file with bounded buffer.
		if err := e.streamIntoCAS(h, resp, int64(size)); err != nil {
			return fmt.Errorf("store %s: %w", gotHex[:8], err)
		}
	}
	// After first wave, re-scan for newly-descendable missing objects (multi-round).
	// Discover deeper missing set by re-walking fetched commit trees.
	// (Bounded to a few rounds; each round fetches strictly deeper objects.)
	return nil
}

func (e *Engine) streamIntoCAS(h cas.Hash, r io.Reader, size int64) error {
	if e.Repo.CAS.Exists(h) {
		_, err := io.CopyN(io.Discard, r, size)
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		got, _, err := e.Repo.CAS.PutReader(pr)
		if err != nil {
			done <- err
			return
		}
		if got != h {
			// Hash mismatch: object bytes don't match requested address.
			// This happens for domain-separated trees/commits (address =
			// SHA(prefix+raw)). Accept by copying blob to explicit address.
			done <- copyBlobToAddress(e, got, h)
			return
		}
		done <- nil
	}()
	_, cpErr := io.CopyN(pw, r, size)
	_ = pw.Close()
	putErr := <-done
	if cpErr != nil {
		return cpErr
	}
	return putErr
}

// --- integration (fast-forward / merge / conflict branch) ---

func (e *Engine) integrate(remoteTip cas.Hash, remotePeer string, localTip cas.Hash, localBranch string, res *SyncResult) error {
	res.RemotePeer = remotePeer
	if localTip == cas.Nil {
		// Empty local: adopt remote.
		if err := e.Repo.SetRef(localBranch, cas.Hex(remoteTip)); err != nil {
			return err
		}
		e.Repo.AppendReflog(localBranch, "", cas.Hex(remoteTip), "sync", "adopted from "+shortHexStr(remotePeer))
		if err := Checkout(e.Repo, remoteTip); err != nil {
			return err
		}
		res.FastForwarded = true
		res.Message = "adopted remote history (empty local)"
		_, _ = e.Repo.Replog.Append(replog.Entry{Type: replog.TypeFetch, PeerHex: remotePeer, Commit: cas.Hex(remoteTip), Message: res.Message})
		return nil
	}
	base, err := lca.Find(e.Repo.DAG, localTip, remoteTip)
	if err != nil || base == cas.Nil {
		// Disjoint histories → conflict branch.
		return e.conflictBranch(remoteTip, remotePeer, localBranch, res, "disjoint histories")
	}
	if base == remoteTip {
		res.Message = "already up to date"
		return nil // remote is ancestor; nothing to do
	}
	if base == localTip {
		// Fast-forward.
		oldHex, _ := e.Repo.GetRef(localBranch)
		if err := e.Repo.SetRef(localBranch, cas.Hex(remoteTip)); err != nil {
			return err
		}
		e.Repo.AppendReflog(localBranch, oldHex, cas.Hex(remoteTip), "sync", "fast-forward from "+shortHexStr(remotePeer))
		// Update working dir + index.
		rc, _ := e.Repo.DAG.Get(remoteTip)
		th, _ := cas.ParseHex(rc.Tree)
		if err := Checkout(e.Repo, remoteTip); err != nil {
			return err
		}
		_ = th
		res.FastForwarded = true
		res.Message = "fast-forwarded to " + cas.Short(remoteTip)
		_, _ = e.Repo.Replog.Append(replog.Entry{Type: replog.TypeFetch, PeerHex: remotePeer, Commit: cas.Hex(remoteTip), Message: res.Message})
		return nil
	}
	// True divergence: try clean 3-way merge, else conflict branch.
	merged, overlapping, err := e.tryMerge(base, localTip, remoteTip)
	if err != nil {
		return e.conflictBranch(remoteTip, remotePeer, localBranch, res, "merge error: "+err.Error())
	}
	if len(overlapping) > 0 {
		return e.conflictBranch(remoteTip, remotePeer, localBranch, res,
			fmt.Sprintf("%d overlapping file(s): %s", len(overlapping), strings.Join(overlapping, ", ")))
	}
	// Create merge commit.
	mergeC, mh, err := e.createMergeCommit(merged, localTip, remoteTip, remotePeer)
	if err != nil {
		return err
	}
	_ = mergeC
	oldHex, _ := e.Repo.GetRef(localBranch)
	if err := e.Repo.SetRef(localBranch, cas.Hex(mh)); err != nil {
		return err
	}
	e.Repo.AppendReflog(localBranch, oldHex, cas.Hex(mh), "sync", "auto-merged "+cas.Short(remoteTip))
	if err := Checkout(e.Repo, mh); err != nil {
		return err
	}
	res.MergedCommit = cas.Hex(mh)
	res.Message = "auto-merged " + cas.Short(remoteTip)
	_, _ = e.Repo.Replog.Append(replog.Entry{Type: replog.TypeMerge, PeerHex: remotePeer, Commit: cas.Hex(mh), Message: res.Message})
	return nil
}

// shortHexStr truncates a hex id for display (defensive on length).
func shortHexStr(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// conflictBranch records the remote tip on HEAD-peer-<short> without
// touching the local branch or working dir.
func (e *Engine) conflictBranch(remoteTip cas.Hash, remotePeer, localBranch string, res *SyncResult, reason string) error {
	short := remotePeer
	if len(short) > 8 {
		short = short[:8]
	}
	name := "HEAD-peer-" + short
	oldHex, _ := e.Repo.GetRef(name)
	if err := e.Repo.SetRef(name, cas.Hex(remoteTip)); err != nil {
		return err
	}
	e.Repo.AppendReflog(name, oldHex, cas.Hex(remoteTip), "sync", "conflict branch ("+reason+")")
	res.ConflictBranch = name
	res.Message = "concurrent edits → conflict branch " + name + " (" + reason + ")"
	_, _ = e.Repo.Replog.Append(replog.Entry{
		Type: replog.TypeConflict, PeerHex: remotePeer, Commit: cas.Hex(remoteTip),
		Message: res.Message, Extra: map[string]string{"branch": name, "local": localBranch},
	})
	return nil
}

// --- responder side (serve fetch requests) ---

func (e *Engine) serveInitiatorRequests(ctl *mux.Stream, sess *mux.Session, hello Msg, res *SyncResult) error {
	rd := bufio.NewReader(ctl)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return nil // peer closed; done
		}
		var m Msg
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		// Envelope dispatch: non-sync families go to registered handlers
		// ("" fam = sync = legacy wire format, handled below).
		if m.Fam != "" && m.Fam != FamSync {
			if h, ok := e.Handlers[m.Fam]; ok {
				if err := h(m, ctl, sess); err != nil {
					return err
				}
			}
			continue
		}
		switch m.Type {
		case "done":
			if res.Message == "" {
				res.Message = fmt.Sprintf("served sync (pushed=%d fetched=%d)", res.Pushed, res.Fetched)
			}
			// Peer finished fetching; now pull anything THEY have that we
			// lack? (Bidirectional: request their tip if unknown.)
			if len(hello.Heads) > 0 {
				if h, err := cas.ParseHex(hello.Heads[0]); err == nil && !e.Repo.DAG.Has(h) {
					// Best-effort fetch via new control stream.
					if ctl2, err := sess.OpenStream(); err == nil {
						_ = e.writeHello(ctl2)
						// Read their hello then fetch... simplified: skip deep
						// bidirectional on responder; dialer already pushed.
						_ = ctl2.Close()
					}
				}
			}
			// Persistent sessions: KEEP SERVING (pings from live daemons)
			// until the dialer FINs the control stream or disconnects.
		case "want":
			var raws []json.RawMessage
			for _, w := range m.Want {
				h, err := cas.ParseHex(w)
				if err != nil || !e.Repo.DAG.Has(h) {
					continue
				}
				c, err := e.Repo.DAG.Get(h)
				if err != nil {
					continue
				}
				raw, _ := json.Marshal(c)
				raws = append(raws, raw)
				res.Pushed++
			}
			if err := writeMsg(ctl, Msg{Type: "have", Commits: raws}); err != nil {
				return err
			}
		case "want-objects":
			if err := e.serveObjects(sess, m.Objects); err != nil {
				return err
			}
		case "push-have":
			// Dialer pushes commits they have that we might lack.
			for _, raw := range m.Commits {
				var c dag.Commit
				if err := json.Unmarshal(raw, &c); err != nil {
					continue
				}
				h := c.Hash()
				if !e.Repo.DAG.Has(h) {
					_, _ = e.Repo.DAG.Put(&c)
					res.Fetched++
				}
			}
		case "ping":
			// Keepalive from a connected dialer: reply with our tip so
			// they can detect divergence without a full sync round.
			br, _ := e.Repo.HeadBranch()
			tipHex, _ := e.Repo.GetRef(br)
			heads := []string{}
			if tipHex != "" {
				heads = append(heads, tipHex)
			}
			if err := writeMsg(ctl, Msg{Type: "pong", Peer: e.Repo.Identity.HexID(), Heads: heads}); err != nil {
				return err
			}
			// If the dialer's tip is news to us, drop the session so our
			// own daemon re-dials and pulls it (hang-up-and-callback).
			if len(m.Heads) > 0 {
				if h, err := cas.ParseHex(m.Heads[0]); err == nil && !e.Repo.DAG.Has(h) {
					res.PeerHasNews = true
					res.Message = "peer advertised newer history — reconnecting"
					_ = sess.Close()
					return nil
				}
			}
		case "push-tip":
			// Dialer announces a tip we should integrate. First ensure we
			// hold every object it needs (fetch from dialer), then integrate.
			// Always reply push-ok exactly once (dialer blocks on it).
			pushErr := ""
			if len(m.Heads) == 0 {
				pushErr = "no tip announced"
			} else if h, err := cas.ParseHex(m.Heads[0]); err != nil || !e.Repo.DAG.Has(h) {
				pushErr = "tip unknown (push-have incomplete?)"
			} else if err := e.ensureTipObjects(ctl, sess, h); err != nil {
				pushErr = err.Error()
			} else {
				localTip, localBranch, _ := e.Repo.HeadCommit()
				_ = e.integrate(h, m.Peer, localTip, localBranch, res)
			}
			_ = writeMsg(ctl, Msg{Type: "push-ok", Peer: e.Repo.Identity.HexID(), Error: pushErr})
		}
	}
}

// pushToRemote sends commits the remote lacks (dialer → responder).
func (e *Engine) pushToRemote(ctl *mux.Stream, sess *mux.Session, localTip cas.Hash, res *SyncResult) error {
	// Simplest correct push: send our tip's history (bounded walk).
	hashes, commits, err := e.Repo.DAG.WalkTipOrder(localTip, 500)
	if err != nil {
		return err
	}
	// Send in small batches.
	for i := 0; i < len(commits); i += 50 {
		j := i + 50
		if j > len(commits) {
			j = len(commits)
		}
		var raws []json.RawMessage
		for _, c := range commits[i:j] {
			raw, _ := json.Marshal(c)
			raws = append(raws, raw)
		}
		_ = hashes
		if err := writeMsg(ctl, Msg{Type: "push-have", Peer: e.Repo.Identity.HexID(), Commits: raws}); err != nil {
			return err
		}
		res.Pushed += len(raws)
	}
	// Announce the tip, then serve object requests until the responder
	// confirms it has everything (push-ok). This closes the push leg:
	// the responder fetches missing trees/blobs/chunks NOW, so its repo
	// is never left with a tip whose objects are missing.
	if err := writeMsg(ctl, Msg{Type: "push-tip", Peer: e.Repo.Identity.HexID(), Heads: []string{cas.Hex(localTip)}}); err != nil {
		return err
	}
	for {
		m, err := readMsg(ctl)
		if err != nil {
			return nil // responder closed; nothing more to serve
		}
		switch m.Type {
		case "want-objects":
			if err := e.serveObjects(sess, m.Objects); err != nil {
				return err
			}
		case "push-ok":
			if m.Error != "" {
				return fmt.Errorf("peer push-tip: %s", m.Error)
			}
			return nil
		case "done":
			return nil
		}
	}
}

// collectKnownChain returns commits reachable from tip through locally-known
// objects (bounded BFS; stops at unknown parents).
func (e *Engine) collectKnownChain(tip cas.Hash) []cas.Hash {
	var out []cas.Hash
	seen := map[cas.Hash]bool{}
	queue := []cas.Hash{tip}
	for len(queue) > 0 && len(out) < 2000 {
		h := queue[0]
		queue = queue[1:]
		if seen[h] || !e.Repo.DAG.Has(h) {
			continue
		}
		seen[h] = true
		out = append(out, h)
		c, err := e.Repo.DAG.Get(h)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if ph, err := cas.ParseHex(p); err == nil && !seen[ph] {
				queue = append(queue, ph)
			}
		}
	}
	return out
}

// ensureTipObjects fetches (from the dialer over ctl) every object needed to
// materialize tip: multi-round until nothing is missing.
func (e *Engine) ensureTipObjects(ctl *mux.Stream, sess *mux.Session, tip cas.Hash) error {
	chain := e.collectKnownChain(tip)
	for round := 0; round < 12; round++ {
		objs, err := e.missingObjects(chain)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		if err := e.fetchObjects(ctl, sess, objs); err != nil {
			return err
		}
	}
	if objs, _ := e.missingObjects(chain); len(objs) > 0 {
		return fmt.Errorf("%d object(s) still missing for pushed tip", len(objs))
	}
	return nil
}

// serveObjects streams requested objects on a fresh mux stream.
func (e *Engine) serveObjects(sess *mux.Session, hexes []string) error {
	ds, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer ds.Close()
	for _, hx := range hexes {
		h, err := cas.ParseHex(hx)
		if err != nil || !e.Repo.CAS.Exists(h) {
			// Signal missing with size = ^0.
			hdr := make([]byte, 72)
			copy(hdr[:64], hx)
			binary.BigEndian.PutUint64(hdr[64:72], ^uint64(0))
			_, _ = ds.Write(hdr)
			continue
		}
		sz, _ := e.Repo.CAS.Stat(h)
		hdr := make([]byte, 72)
		copy(hdr[:64], strings.ToLower(hex.EncodeToString(h[:])))
		// NOTE: for domain-separated trees the stored blob hash differs from
		// requested address; we still label with the REQUESTED address so the
		// receiver files bytes under the right name.
		copy(hdr[:64], strings.ToLower(hx))
		binary.BigEndian.PutUint64(hdr[64:72], uint64(sz))
		if _, err := ds.Write(hdr); err != nil {
			return err
		}
		rc, err := e.Repo.CAS.Get(h)
		if err != nil {
			return err
		}
		_, cpErr := io.CopyBuffer(ds, rc, make([]byte, 32*1024))
		_ = rc.Close()
		if cpErr != nil {
			return cpErr
		}
	}
	return nil
}

// --- merge ---

// tryMerge performs a 3-way file-level merge. Returns merged path→hash map
// and the list of overlapping (conflicting) paths.
func (e *Engine) tryMerge(base, local, remote cas.Hash) (map[string]string, []string, error) {
	bc, err := e.Repo.DAG.Get(base)
	if err != nil {
		return nil, nil, err
	}
	lc, err := e.Repo.DAG.Get(local)
	if err != nil {
		return nil, nil, err
	}
	rc, err := e.Repo.DAG.Get(remote)
	if err != nil {
		return nil, nil, err
	}
	bh, _ := cas.ParseHex(bc.Tree)
	lh, _ := cas.ParseHex(lc.Tree)
	rh, _ := cas.ParseHex(rc.Tree)
	bm, lm, rm := map[string]string{}, map[string]string{}, map[string]string{}
	_ = merkle.Flatten(e.Repo.CAS, bh, "", bm)
	_ = merkle.Flatten(e.Repo.CAS, lh, "", lm)
	_ = merkle.Flatten(e.Repo.CAS, rh, "", rm)
	paths := map[string]bool{}
	for p := range bm {
		paths[p] = true
	}
	for p := range lm {
		paths[p] = true
	}
	for p := range rm {
		paths[p] = true
	}
	merged := map[string]string{}
	var overlapping []string
	for p := range paths {
		b, lok, rok := bm[p], lm[p], rm[p]
		lEx, rEx := hasKey(lm, p), hasKey(rm, p)
		switch {
		case lok == rok && lEx == rEx:
			if lEx {
				merged[p] = lok
			}
		case b == lok && !lEx == (b == "" && !hasKey(bm, p)):
			// unchanged locally → take remote
			if rEx {
				merged[p] = rok
			}
		case b == rok:
			// unchanged remotely → take local
			if lEx {
				merged[p] = lok
			}
		default:
			// Both changed.
			if lok == rok && lEx == rEx {
				if lEx {
					merged[p] = lok
				}
				continue
			}
			// Check true no-op: local==base?
			if lEx == hasKey(bm, p) && lok == b {
				if rEx {
					merged[p] = rok
				}
				continue
			}
			if rEx == hasKey(bm, p) && rok == b {
				if lEx {
					merged[p] = lok
				}
				continue
			}
			overlapping = append(overlapping, p)
		}
	}
	sort.Strings(overlapping)
	return merged, overlapping, nil
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

func (e *Engine) createMergeCommit(merged map[string]string, localTip, remoteTip cas.Hash, remotePeer string) (*dag.Commit, cas.Hash, error) {
	root, err := BuildTreeFromMap(e.Repo.CAS, merged)
	if err != nil {
		return nil, cas.Nil, err
	}
	lc, _ := e.Repo.DAG.Get(localTip)
	rc, _ := e.Repo.DAG.Get(remoteTip)
	clock := lc.Clock.Clone()
	clock.Merge(rc.Clock)
	clock.Increment(e.Repo.Identity.HexID())
	short := remotePeer
	if len(short) > 8 {
		short = short[:8]
	}
	c := &dag.Commit{
		Version: 1, Tree: cas.Hex(root),
		Parents: []string{cas.Hex(localTip), cas.Hex(remoteTip)},
		Author:  e.Repo.Config.User, PeerHex: e.Repo.Identity.HexID(),
		Timestamp: time.Now().UnixNano(),
		Message:   "Merge remote " + short,
		Clock:     clock,
	}
	h, err := e.Repo.DAG.Put(c)
	return c, h, err
}

// --- wire helpers ---

// WriteMsg encodes one control message (exported for daemon relay/punch
// handlers that speak envelope families on mux streams).
func WriteMsg(w io.Writer, m Msg) error { return writeMsg(w, m) }

// ReadMsg decodes one control message (exported; see WriteMsg).
func ReadMsg(r io.Reader) (Msg, error) { return readMsg(r) }

func writeMsg(w io.Writer, m Msg) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}

func readMsg(r io.Reader) (Msg, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		return Msg{}, err
	}
	return m, nil
}
