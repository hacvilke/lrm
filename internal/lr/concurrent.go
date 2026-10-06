package lr

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Batch network builtins.
//
// A script that probes fifty hosts with a `for` loop does them one at a
// time, because LRS evaluates sequentially. That is the right design for
// the interpreter -- the Evaluator shares Globals, the recorder, the
// output writer and the loop/call depth counters, and Env is a plain map,
// so running loop bodies concurrently would be a data race, and the
// natural accumulator pattern (`down = down + 1`) is exactly what breaks.
// Making that safe means locking every variable access, which serialises
// evaluation again and buys nothing.
//
// The slow part was never evaluation; it is the network. So the
// concurrency lives here, inside Go, where it is contained: each builtin
// takes a list, fans the I/O out across a bounded worker pool, and returns
// an ordered list of results. The script stays single-threaded and no
// interpreter state is shared across goroutines.
//
// This is the same shape as `lrm scan` in lrm-mobile, which sweeps a /24
// with a worker pool rather than a loop.
//
// Every result list is returned in input order, so results[i] always
// corresponds to the i-th input regardless of which finished first.

// defaultFanout bounds in-flight connections. High enough to make a sweep
// fast, low enough not to exhaust file descriptors or look like a flood.
const defaultFanout = 32

// maxFanout caps what a script may ask for.
const maxFanout = 256

// fanout runs work(i) for each index below n, at most workers at a time,
// and returns once all have finished. Results are written by index, so no
// locking is needed around the output slice.
func fanout(n, workers int, work func(i int)) {
	if n <= 0 {
		return
	}
	if workers <= 0 {
		workers = defaultFanout
	}
	if workers > maxFanout {
		workers = maxFanout
	}
	if workers > n {
		workers = n
	}
	idx := make(chan int, n)
	for i := 0; i < n; i++ {
		idx <- i
	}
	close(idx)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				work(i)
			}
		}()
	}
	wg.Wait()
}

// strList reads a list-of-strings argument.
func strList(ev *Evaluator, v Value, who string, pos Pos) ([]string, *RuntimeError) {
	if v.Kind != KList {
		return nil, ev.errf(pos, "%s() needs a list of strings", who)
	}
	out := make([]string, 0, len(v.L))
	for _, e := range v.L {
		if e.Kind != KStr {
			return nil, ev.errf(pos, "%s() needs a list of strings, found %s", who, e.TypeName())
		}
		out = append(out, e.S)
	}
	return out, nil
}

// intList reads a list-of-numbers argument.
func intList(ev *Evaluator, v Value, who string, pos Pos) ([]int, *RuntimeError) {
	if v.Kind != KList {
		return nil, ev.errf(pos, "%s() needs a list of port numbers", who)
	}
	out := make([]int, 0, len(v.L))
	for _, e := range v.L {
		if e.Kind != KNum {
			return nil, ev.errf(pos, "%s() needs a list of port numbers, found %s", who, e.TypeName())
		}
		p := int(e.N)
		if p < 1 || p > 65535 {
			return nil, ev.errf(pos, "%s(): port %d is out of range (1-65535)", who, p)
		}
		out = append(out, p)
	}
	return out, nil
}

// optInt reads an optional numeric argument.
func optInt(args []Value, i, def int) int {
	if i < len(args) && args[i].Kind == KNum {
		return int(args[i].N)
	}
	return def
}

func registerConcurrent(reg func(string, func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError))) {
	// dns_lookup_all(hosts [, workers]) -- resolve many names at once.
	reg("dns_lookup_all", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("dns_lookup_all", args, 1, 2, pos); err != nil {
			return Null, err
		}
		hosts, err := strList(ev, args[0], "dns_lookup_all", pos)
		if err != nil {
			return Null, err
		}
		workers := optInt(args, 1, defaultFanout)

		type row struct {
			host string
			ips  []string
			ms   int64
			err  string
		}
		rows := make([]row, len(hosts))
		t0 := time.Now()
		ctx, cancel := deadlineCtx(ev, 20*time.Second)
		defer cancel()

		fanout(len(hosts), workers, func(i int) {
			h := hosts[i]
			s := time.Now()
			ips, e := net.DefaultResolver.LookupHost(ctx, h)
			rows[i] = row{host: h, ips: ips, ms: time.Since(s).Milliseconds()}
			if e != nil {
				rows[i].err = shortErr(e)
			}
		})

		out := make([]Value, 0, len(rows))
		failed := 0
		for _, r := range rows {
			m := NewMap()
			m.M["host"] = Str(r.host)
			m.M["latency_ms"] = Int(r.ms)
			if r.err != "" {
				failed++
				m.M["ok"] = False
				m.M["error"] = Str(r.err)
				ev.Rec.Net(fmt.Sprintf("dns %s: FAIL %s (%dms)", r.host, r.err, r.ms))
			} else {
				m.M["ok"] = True
				m.M["ips"] = toValue(r.ips)
				ev.Rec.Net(fmt.Sprintf("dns %s: OK %v (%dms)", r.host, r.ips, r.ms))
			}
			out = append(out, m)
		}
		ev.Rec.Net(fmt.Sprintf("dns_lookup_all: %d name(s), %d failed, %dms wall",
			len(hosts), failed, time.Since(t0).Milliseconds()))
		return okMap("results", out, "failed", failed, "count", len(hosts),
			"wall_ms", time.Since(t0).Milliseconds()), nil
	})

	// tcp_scan(hosts, ports [, timeout_ms [, workers]]) -- probe a matrix
	// of hosts x ports concurrently.
	reg("tcp_scan", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("tcp_scan", args, 2, 4, pos); err != nil {
			return Null, err
		}
		hosts, err := strList(ev, args[0], "tcp_scan", pos)
		if err != nil {
			return Null, err
		}
		ports, err := intList(ev, args[1], "tcp_scan", pos)
		if err != nil {
			return Null, err
		}
		timeout := capMS(ev, optInt(args, 2, 2000))
		if timeout <= 0 {
			return Null, &RuntimeError{Msg: "script timeout exceeded", Pos: pos}
		}
		workers := optInt(args, 3, defaultFanout)

		type probe struct {
			host string
			port int
			ms   int64
			open bool
			err  string
		}
		probes := make([]probe, 0, len(hosts)*len(ports))
		for _, h := range hosts {
			for _, p := range ports {
				probes = append(probes, probe{host: h, port: p})
			}
		}

		t0 := time.Now()
		fanout(len(probes), workers, func(i int) {
			pr := &probes[i]
			addr := net.JoinHostPort(pr.host, fmt.Sprint(pr.port))
			s := time.Now()
			c, e := net.DialTimeout("tcp", addr, time.Duration(timeout)*time.Millisecond)
			pr.ms = time.Since(s).Milliseconds()
			if e != nil {
				pr.err = shortErr(e)
				return
			}
			_ = c.Close()
			pr.open = true
		})

		results := make([]Value, 0, len(probes))
		openList := make([]Value, 0, 8)
		openCount := 0
		for _, pr := range probes {
			m := NewMap()
			m.M["host"] = Str(pr.host)
			m.M["port"] = Int(int64(pr.port))
			m.M["latency_ms"] = Int(pr.ms)
			m.M["ok"] = Bool(pr.open)
			if pr.open {
				openCount++
				o := NewMap()
				o.M["host"] = Str(pr.host)
				o.M["port"] = Int(int64(pr.port))
				o.M["latency_ms"] = Int(pr.ms)
				openList = append(openList, o)
				ev.Rec.Net(fmt.Sprintf("tcp %s:%d: OK (%dms)", pr.host, pr.port, pr.ms))
			} else {
				m.M["error"] = Str(pr.err)
				ev.Rec.Net(fmt.Sprintf("tcp %s:%d: FAIL %s (%dms)", pr.host, pr.port, pr.err, pr.ms))
			}
			results = append(results, m)
		}
		wall := time.Since(t0).Milliseconds()
		ev.Rec.Net(fmt.Sprintf("tcp_scan: %d probe(s), %d open, %dms wall", len(probes), openCount, wall))
		return okMap("results", results, "open", openList,
			"open_count", openCount, "closed_count", len(probes)-openCount,
			"count", len(probes), "wall_ms", wall), nil
	})

	// http_get_all(urls [, timeout_ms [, workers]]) -- fetch many URLs.
	reg("http_get_all", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("http_get_all", args, 1, 3, pos); err != nil {
			return Null, err
		}
		urls, err := strList(ev, args[0], "http_get_all", pos)
		if err != nil {
			return Null, err
		}
		timeout := capMS(ev, optInt(args, 1, 5000))
		if timeout <= 0 {
			return Null, &RuntimeError{Msg: "script timeout exceeded", Pos: pos}
		}
		workers := optInt(args, 2, defaultFanout)

		type row struct {
			url    string
			status int
			bytes  int
			ms     int64
			err    string
		}
		rows := make([]row, len(urls))
		ctx, cancel := deadlineCtx(ev, time.Duration(timeout)*time.Millisecond*time.Duration(len(urls)+1))
		defer cancel()
		client := &http.Client{Timeout: time.Duration(timeout) * time.Millisecond}

		t0 := time.Now()
		fanout(len(urls), workers, func(i int) {
			u := urls[i]
			s := time.Now()
			rows[i].url = u
			req, e := http.NewRequestWithContext(ctx, "GET", u, nil)
			if e != nil {
				rows[i].err = shortErr(e)
				rows[i].ms = time.Since(s).Milliseconds()
				return
			}
			resp, e := client.Do(req)
			if e != nil {
				rows[i].err = shortErr(e)
				rows[i].ms = time.Since(s).Milliseconds()
				return
			}
			defer resp.Body.Close()
			// Bound the read: a script asking for 50 URLs should not be
			// able to pull 50 unbounded bodies into memory.
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			rows[i].status = resp.StatusCode
			rows[i].bytes = len(b)
			rows[i].ms = time.Since(s).Milliseconds()
		})

		out := make([]Value, 0, len(rows))
		failed := 0
		for _, r := range rows {
			m := NewMap()
			m.M["url"] = Str(r.url)
			m.M["latency_ms"] = Int(r.ms)
			if r.err != "" {
				failed++
				m.M["ok"] = False
				m.M["error"] = Str(r.err)
				ev.Rec.Net(fmt.Sprintf("http %s: FAIL %s (%dms)", r.url, r.err, r.ms))
			} else {
				m.M["ok"] = True
				m.M["status"] = Int(int64(r.status))
				m.M["bytes"] = Int(int64(r.bytes))
				ev.Rec.Net(fmt.Sprintf("http %s: %d (%d bytes, %dms)", r.url, r.status, r.bytes, r.ms))
			}
			out = append(out, m)
		}
		wall := time.Since(t0).Milliseconds()
		ev.Rec.Net(fmt.Sprintf("http_get_all: %d url(s), %d failed, %dms wall", len(urls), failed, wall))
		return okMap("results", out, "failed", failed, "count", len(urls), "wall_ms", wall), nil
	})

	// sort_by_latency(results) -- convenience for ranking scan output.
	reg("sort_by_latency", func(ev *Evaluator, args []Value, pos Pos) (Value, *RuntimeError) {
		if err := arity("sort_by_latency", args, 1, 1, pos); err != nil {
			return Null, err
		}
		if args[0].Kind != KList {
			return Null, ev.errf(pos, "sort_by_latency() needs a list of result maps")
		}
		cp := make([]Value, len(args[0].L))
		copy(cp, args[0].L)
		sort.SliceStable(cp, func(i, j int) bool {
			return latencyOf(cp[i]) < latencyOf(cp[j])
		})
		return List(cp), nil
	})
}

func latencyOf(v Value) float64 {
	if v.Kind != KMap {
		return 1 << 30
	}
	if n, ok := v.M["latency_ms"]; ok && n.Kind == KNum {
		return n.N
	}
	return 1 << 30
}
