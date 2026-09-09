package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ReflogEntry records one branch-tip move (commit/amend/merge/reset/sync...).
type ReflogEntry struct {
	Time   time.Time
	OldHex string // 64 zeros when the branch was born
	NewHex string
	Action string // commit|amend|merge|reset --soft|sync|branch|...
	Detail string // first message line / target / peer
}

// ZeroHex is the old-value for a newly created ref.
const ZeroHex = "0000000000000000000000000000000000000000000000000000000000000000"

// AppendReflog records a tip move. Failures to log never fail the caller:
// ref history is advisory, the ref move itself already succeeded.
func (r *Repo) AppendReflog(branch, oldHex, newHex, action, detail string) {
	if branch == "" || strings.ContainsAny(branch, "/\\") {
		return
	}
	if oldHex == "" {
		oldHex = ZeroHex
	}
	detail = strings.ReplaceAll(strings.TrimSpace(firstLine(detail)), "\n", " ")
	line := fmt.Sprintf("%d %s %s %s\t%s\n", time.Now().UnixNano(), oldHex, newHex, action, detail)
	dir := filepath.Join(r.LrmDir, "logs", "refs")
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, branch), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// ReadReflog returns entries newest-first (lastN <= 0 means all).
func (r *Repo) ReadReflog(branch string, lastN int) ([]ReflogEntry, error) {
	if strings.ContainsAny(branch, "/\\") || branch == "" {
		return nil, fmt.Errorf("bad branch name %q", branch)
	}
	raw, err := os.ReadFile(filepath.Join(r.LrmDir, "logs", "refs", branch))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ReflogEntry
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		e, err := parseReflogLine(ln)
		if err != nil {
			continue // tolerate hand-edits; skip bad lines
		}
		out = append(out, e)
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if lastN > 0 && len(out) > lastN {
		out = out[:lastN]
	}
	return out, nil
}

func parseReflogLine(ln string) (ReflogEntry, error) {
	var e ReflogEntry
	head, detail, _ := strings.Cut(ln, "\t")
	e.Detail = detail
	parts := strings.SplitN(head, " ", 4)
	if len(parts) != 4 {
		return e, fmt.Errorf("bad entry")
	}
	nano, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return e, err
	}
	e.Time = time.Unix(0, nano)
	e.OldHex, e.NewHex, e.Action = parts[1], parts[2], parts[3]
	return e, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
