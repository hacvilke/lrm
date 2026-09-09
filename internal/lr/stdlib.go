package lr

// LRS standard library: sandboxed files, network probes, and LRM repo ops.
// All failures are graceful ({ok:false} maps), never panics; network events
// go to the report via ev.Rec.Net.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/mdns"
	"github.com/lrm-project/lrm/internal/mux"
	"github.com/lrm-project/lrm/internal/natpmp"
	"github.com/lrm-project/lrm/internal/portkey"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/stun"
	lrmsync "github.com/lrm-project/lrm/internal/sync"
	"github.com/lrm-project/lrm/internal/transport"
	"github.com/lrm-project/lrm/internal/upnp"
)

// installStdlib registers file, network and LRM builtins. Files are jailed
// to the evaluator's WorkDir; LRM builtins open the repo containing WorkDir.
func (ev *Evaluator) installStdlib() {
	box, err := newSandbox(ev.WorkDir)
	if err != nil {
		box = &sandbox{root: ev.WorkDir}
	}
	reg := func(name string, fn func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError)) {
		ev.Globals.Define(name, Value{Kind: KBuiltin, Bl: &Builtin{Name: name, Fn: fn}})
	}

	// ---- Sandboxed files ----
	reg("read_file", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("read_file", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "read_file() needs a string path")
		}
		p, err := box.resolve(args[0].S)
		if err != nil {
			return errMap("blocked: " + err.Error()), nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return errMap(err.Error()), nil
		}
		if len(raw) > 4<<20 {
			return errMap("file too large for read_file (>4MB)"), nil
		}
		return okMap("text", string(raw)), nil
	})
	reg("write_file", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("write_file", args, 2, 2, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr || args[1].Kind != KStr {
			return Null, ev.errf(pos, "write_file() needs (path, content) strings")
		}
		p, err := box.resolve(args[0].S)
		if err != nil {
			return errMap("blocked: " + err.Error()), nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return errMap(err.Error()), nil
		}
		if err := os.WriteFile(p, []byte(args[1].S), 0o644); err != nil {
			return errMap(err.Error()), nil
		}
		return okMap("bytes", len(args[1].S)), nil
	})
	reg("file_exists", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("file_exists", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "file_exists() needs a string path")
		}
		p, err := box.resolve(args[0].S)
		if err != nil {
			return False, nil
		}
		_, err = os.Stat(p)
		return Bool(err == nil), nil
	})

	// ---- Network probes ----
	reg("tcp_connect", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("tcp_connect", args, 2, 3, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "tcp_connect() host must be a string")
		}
		timeout := 8000
		if len(args) == 3 {
			if n, ok := AsInt(args[2]); ok && n > 0 {
				timeout = int(n)
			}
		}
		addr := net.JoinHostPort(args[0].S, args[1].String())
		t0 := time.Now()
		conn, err := net.DialTimeout("tcp", addr, time.Duration(timeout)*time.Millisecond)
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("tcp %s: FAIL %v (%dms)", addr, shortErr(err), ms))
			return failMap("error", err.Error(), "latency_ms", ms), nil
		}
		_ = conn.Close()
		ev.Rec.Net(fmt.Sprintf("tcp %s: OK (%dms)", addr, ms))
		return okMap("latency_ms", ms), nil
	})
	reg("dns_lookup", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("dns_lookup", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "dns_lookup() needs a string hostname")
		}
		host := args[0].S
		t0 := time.Now()
		ips, err := net.DefaultResolver.LookupHost(context.Background(), host)
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("dns %s: FAIL %v (%dms)", host, shortErr(err), ms))
			return errMap(err.Error()), nil
		}
		ev.Rec.Net(fmt.Sprintf("dns %s: OK %v (%dms)", host, ips, ms))
		return okMap("ips", ips, "latency_ms", ms), nil
	})
	reg("http_get", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("http_get", args, 1, 2, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "http_get() needs a string URL")
		}
		timeout := 15000
		if len(args) == 2 {
			if n, ok := AsInt(args[1]); ok && n > 0 {
				timeout = int(n)
			}
		}
		url := args[0].S
		t0 := time.Now()
		client := &http.Client{Timeout: time.Duration(timeout) * time.Millisecond}
		resp, err := client.Get(url)
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("http %s: FAIL %v", url, shortErr(err)))
			return errMap(err.Error()), nil
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("http %s: FAIL read %v", url, shortErr(err)))
			return errMap(err.Error()), nil
		}
		ev.Rec.Net(fmt.Sprintf("http %s: %d (%d bytes, %dms)", url, resp.StatusCode, len(body), ms))
		return okMap("status", resp.StatusCode, "body", string(body), "latency_ms", ms), nil
	})
	reg("stun_ip", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("stun_ip", args, 0, 0, pos); err != nil {
			return Null, err
		}
		t0 := time.Now()
		ip, err := stun.DiscoverPublicIP(nil, 8*time.Second)
		if err != nil || ip == nil {
			ev.Rec.Net(fmt.Sprintf("stun: FAIL %v", shortErr(err)))
			return errMap(fmt.Sprint("stun failed: ", shortErr(err))), nil
		}
		ev.Rec.Net(fmt.Sprintf("stun: public ip %s (%dms)", ip.String(), time.Since(t0).Milliseconds()))
		return okMap("ip", ip.String()), nil
	})

	// ---- LRM repo ops (repo containing WorkDir) ----
	reg("lrm_status", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_status", args, 0, 0, pos); err != nil {
			return Null, err
		}
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		br, _ := r.HeadBranch()
		tipHex, _ := r.GetRef(br)
		tip := ""
		if len(tipHex) >= 12 {
			tip = tipHex[:12]
		}
		res, err := r.Index.Scan(r.Root, r.CAS)
		if err != nil {
			return errMap(err.Error()), nil
		}
		changes := make([]Value, 0, len(res.Changes))
		for _, ch := range res.Changes {
			c := NewMap()
			c.M["kind"] = Str(ch.Kind)
			c.M["path"] = Str(ch.Path)
			changes = append(changes, c)
		}
		return okMap("branch", br, "tip", tip,
			"root", cas.Hex(res.RootHash)[:12],
			"clean", res.Clean, "changes", changes), nil
	})
	reg("lrm_commit", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_commit", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "lrm_commit() needs a string message")
		}
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		res, err := r.Index.Scan(r.Root, r.CAS)
		if err != nil {
			return errMap(err.Error()), nil
		}
		if res.Clean {
			return okMap("hash", "", "message", "nothing to commit"), nil
		}
		h, _, err := r.Commit(args[0].S, res.RootHash)
		if err != nil {
			return errMap(err.Error()), nil
		}
		_ = r.Index.UpdateFromScan(r.Root, r.CAS, res.RootHash)
		_ = r.Index.Save()
		return okMap("hash", cas.Hex(h)[:12]), nil
	})
	reg("lrm_log", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_log", args, 1, 1, pos); err != nil {
			return Null, err
		}
		n := 20
		if m, ok := AsInt(args[0]); ok && m > 0 {
			n = int(m)
		}
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		br, _ := r.HeadBranch()
		tipHex, _ := r.GetRef(br)
		if tipHex == "" {
			return okMap("commits", []Value{}), nil
		}
		tip, err := cas.ParseHex(tipHex)
		if err != nil {
			return errMap(err.Error()), nil
		}
		hashes, commits, err := r.DAG.WalkTipOrder(tip, n)
		if err != nil {
			return errMap(err.Error()), nil
		}
		out := make([]Value, 0, len(commits))
		for i, c := range commits {
			e := NewMap()
			e.M["hash"] = Str(cas.Hex(hashes[i])[:12])
			e.M["author"] = Str(c.Author)
			e.M["time"] = Str(time.Unix(0, c.Timestamp).UTC().Format(time.RFC3339))
			e.M["message"] = Str(c.Message)
			out = append(out, e)
		}
		return okMap("commits", out), nil
	})
	reg("lrm_peers", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_peers", args, 0, 0, pos); err != nil {
			return Null, err
		}
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		self := r.Identity.HexID()
		_ = r.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		peers, _ := mdns.Browse(ctx, 4*time.Second)
		cancel()
		out := []Value{}
		for i := range peers {
			p := &peers[i]
			if p.PeerHex == self {
				continue
			}
			id := p.PeerHex
			if len(id) > 12 {
				id = id[:12]
			}
			e := NewMap()
			e.M["user"] = Str(p.User)
			e.M["addr"] = Str(p.Addr())
			e.M["id"] = Str(id)
			e.M["source"] = Str(p.Source)
			out = append(out, e)
		}
		ev.Rec.Net(fmt.Sprintf("lan peers: %d found", len(out)))
		return okMap("peers", out), nil
	})
	reg("lrm_share", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		// lrm_share(port_num, try_upnp) -> {ok,key,human,wan,mapped,listening}
		if err := arity("lrm_share", args, 2, 2, pos); err != nil {
			return Null, err
		}
		port := 0
		if n, ok := AsInt(args[0]); ok {
			port = int(n)
		}
		if port <= 0 || port > 65535 {
			return errMap("lrm_share() needs a valid port number"), nil
		}
		tryUPnP := args[1].Truthy()
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		mappedPort := uint16(port)
		mapped := false
		if tryUPnP {
			if gw, err := upnp.DiscoverGateway(4 * time.Second); err == nil {
				if err := gw.AddPortMapping(port, port, lanAddr(), "TCP", "LRM P2P:"+r.Config.User, 3600); err == nil {
					mapped = true
					ev.Rec.Net(fmt.Sprintf("share: UPnP mapped %d", port))
				} else {
					ev.Rec.Net(fmt.Sprintf("share: UPnP map failed: %v", shortErr(err)))
				}
			} else {
				ev.Rec.Net(fmt.Sprintf("share: UPnP discover failed: %v", shortErr(err)))
			}
			if !mapped {
				if gwIP, err := natpmp.DiscoverGateway(); err == nil {
					if res, err := natpmp.MapTCP(gwIP, uint16(port), uint16(port), 3600); err == nil {
						mappedPort = res.ExternalPort
						mapped = true
						ev.Rec.Net(fmt.Sprintf("share: NAT-PMP mapped %d", mappedPort))
					}
				}
			}
		}
		publicIP, err := stun.DiscoverPublicIP(nil, 6*time.Second)
		if err != nil || publicIP == nil {
			ev.Rec.Net(fmt.Sprintf("share: STUN failed: %v", shortErr(err)))
			return errMap("could not determine public IP (STUN failed)"), nil
		}
		key := portkey.Generate(r.Identity.Pub, publicIP, mappedPort)
		listening := probeListen(port)
		ev.Rec.Net(fmt.Sprintf("share: key for %s (mapped=%v listening=%v)", key.Addr(), mapped, listening))
		return okMap("key", key.Encode(), "human", key.Human(),
			"wan", key.Addr(), "mapped", mapped, "listening", listening), nil
	})
	reg("lrm_join", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_join", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "lrm_join() needs a port key string")
		}
		key, err := portkey.Decode(args[0].S)
		if err != nil {
			return errMap("bad port key: " + err.Error()), nil
		}
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		ev.Rec.Net(fmt.Sprintf("join: dialing %s", key.Addr()))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		sconn, err := transport.Dial(ctx, key.Addr(), r.Identity, r.Identity.Pub, key.PeerID)
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("join: dial failed: %v", shortErr(err)))
			return errMap("dial failed: " + err.Error()), nil
		}
		defer sconn.Close()
		sess := mux.NewSession(sconn, true)
		defer sess.Close()
		eng := lrmsync.New(r)
		res, err := eng.SyncWithSession(sess, true, "")
		if err != nil {
			ev.Rec.Net(fmt.Sprintf("join: sync failed: %v", shortErr(err)))
			return errMap("sync failed: " + err.Error()), nil
		}
		ev.Rec.Net(fmt.Sprintf("join: fetched=%d pushed=%d", res.Fetched, res.Pushed))
		return okMap("fetched", res.Fetched, "pushed", res.Pushed, "message", res.Message), nil
	})
	reg("lrm_sync", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("lrm_sync", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KStr {
			return Null, ev.errf(pos, "lrm_sync() needs a peer address string (\"\" = discover)")
		}
		peerAddr := args[0].S
		r, err := openLRMRepo(ev.WorkDir)
		if err != nil {
			return errMap(err.Error()), nil
		}
		defer r.Close()
		targets := []string{}
		if peerAddr != "" {
			targets = append(targets, peerAddr)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			peers, _ := mdns.Browse(ctx, 5*time.Second)
			cancel()
			self := r.Identity.HexID()
			for i := range peers {
				p := &peers[i]
				if p.PeerHex == self || p.Port == 0 || p.Addr() == "" {
					continue
				}
				targets = append(targets, p.Addr())
			}
		}
		if len(targets) == 0 {
			return errMap("no peers to sync with"), nil
		}
		fetched, pushed := 0, 0
		msgs := []string{}
		for _, t := range targets {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			sconn, err := transport.Dial(ctx, t, r.Identity, r.Identity.Pub, nil)
			cancel()
			if err != nil {
				msgs = append(msgs, t+": dial failed")
				ev.Rec.Net(fmt.Sprintf("sync %s: dial failed: %v", t, shortErr(err)))
				continue
			}
			sess := mux.NewSession(sconn, true)
			eng := lrmsync.New(r)
			res, err := eng.SyncWithSession(sess, true, "")
			_ = sess.Close()
			_ = sconn.Close()
			if err != nil {
				msgs = append(msgs, t+": sync failed")
				ev.Rec.Net(fmt.Sprintf("sync %s: failed: %v", t, shortErr(err)))
				continue
			}
			fetched += res.Fetched
			pushed += res.Pushed
			msgs = append(msgs, t+": "+res.Message)
			ev.Rec.Net(fmt.Sprintf("sync %s: fetched=%d pushed=%d", t, res.Fetched, res.Pushed))
		}
		return okMap("fetched", fetched, "pushed", pushed, "notes", strings.Join(msgs, "; ")), nil
	})
}

// failMap builds an {ok:false} map with extra keys (e.g. latency on failure).
func failMap(pairs ...any) Value {
	m := NewMap()
	m.M["ok"] = False
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			m.M[k] = toValue(pairs[i+1])
		}
	}
	return m
}

// sandbox guards all script file access to one directory tree.
type sandbox struct {
	root string // absolute, symlink-resolved
}

func newSandbox(root string) (*sandbox, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		if mkErr := os.MkdirAll(abs, 0o755); mkErr != nil {
			return nil, mkErr
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
	}
	return &sandbox{root: abs}, nil
}

// resolve maps a script path into the sandbox, rejecting escapes.
func (s *sandbox) resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.root, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// Resolve symlinks on the existing parent so ../ tricks can't escape.
	parent := abs
	if _, err := os.Lstat(abs); err != nil {
		parent = filepath.Dir(abs)
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		realParent = parent
	}
	real := abs
	if realParent != parent {
		real = filepath.Join(realParent, filepath.Base(abs))
	}
	if real != s.root && !strings.HasPrefix(real, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes sandbox: %q", p)
	}
	return real, nil
}

// openLRMRepo opens the repo containing dir (dir may be the root or below it).
func openLRMRepo(dir string) (*store.Repo, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = cwd
	}
	return store.Open(dir)
}

// lanAddr returns a LAN IPv4 for port-mapping calls.
func lanAddr() string {
	for _, a := range mdns.LocalAddrs() {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return a
		}
	}
	return ""
}

// probeListen reports whether something accepts TCP on localhost:port.
func probeListen(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func shortErr(err error) string {
	if err == nil {
		return "?"
	}
	s := err.Error()
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
