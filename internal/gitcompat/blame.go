package gitcompat

import (
	"fmt"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/diff"
	"github.com/lrm-project/lrm/internal/merkle"
	"github.com/lrm-project/lrm/internal/store"
)

// BlameLine attributes one tip line to the commit that introduced it.
type BlameLine struct {
	Hash   string // full hex of introducing commit
	Author string
	Time   int64 // unix nanos
	Lineno int   // 1-based at tip
	Text   string
}

// Blame attributes every line of path at tip, walking the first-parent
// chain (documented semantic: merges attribute through their first parent).
func Blame(r *store.Repo, tip cas.Hash, path string) ([]BlameLine, error) {
	tipFlat, err := TreeAtCommit(r, tip)
	if err != nil {
		return nil, err
	}
	tipHx, ok := tipFlat[path]
	if !ok {
		return nil, fmt.Errorf("no such file %q at %s", path, cas.Short(tip))
	}
	tipH, err := cas.ParseHex(tipHx)
	if err != nil {
		return nil, err
	}
	raw, err := ReadObjectBytes(r.CAS, tipH, MaxReadBytes)
	if err != nil {
		return nil, err
	}
	if diff.IsBinary(raw) {
		return nil, fmt.Errorf("cannot blame binary file %q", path)
	}
	tipLines := splitLines(string(raw))

	type live struct {
		num int // current line number in the commit being processed
		idx int // index into tipLines
	}
	lives := make([]live, 0, len(tipLines))
	for i := range tipLines {
		lives = append(lives, live{num: i, idx: i})
	}
	out := make([]BlameLine, len(tipLines))
	pending := len(tipLines)

	attr := func(c *dag.Commit, h cas.Hash) {
		for _, lv := range lives {
			if out[lv.idx].Hash != "" {
				continue
			}
			out[lv.idx] = BlameLine{
				Hash: cas.Hex(h), Author: c.Author, Time: c.Timestamp,
				Lineno: lv.idx + 1, Text: tipLines[lv.idx],
			}
			pending--
		}
	}

	cur := tip
	curC, err := r.DAG.Get(cur)
	if err != nil {
		return nil, err
	}
	curFlat := tipFlat
	for {
		if pending == 0 {
			break
		}
		if len(curC.Parents) == 0 {
			attr(curC, cur) // root: everything left was born here
			break
		}
		ph, err := cas.ParseHex(curC.Parents[0])
		if err != nil {
			return nil, err
		}
		pC, err := r.DAG.Get(ph)
		if err != nil {
			return nil, err
		}
		pth, err := cas.ParseHex(pC.Tree)
		if err != nil {
			return nil, err
		}
		parentFlat := map[string]string{}
		if err := merkle.Flatten(r.CAS, pth, "", parentFlat); err != nil {
			return nil, err
		}
		oldHx := parentFlat[path]
		newHx := curFlat[path]
		if newHx == "" {
			// File absent in cur (deleted then re-added later):
			// whatever survives was born in cur.
			attr(curC, cur)
			break
		}
		if oldHx == newHx {
			// Unchanged in cur: numbers carry over untouched.
			cur, curC, curFlat = ph, pC, parentFlat
			continue
		}
		var oldLines []string
		if oldHx != "" {
			oh, err := cas.ParseHex(oldHx)
			if err != nil {
				return nil, err
			}
			oraw, err := ReadObjectBytes(r.CAS, oh, MaxReadBytes)
			if err != nil {
				return nil, err
			}
			oldLines = splitLines(string(oraw))
		}
		newH, err := cas.ParseHex(newHx)
		if err != nil {
			return nil, err
		}
		nraw, err := ReadObjectBytes(r.CAS, newH, MaxReadBytes)
		if err != nil {
			return nil, err
		}
		newLines := splitLines(string(nraw))
		// Map each live line (numbered in new) back to old; '+' lines
		// were born in cur.
		byNew := map[int]int{} // newIdx -> position in lives
		for i, lv := range lives {
			if out[lv.idx].Hash == "" {
				byNew[lv.num] = i
			}
		}
		for _, o := range diff.DiffLineOps(oldLines, newLines) {
			switch o.Kind {
			case '+':
				if li, ok := byNew[o.NewIdx]; ok {
					lv := lives[li]
					out[lv.idx] = BlameLine{
						Hash: cas.Hex(cur), Author: curC.Author,
						Time: curC.Timestamp, Lineno: lv.idx + 1,
						Text: tipLines[lv.idx],
					}
					pending--
					delete(byNew, o.NewIdx)
				}
			case ' ':
				if li, ok := byNew[o.NewIdx]; ok {
					lives[li].num = o.OldIdx
				}
			}
		}
		cur, curC, curFlat = ph, pC, parentFlat
	}
	return out, nil
}

// splitLines splits text into lines, dropping the phantom empty element
// that follows a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// FormatBlame renders one line in classic blame style.
func FormatBlame(b BlameLine) string {
	return fmt.Sprintf("%s %s %s %d) %s",
		b.Hash[:12], b.Author,
		time.Unix(0, b.Time).Format("2006-01-02"),
		b.Lineno, b.Text)
}
