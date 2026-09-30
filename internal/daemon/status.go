package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dash"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/merkle"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
)

// StatusFileName is the snapshot the daemon publishes for `lrm dashboard`.
// It is written atomically and contains only local information.
const StatusFileName = "status.json"

const (
	statusRing    = 64
	maxSyncFiles  = 40 // changed paths recorded per sync
	statusRefresh = 2 * time.Second
)

func (d *Daemon) statusPath() string { return filepath.Join(d.repo.LrmDir, StatusFileName) }

func ring[T any](r []T, e T) []T {
	r = append(r, e)
	if len(r) > statusRing {
		r = r[len(r)-statusRing:]
	}
	return r
}

// noteSync records a completed (or refused) sync in the published view.
func (d *Daemon) noteSync(res *lrmsync.SyncResult, err error, outbound bool, peerHex, peerUser string, files []string) {
	if res == nil && err == nil {
		return
	}
	d.statusMu.Lock()
	if err != nil {
		d.rejectLog = ring(d.rejectLog, dash.RejectEntry{
			Time:   time.Now().UTC().Format(time.RFC3339),
			Peer:   peerHex,
			Reason: err.Error(),
		})
	} else {
		d.syncLog = ring(d.syncLog, dash.SyncEntry{
			Time:     time.Now().UTC().Format(time.RFC3339),
			Peer:     peerHex,
			User:     peerUser,
			Message:  res.Message,
			Fetched:  res.Fetched,
			Pushed:   res.Pushed,
			Outbound: outbound,
			Files:    files,
		})
	}
	d.statusMu.Unlock()
	d.publishStatus()
}

// noteEvent records a daemon activity line (also feeds `lrm watch`).
func (d *Daemon) noteEvent(msg string) {
	d.statusMu.Lock()
	d.eventLog = ring(d.eventLog, dash.EventEntry{
		Time: time.Now().UTC().Format(time.RFC3339),
		Text: msg,
	})
	d.statusMu.Unlock()
}

// headTree returns the tree hash of HEAD (zero hash when there is none).
func (d *Daemon) headTree() cas.Hash {
	h, _, err := d.repo.HeadCommit()
	if err != nil {
		return cas.Nil
	}
	c, err := d.repo.DAG.Get(h)
	if err != nil {
		return cas.Nil
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return cas.Nil
	}
	return th
}

// changedFiles diffs the working state before a sync against HEAD now,
// naming what moved (capped). This is what makes the dashboard show the
// files a push actually touched.
func (d *Daemon) changedFiles(before cas.Hash) []string {
	after := d.headTree()
	if before == after || after == cas.Nil {
		return nil
	}
	if before == cas.Nil {
		// Empty repo adopted a history: list it.
		flat := map[string]string{}
		if err := merkle.Flatten(d.repo.CAS, after, "", flat); err != nil {
			return nil
		}
		out := make([]string, 0, len(flat))
		for p := range flat {
			out = append(out, p)
			if len(out) >= maxSyncFiles {
				out = append(out, "...")
				break
			}
		}
		sortStrings(out)
		return out
	}
	changes, err := merkle.Diff(d.repo.CAS, before, after)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		switch c.Kind {
		case "added":
			out = append(out, "+"+c.Path)
		case "deleted":
			out = append(out, "-"+c.Path)
		default:
			out = append(out, "~"+c.Path)
		}
		if len(out) >= maxSyncFiles {
			out = append(out, "...")
			break
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// statusLoop republishes the snapshot periodically so presence, RTT, and
// file listings stay current even when nothing is happening.
func (d *Daemon) statusLoop(ctx context.Context) {
	t := time.NewTicker(statusRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.publishStatus()
		}
	}
}

// buildStatus assembles the snapshot from local state only: the live
// presence table, the discovery cache, and the two rings.
func (d *Daemon) buildStatus() dash.Status {
	st := dash.Status{
		Updated:   time.Now().UTC().Format(time.RFC3339),
		User:      d.repo.Config.User,
		PeerID:    d.repo.Identity.HexID(),
		Port:      d.cfg.Port,
		Workspace: d.repo.Config.Workspace,
		Peers:     []dash.PeerView{},
		Known:     []dash.PeerView{},
		Syncs:     []dash.SyncEntry{},
		Rejects:   []dash.RejectEntry{},
		Events:    []dash.EventEntry{},
	}
	if d.nodeID != nil {
		st.NodeID = d.nodeID.HexID()
	}
	if br, err := d.repo.HeadBranch(); err == nil {
		st.Branch = br
	}
	if h, br, err := d.repo.HeadCommit(); err == nil {
		st.Head = cas.Short(h)
		if br != "" {
			st.Branch = br
		}
	}

	for _, p := range d.Presence() {
		pv := dash.PeerView{
			ID: p.Peer, User: p.User, Addr: p.Addr, State: p.State, RTTms: p.RTTms,
			Source: "session",
		}
		if !p.Since.IsZero() {
			pv.Since = p.Since.UTC().Format(time.RFC3339)
		}
		if p.WS == d.repo.Config.Workspace {
			pv.WS = p.WS
		} else {
			pv.WS = "" // never show another workspace's id
		}
		st.Peers = append(st.Peers, pv)
	}

	d.known.Range(func(_, v any) bool {
		p, ok := v.(mdns.Peer)
		if !ok || p.PeerHex == "" || p.PeerHex == st.PeerID {
			return true
		}
		st.Known = append(st.Known, dash.PeerView{
			ID: p.PeerHex, User: p.User, Addr: p.Addr(), Source: p.Source,
			LastSeen: p.SeenAt.UTC().Format(time.RFC3339),
			// Only report the workspace id when it matches ours; another
			// workspace's id is not ours to display.
			WS: sameOrEmpty(p.WS, d.repo.Config.Workspace),
		})
		return true
	})

	d.statusMu.Lock()
	st.Syncs = append(st.Syncs, d.syncLog...)
	st.Rejects = append(st.Rejects, d.rejectLog...)
	st.Events = append(st.Events, d.eventLog...)
	d.statusMu.Unlock()
	return st
}

func sameOrEmpty(a, b string) string {
	if a != "" && a == b {
		return a
	}
	return ""
}

// publishStatus writes the snapshot atomically (temp file + rename), so a
// reader never sees a half-written page. Failures are ignored: a dashboard
// is a convenience, never a reason for the daemon to stop.
func (d *Daemon) publishStatus() {
	st := d.buildStatus()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := d.statusPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
