// Package lrq implements LRQ — the LRM Query language (.lrq files).
//
// LRQ is the declarative, read-only twin of LRS: instead of writing
// automation, you ask questions and get tables:
//
//	$ lrm query inspect.lrq --report out.txt
//	$ lrm inspect.lrq              # bare form also works
//
//	peers limit 5;
//	status;
//	commits on "main" limit 5;
//	files at "main" bigger than 1MB;
//	find "test" in "main";
//
// Keywords are case-insensitive; every statement ends with ';'.
// Like LRS runs, every query run exports a .txt report.
package lrq

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
)

// ---------- tables ----------

// Table is one rendered result set.
type Table struct {
	Title  string
	Header []string
	Rows   [][]string
}

// Render aligns columns with two-space gutters.
func (t Table) Render() string {
	widths := make([]int, len(t.Header))
	for i, h := range t.Header {
		widths[i] = len(h)
	}
	for _, r := range t.Rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "== %s (%d row(s)) ==\n", t.Title, len(t.Rows))
	if len(t.Rows) == 0 {
		sb.WriteString("(empty)\n")
		return sb.String()
	}
	for i, h := range t.Header {
		if i > 0 {
			sb.WriteString("  ")
		}
		sb.WriteString(padRight(h, widths[i]))
	}
	sb.WriteString("\n")
	for _, r := range t.Rows {
		for i := range t.Header {
			if i > 0 {
				sb.WriteString("  ")
			}
			c := ""
			if i < len(r) {
				c = r[i]
			}
			sb.WriteString(padRight(c, widths[i]))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func padRight(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s
}

// ---------- lexer ----------

type tok struct {
	text  string // raw (strings unquoted)
	num   int64
	isNo  bool // numeric
	isStr bool // quoted string
	pos   int  // byte offset (for errors)
}

func lex(src string) ([]tok, error) {
	var out []tok
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '#' || (c == '/' && i+1 < len(src) && src[i+1] == '/') {
			// Comment to end of line (# or //).
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == ';' {
			out = append(out, tok{text: ";", pos: i})
			i++
			continue
		}
		if c == '"' || c == '\'' {
			q := c
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != q {
				if src[j] == '\\' && j+1 < len(src) {
					sb.WriteByte(src[j+1])
					j += 2
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string at offset %d", i)
			}
			out = append(out, tok{text: sb.String(), isStr: true, pos: i})
			i = j + 1
			continue
		}
		if isTokDigit(c) {
			j := i
			for j < len(src) && (isTokDigit(src[j])) {
				j++
			}
			// Optional fraction (1.5MB) then optional glued unit letters.
			if j < len(src) && src[j] == '.' && j+1 < len(src) && isTokDigit(src[j+1]) {
				j++
				for j < len(src) && isTokDigit(src[j]) {
					j++
				}
			}
			k := j
			for k < len(src) && isTokLetter(src[k]) {
				k++
			}
			// Hash prefixes starting with a digit (3f29e8de) are one word:
			// extend through any further mixed alphanumerics.
			for k < len(src) && (isTokLetter(src[k]) || isTokDigit(src[k])) {
				k++
			}
			word := src[i:k]
			if n, err := strconv.ParseInt(word, 10, 64); err == nil {
				out = append(out, tok{text: word, num: n, isNo: true, pos: i})
			} else {
				out = append(out, tok{text: word, pos: i})
			}
			i = k
			continue
		}
		if isTokLetter(c) || c == '_' || c == '.' || c == '-' || c == '/' {
			j := i
			for j < len(src) && (isTokLetter(src[j]) || isTokDigit(src[j]) || src[j] == '_' || src[j] == '.' || src[j] == '-' || src[j] == '/') {
				j++
			}
			// A bare hash prefix may start with digits — already handled above
			// only when ALL digits; mixed alnum falls here via letters.
			out = append(out, tok{text: src[i:j], pos: i})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %q at offset %d", c, i)
	}
	return out, nil
}

func isTokLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isTokDigit(c byte) bool  { return c >= '0' && c <= '9' }

// ---------- parser ----------

// Query is one parsed statement.
type Query struct {
	Kind    string // peers|status|branches|tags|commits|files|show|find
	Ref     string // branch/tag/hash or "" (default HEAD branch)
	Pattern string // LIKE glob (files) or substring (find)
	Bigger  int64  // -1 = unused
	Small   int64  // -1 = unused
	Limit   int
}

func (q Query) kw(t tok) string { return strings.ToLower(t.text) }

// Parse parses LRQ source into queries.
func Parse(src string) ([]Query, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &qparser{toks: toks}
	var out []Query
	for p.i < len(p.toks) {
		q, err := p.parseOne()
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no queries (empty file?)")
	}
	return out, nil
}

type qparser struct {
	toks []tok
	i    int
}

func (p *qparser) peek() (tok, bool) {
	if p.i < len(p.toks) {
		return p.toks[p.i], true
	}
	return tok{}, false
}

func (p *qparser) next() (tok, bool) {
	t, ok := p.peek()
	if ok {
		p.i++
	}
	return t, ok
}

func (p *qparser) kwMatches(word string) bool {
	t, ok := p.peek()
	return ok && strings.ToLower(t.text) == word && !t.isNo && !t.isStr
}

func (p *qparser) eat(word string) bool {
	if p.kwMatches(word) {
		p.i++
		return true
	}
	return false
}

func (p *qparser) expectSemi() error {
	t, ok := p.next()
	if !ok || t.text != ";" {
		return fmt.Errorf("expected ';' to end the query")
	}
	return nil
}

func (p *qparser) parseOne() (Query, error) {
	q := Query{Bigger: -1, Small: -1, Limit: 50}
	t, ok := p.next()
	if !ok {
		return q, fmt.Errorf("unexpected end of file")
	}
	kw := strings.ToLower(t.text)
	if kw == "from" {
		nt, ok := p.next()
		if !ok || nt.text == ";" {
			return q, fmt.Errorf("FROM needs a table name")
		}
		kw = strings.ToLower(nt.text)
	}
	// Singular aliases.
	switch kw {
	case "peer":
		kw = "peers"
	case "branch":
		kw = "branches"
	case "tag":
		kw = "tags"
	case "commit":
		kw = "commits"
	case "file":
		kw = "files"
	}
	switch kw {
	case "peers":
		q.Kind = "peers"
		if err := p.parseLimit(&q); err != nil {
			return q, err
		}
		return q, p.expectSemi()
	case "status":
		q.Kind = "status"
		return q, p.expectSemi()
	case "branches":
		q.Kind = "branches"
		return q, p.expectSemi()
	case "tags":
		q.Kind = "tags"
		return q, p.expectSemi()
	case "commits":
		q.Kind = "commits"
		if p.eat("on") {
			ref, err := p.parseRef()
			if err != nil {
				return q, err
			}
			q.Ref = ref
		}
		if err := p.parseLimit(&q); err != nil {
			return q, err
		}
		return q, p.expectSemi()
	case "files":
		q.Kind = "files"
		if p.eat("at") {
			ref, err := p.parseRef()
			if err != nil {
				return q, err
			}
			q.Ref = ref
		}
		if p.eat("path") {
			if !p.eat("like") {
				return q, fmt.Errorf("expected LIKE after PATH")
			}
			pat, ok := p.next()
			if !ok || !pat.isStr {
				return q, fmt.Errorf("LIKE needs a quoted glob, e.g. LIKE \"src/*.go\"")
			}
			q.Pattern = pat.text
		}
		if p.eat("bigger") {
			if !p.eat("than") {
				return q, fmt.Errorf("expected THAN after BIGGER")
			}
			n, err := p.parseSize()
			if err != nil {
				return q, err
			}
			q.Bigger = n
		} else if p.eat("smaller") {
			if !p.eat("than") {
				return q, fmt.Errorf("expected THAN after SMALLER")
			}
			n, err := p.parseSize()
			if err != nil {
				return q, err
			}
			q.Small = n
		}
		if err := p.parseLimit(&q); err != nil {
			return q, err
		}
		return q, p.expectSemi()
	case "show":
		q.Kind = "show"
		ref, err := p.parseRef()
		if err != nil {
			return q, err
		}
		q.Ref = ref
		return q, p.expectSemi()
	case "find":
		q.Kind = "find"
		pat, ok := p.next()
		if !ok {
			return q, fmt.Errorf("FIND needs a substring")
		}
		q.Pattern = pat.text
		if p.eat("in") {
			ref, err := p.parseRef()
			if err != nil {
				return q, err
			}
			q.Ref = ref
		}
		if err := p.parseLimit(&q); err != nil {
			return q, err
		}
		return q, p.expectSemi()
	}
	return q, fmt.Errorf("unknown query %q (want PEERS|STATUS|BRANCHES|TAGS|COMMITS|FILES|SHOW|FIND)", t.text)
}

func (p *qparser) parseRef() (string, error) {
	t, ok := p.next()
	if !ok || t.text == ";" {
		return "", fmt.Errorf("expected a branch, tag or commit hash")
	}
	return t.text, nil
}

func (p *qparser) parseLimit(q *Query) error {
	if !p.eat("limit") {
		return nil
	}
	t, ok := p.next()
	if !ok || !t.isNo {
		return fmt.Errorf("LIMIT needs a number")
	}
	if t.num <= 0 || t.num > 500 {
		return fmt.Errorf("LIMIT must be 1..500")
	}
	q.Limit = int(t.num)
	return nil
}

func (p *qparser) parseSize() (int64, error) {
	t, ok := p.next()
	if !ok {
		return 0, fmt.Errorf("expected a size like 1MB")
	}
	if t.isNo {
		return t.num, nil
	}
	return parseSizeStr(t.text)
}

// parseSizeStr parses "512", "10KB", "1MB", "2GB" (case-insensitive).
func parseSizeStr(s string) (int64, error) {
	up := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	num := up
	for _, suf := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(up, suf.s) {
			mult = suf.m
			num = strings.TrimSpace(up[:len(up)-len(suf.s)])
			break
		}
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad size %q (want like 512, 10KB, 1.5MB)", s)
	}
	return int64(f * float64(mult)), nil
}

// ---------- engine ----------

// Options configures a query run.
type Options struct {
	WorkDir    string
	ReportPath string
	Out        io.Writer
}

// Result summarizes a query run.
type Result struct {
	Passed     bool
	Tables     []Table
	ReportPath string
	Error      string
	Duration   time.Duration
}

// RunFile parses and executes a .lrq file, exporting a .txt report.
func RunFile(path string, opts Options) *Result {
	start := time.Now()
	res := &Result{}
	if opts.WorkDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.WorkDir = cwd
		} else {
			opts.WorkDir = "."
		}
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ReportPath == "" {
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		name := fmt.Sprintf("%s_query_%s.txt", base, time.Now().Format("20060102_150405"))
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			opts.ReportPath = filepath.Join(dir, name)
		} else if cwd, err := os.Getwd(); err == nil {
			opts.ReportPath = filepath.Join(cwd, name)
		} else {
			opts.ReportPath = name
		}
	}
	finish := func(runErr error) *Result {
		res.Duration = time.Since(start)
		if runErr != nil {
			res.Error = runErr.Error()
		}
		res.Passed = runErr == nil
		body := renderReport(path, res.Tables, res.Duration, runErr)
		if err := os.MkdirAll(filepath.Dir(opts.ReportPath), 0o755); err == nil {
			if err := os.WriteFile(opts.ReportPath, []byte(body), 0o644); err == nil {
				res.ReportPath = opts.ReportPath
			}
		}
		fmt.Fprint(opts.Out, body)
		if res.ReportPath != "" {
			fmt.Fprintf(opts.Out, "---\nreport: %s\n", res.ReportPath)
		}
		return res
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return finish(fmt.Errorf("cannot read query file: %w", err))
	}
	queries, err := Parse(string(raw))
	if err != nil {
		return finish(err)
	}
	r, err := store.Open(opts.WorkDir)
	if err != nil {
		return finish(err)
	}
	defer r.Close()
	eng := &engine{repo: r}
	for _, q := range queries {
		tbl, err := eng.exec(q)
		if err != nil {
			return finish(fmt.Errorf("%s: %w", q.Kind, err))
		}
		res.Tables = append(res.Tables, tbl)
	}
	return finish(nil)
}

func renderReport(path string, tables []Table, dur time.Duration, runErr error) string {
	var sb strings.Builder
	sb.WriteString("LRM query report\n================\n")
	fmt.Fprintf(&sb, "query file: %s\nduration:   %s\n----------------\n", path, dur.Round(time.Millisecond))
	for _, t := range tables {
		sb.WriteString(t.Render())
		sb.WriteString("\n")
	}
	if runErr != nil {
		fmt.Fprintf(&sb, "error: %s\n", runErr.Error())
		sb.WriteString("verdict: FAIL\n")
	} else {
		sb.WriteString("verdict: PASS\n")
	}
	return sb.String()
}

type engine struct {
	repo *store.Repo
}

func (e *engine) exec(q Query) (Table, error) {
	switch q.Kind {
	case "peers":
		return e.qPeers(q)
	case "status":
		return e.qStatus()
	case "branches":
		return e.qBranches()
	case "tags":
		return e.qTags()
	case "commits":
		return e.qCommits(q)
	case "files":
		return e.qFiles(q)
	case "show":
		return e.qShow(q)
	case "find":
		return e.qFind(q)
	}
	return Table{}, fmt.Errorf("unknown query %s", q.Kind)
}

// resolveRef maps branch/tag/hash (or "" = current branch tip) to a commit.
func (e *engine) resolveRef(ref string) (cas.Hash, error) {
	if ref == "" {
		h, br, err := e.repo.HeadCommit()
		if err != nil {
			return cas.Nil, err
		}
		if h == cas.Nil {
			return cas.Nil, fmt.Errorf("branch %s has no commits yet", br)
		}
		return h, nil
	}
	// Branch?
	if hexStr, _ := e.repo.GetRef(ref); hexStr != "" {
		h, err := cas.ParseHex(hexStr)
		if err != nil {
			return cas.Nil, err
		}
		return h, nil
	}
	// Tag?
	if hexStr, _ := e.repo.GetTag(ref); hexStr != "" {
		h, err := cas.ParseHex(hexStr)
		if err != nil {
			return cas.Nil, err
		}
		return h, nil
	}
	// Hash (full or unique prefix)?
	h, err := e.repo.CAS.Parse(ref)
	if err != nil {
		return cas.Nil, fmt.Errorf("unknown ref %q (no branch/tag/hash)", ref)
	}
	if !e.repo.DAG.Has(h) {
		return cas.Nil, fmt.Errorf("ref %q is not a commit", ref)
	}
	return h, nil
}

func shortHash(h cas.Hash) string { return cas.Hex(h)[:8] }
