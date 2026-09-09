package merkle

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IgnoreRule is one parsed .lrmignore line (gitignore-lite).
type IgnoreRule struct {
	Pattern  string
	Negate   bool // !pattern re-includes
	DirOnly  bool // dir/ matches everything beneath
	Anchored bool // pattern contains / (after dir strip): match full path
	Line     int  // 1-based source line (0 when built by hand)
}

// Source reconstructs the rule's .lrmignore spelling for reports.
func (r IgnoreRule) Source() string {
	var sb strings.Builder
	if r.Negate {
		sb.WriteString("!")
	}
	if r.Anchored {
		sb.WriteString("/")
	}
	sb.WriteString(r.Pattern)
	if r.DirOnly {
		sb.WriteString("/")
	}
	return sb.String()
}

// Matcher applies .lrmignore rules. The zero value ignores nothing;
// Matcher only ADDS ignores — hardcoded safety names (.lrm, .git...)
// are handled separately and can never be un-ignored.
type Matcher struct {
	rules []IgnoreRule
}

// LoadIgnore reads root/.lrmignore (missing file → empty matcher).
func LoadIgnore(root string) *Matcher {
	m := &Matcher{}
	raw, err := os.ReadFile(filepath.Join(root, ".lrmignore"))
	if err != nil {
		return m
	}
	for i, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		var r IgnoreRule
		r.Line = i + 1
		if strings.HasPrefix(ln, "!") {
			r.Negate = true
			ln = strings.TrimSpace(ln[1:])
		}
		if ln == "" {
			continue
		}
		ln = strings.TrimPrefix(ln, "/")
		if strings.HasSuffix(ln, "/") {
			r.DirOnly = true
			ln = strings.TrimSuffix(ln, "/")
		}
		if strings.Contains(ln, "/") {
			r.Anchored = true
		}
		r.Pattern = ln
		m.rules = append(m.rules, r)
	}
	return m
}

// MatchRule returns the last rule deciding rel (nil when no rule matches).
// A returned Negate rule means "re-included" (not ignored).
func (m *Matcher) MatchRule(rel string) *IgnoreRule {
	if m == nil {
		return nil
	}
	rel = strings.TrimPrefix(rel, "./")
	var last *IgnoreRule
	for i := range m.rules {
		if m.rules[i].match(rel) {
			last = &m.rules[i]
		}
	}
	return last
}

// Ignored reports whether slash-relative rel (file or dir) is ignored.
// Last matching rule wins.
func (m *Matcher) Ignored(rel string) bool {
	if m == nil {
		return false
	}
	rel = strings.TrimPrefix(rel, "./")
	ignored := false
	for _, r := range m.rules {
		if r.match(rel) {
			ignored = !r.Negate
		}
	}
	return ignored
}

func (r IgnoreRule) match(rel string) bool {
	if r.Anchored {
		if r.DirOnly {
			return rel == r.Pattern || strings.HasPrefix(rel, r.Pattern+"/")
		}
		ok, _ := path.Match(r.Pattern, rel)
		return ok
	}
	// Unanchored: match the pattern against every self-or-ancestor
	// basename, so `build` ignores build/ and all beneath it.
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		if ok, _ := path.Match(r.Pattern, seg); ok {
			return true
		}
	}
	return false
}
