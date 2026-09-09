package lrq

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/chunker"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/merkle"
)

// qPeers discovers LAN peers (4s browse).
func (e *engine) qPeers(q Query) (Table, error) {
	t := Table{Title: "peers", Header: []string{"user", "addr", "id", "source"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	peers, _ := mdns.Browse(ctx, 4*time.Second)
	cancel()
	n := 0
	for _, p := range peers {
		if p.PeerHex == e.repo.Identity.HexID() {
			continue
		}
		t.Rows = append(t.Rows, []string{p.User, p.Addr(), p.PeerHex[:12], p.Source})
		n++
		if n >= q.Limit {
			break
		}
	}
	return t, nil
}

// qStatus reports branch/tip/cleanliness plus pending changes.
func (e *engine) qStatus() (Table, error) {
	br, _ := e.repo.HeadBranch()
	tipHex, _ := e.repo.GetRef(br)
	res, err := e.repo.Index.Scan(e.repo.Root, e.repo.CAS)
	if err != nil {
		return Table{}, err
	}
	clean := "yes"
	if !res.Clean {
		clean = fmt.Sprintf("no (%d change(s))", len(res.Changes))
	}
	t := Table{Title: "status", Header: []string{"branch", "tip", "root", "clean"}}
	t.Rows = append(t.Rows, []string{br, short12(tipHex), cas.Hex(res.RootHash)[:12], clean})
	for _, ch := range res.Changes {
		t.Rows = append(t.Rows, []string{"", ch.Kind, ch.Path, ""})
	}
	return t, nil
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(unborn)"
	}
	return s
}

// qBranches lists branches with tips.
func (e *engine) qBranches() (Table, error) {
	t := Table{Title: "branches", Header: []string{"current", "branch", "tip"}}
	cur, _ := e.repo.HeadBranch()
	branches, err := e.repo.ListBranches()
	if err != nil {
		return Table{}, err
	}
	sort.Strings(branches)
	for _, b := range branches {
		tip, _ := e.repo.GetRef(b)
		mark := ""
		if b == cur {
			mark = "*"
		}
		t.Rows = append(t.Rows, []string{mark, b, short12(tip)})
	}
	return t, nil
}

// qTags lists lightweight tags.
func (e *engine) qTags() (Table, error) {
	t := Table{Title: "tags", Header: []string{"tag", "target"}}
	tags, err := e.repo.ListTags()
	if err != nil {
		return Table{}, err
	}
	names := make([]string, 0, len(tags))
	for n := range tags {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Rows = append(t.Rows, []string{n, short12(tags[n])})
	}
	return t, nil
}

// qCommits lists history on a ref.
func (e *engine) qCommits(q Query) (Table, error) {
	t := Table{Title: "commits", Header: []string{"hash", "author", "time", "message"}}
	tip, err := e.resolveRef(q.Ref)
	if err != nil {
		return Table{}, err
	}
	hashes, commits, err := e.repo.DAG.WalkTipOrder(tip, q.Limit)
	if err != nil {
		return Table{}, err
	}
	for i, c := range commits {
		t.Rows = append(t.Rows, []string{
			short12(cas.Hex(hashes[i])), c.Author,
			time.Unix(0, c.Timestamp).Format("2006-01-02 15:04"),
			firstLine(c.Message),
		})
	}
	return t, nil
}

// qFiles lists files at a ref with size/hash and optional filters.
func (e *engine) qFiles(q Query) (Table, error) {
	t := Table{Title: "files", Header: []string{"path", "size", "hash"}}
	tip, err := e.resolveRef(q.Ref)
	if err != nil {
		return Table{}, err
	}
	c, err := e.repo.DAG.Get(tip)
	if err != nil {
		return Table{}, err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return Table{}, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(e.repo.CAS, th, "", flat); err != nil {
		return Table{}, err
	}
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	shown := 0
	for _, p := range paths {
		if q.Pattern != "" {
			ok, _ := path.Match(q.Pattern, p)
			if !ok {
				continue
			}
		}
		h, err := cas.ParseHex(flat[p])
		if err != nil {
			continue
		}
		size := e.objectSize(h)
		if q.Bigger >= 0 && size <= q.Bigger {
			continue
		}
		if q.Small >= 0 && size >= q.Small {
			continue
		}
		t.Rows = append(t.Rows, []string{p, humanSize(size), flat[p][:8]})
		shown++
		if shown >= q.Limit {
			break
		}
	}
	return t, nil
}

// objectSize returns real content bytes (manifest-aware).
func (e *engine) objectSize(h cas.Hash) int64 {
	raw, err := e.repo.CAS.GetBytes(h, 4<<20)
	if err != nil {
		return 0
	}
	var m chunker.Manifest
	if err := json.Unmarshal(raw, &m); err == nil && m.Version == 1 && len(m.Chunks) > 0 {
		return m.TotalSize
	}
	if sz, err := e.repo.CAS.Stat(h); err == nil {
		return sz
	}
	return int64(len(raw))
}

// qShow renders one commit vertically.
func (e *engine) qShow(q Query) (Table, error) {
	t := Table{Title: "show " + q.Ref, Header: []string{"field", "value"}}
	h, err := e.resolveRef(q.Ref)
	if err != nil {
		return Table{}, err
	}
	c, err := e.repo.DAG.Get(h)
	if err != nil {
		return Table{}, err
	}
	parents := "(none)"
	if len(c.Parents) > 0 {
		ps := make([]string, 0, len(c.Parents))
		for _, p := range c.Parents {
			ps = append(ps, short12(p))
		}
		parents = strings.Join(ps, " ")
	}
	t.Rows = [][]string{
		{"hash", cas.Hex(h)},
		{"author", c.Author},
		{"peer", short12(c.PeerHex)},
		{"time", time.Unix(0, c.Timestamp).Format(time.RFC3339)},
		{"clock", c.Clock.String()},
		{"parents", parents},
		{"tree", c.Tree},
		{"message", c.Message},
	}
	return t, nil
}

// qFind searches filenames at a ref for a substring.
func (e *engine) qFind(q Query) (Table, error) {
	t := Table{Title: "find", Header: []string{"path", "hash"}}
	tip, err := e.resolveRef(q.Ref)
	if err != nil {
		return Table{}, err
	}
	c, err := e.repo.DAG.Get(tip)
	if err != nil {
		return Table{}, err
	}
	th, err := cas.ParseHex(c.Tree)
	if err != nil {
		return Table{}, err
	}
	flat := map[string]string{}
	if err := merkle.Flatten(e.repo.CAS, th, "", flat); err != nil {
		return Table{}, err
	}
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	sub := strings.ToLower(q.Pattern)
	shown := 0
	for _, p := range paths {
		if !strings.Contains(strings.ToLower(p), sub) {
			continue
		}
		t.Rows = append(t.Rows, []string{p, flat[p][:8]})
		shown++
		if shown >= q.Limit {
			break
		}
	}
	return t, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
