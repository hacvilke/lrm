package gitcompat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/store"
	"github.com/lrm-project/lrm/internal/sync"
)

// ScratchBranch hosts each bisect candidate (LRM has no detached HEAD,
// so bisection rides a real branch that reset deletes afterwards).
const ScratchBranch = "bisect-scratch"

// BisectState persists across `bisect good/bad` invocations.
type BisectState struct {
	OrigBranch string            `json:"orig_branch"`
	Bad        string            `json:"bad"`    // hex: asserted-bad commit
	Good       []string          `json:"good"`   // hex: asserted-good commits
	Tested     map[string]string `json:"tested"` // hex -> good|bad|skip
	Current    string            `json:"current"`
	Concluded  string            `json:"concluded,omitempty"` // definitive answer
}

// BisectStep is the outcome of one mark: next candidate or conclusion.
type BisectStep struct {
	Next     string   // hex to test ("" when concluded)
	Message  string   // human progress line
	Done     bool     // definitive single-culprit answer
	Culprit  string   // set when Done
	Range    []string // set when only skipped suspects remain
	Left     int      // suspects remaining
	Testable int
}

func bisectStatePath(r *store.Repo) string {
	return filepath.Join(r.LrmDir, "BISECT_STATE")
}

// BisectActive reports whether a session is in progress.
func BisectActive(r *store.Repo) bool {
	_, err := os.Stat(bisectStatePath(r))
	return err == nil
}

func loadBisect(r *store.Repo) (*BisectState, error) {
	raw, err := os.ReadFile(bisectStatePath(r))
	if err != nil {
		return nil, fmt.Errorf("no bisect in progress (lrm bisect start <bad> [good...])")
	}
	var st BisectState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("corrupt bisect state (lrm bisect reset to clear): %w", err)
	}
	if st.Tested == nil {
		st.Tested = map[string]string{}
	}
	return &st, nil
}

func saveBisect(r *store.Repo, st *BisectState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(bisectStatePath(r), raw, 0o644)
}

// BisectStart begins a session: bad is broken, goods (maybe none yet) work.
func BisectStart(r *store.Repo, bad cas.Hash, goods []cas.Hash) (*BisectStep, error) {
	if BisectActive(r) {
		return nil, fmt.Errorf("bisect already in progress (lrm bisect reset first)")
	}
	if branches, _ := r.ListBranches(); branches != nil {
		for _, b := range branches {
			if b == ScratchBranch {
				return nil, fmt.Errorf("branch %q exists — delete it first", ScratchBranch)
			}
		}
	}
	if err := requireClean(r); err != nil {
		return nil, err
	}
	badAnc, err := ancestors(r, bad)
	if err != nil {
		return nil, err
	}
	st := &BisectState{Bad: cas.Hex(bad), Tested: map[string]string{}}
	br, err := r.HeadBranch()
	if err != nil {
		return nil, err
	}
	st.OrigBranch = br
	for _, g := range goods {
		if g == bad {
			return nil, fmt.Errorf("a commit cannot be both good and bad")
		}
		if !badAnc[g] {
			return nil, fmt.Errorf("good %s is not an ancestor of bad %s", cas.Short(g), cas.Short(bad))
		}
		st.Good = append(st.Good, cas.Hex(g))
		st.Tested[cas.Hex(g)] = "good"
	}
	st.Tested[cas.Hex(bad)] = "bad"
	if err := saveBisect(r, st); err != nil {
		return nil, err
	}
	return bisectAdvance(r, st)
}

// BisectMark records good|bad|skip for the current (or given) commit.
func BisectMark(r *store.Repo, verdict string, ref cas.Hash, hasRef bool) (*BisectStep, error) {
	st, err := loadBisect(r)
	if err != nil {
		return nil, err
	}
	if st.Concluded != "" {
		return nil, fmt.Errorf("bisect already concluded (%s); lrm bisect reset", short12(st.Concluded))
	}
	target := st.Current
	if hasRef {
		target = cas.Hex(ref)
	}
	if target == "" {
		return nil, fmt.Errorf("nothing to mark (no current candidate?)")
	}
	suspects, err := suspectSet(r, st)
	if err != nil {
		return nil, err
	}
	th, err := cas.ParseHex(target)
	if err != nil {
		return nil, err
	}
	if !suspects[th] && verdict != "skip" {
		// Marking outside the range is allowed only when it still narrows;
		// otherwise it is almost certainly user confusion.
		if verdict == "good" {
			if !canNarrowGood(r, suspects, th) {
				return nil, fmt.Errorf("%s is outside the suspect range (no effect)", short12(target))
			}
		} else if !canNarrowBad(r, suspects, th) {
			return nil, fmt.Errorf("%s is outside the suspect range (no effect)", short12(target))
		}
	}
	st.Tested[target] = verdict
	if verdict == "good" && !hasRef {
		// keep explicit goods list tidy (informational only)
		st.Good = append(st.Good, target)
	}
	if err := saveBisect(r, st); err != nil {
		return nil, err
	}
	return bisectAdvance(r, st)
}

// bisectAdvance recomputes suspects and checks out the next candidate.
func bisectAdvance(r *store.Repo, st *BisectState) (*BisectStep, error) {
	suspects, err := suspectSet(r, st)
	if err != nil {
		return nil, err
	}
	if len(suspects) == 0 {
		return nil, fmt.Errorf("inconsistent marks (suspect set is empty); lrm bisect reset and retry")
	}
	if len(suspects) == 1 {
		for h := range suspects {
			st.Concluded = cas.Hex(h)
			st.Current = cas.Hex(h)
		}
		if err := checkoutScratch(r, st.Current); err != nil {
			return nil, err
		}
		_ = saveBisect(r, st)
		c, _ := r.DAG.Get(mustHash(st.Current))
		msg := fmt.Sprintf("%s is the first bad commit", short12(st.Concluded))
		if c != nil {
			msg += fmt.Sprintf(" — %s", firstLineOf(c.Message))
		}
		return &BisectStep{Done: true, Culprit: st.Concluded, Message: msg, Left: 1}, nil
	}
	order, err := topoOrder(r, suspects)
	if err != nil {
		return nil, err
	}
	var testable []cas.Hash
	skipped := 0
	for _, h := range order {
		switch st.Tested[cas.Hex(h)] {
		case "skip":
			skipped++
			continue
		case "good", "bad":
			continue // re-marking changes nothing; never re-pick
		}
		testable = append(testable, h)
	}
	if len(testable) == 0 {
		if skipped == 0 {
			return nil, fmt.Errorf("internal error: no testable suspects left")
		}
		hexes := make([]string, 0, len(suspects))
		for h := range suspects {
			hexes = append(hexes, cas.Hex(h)[:12])
		}
		sort.Strings(hexes)
		return &BisectStep{
			Message: fmt.Sprintf("only skipped suspects remain (%d); first bad is one of: %v", len(hexes), hexes),
			Range:   hexes, Left: len(suspects),
		}, nil
	}
	next := testable[len(testable)/2]
	if err := checkoutScratch(r, cas.Hex(next)); err != nil {
		return nil, err
	}
	st.Current = cas.Hex(next)
	_ = saveBisect(r, st)
	return &BisectStep{
		Next:     st.Current,
		Message:  fmt.Sprintf("testing %s (%d suspects, %d skipped) — mark with: lrm bisect good|bad|skip", short12(st.Current), len(suspects), skipped),
		Left:     len(suspects),
		Testable: len(testable),
	}, nil
}

// suspectSet narrows ancestors(bad) by every good/bad mark.
// Invariant: the true first-bad commit is always in the set (skips only
// remove testability, never suspicion).
func suspectSet(r *store.Repo, st *BisectState) (map[cas.Hash]bool, error) {
	bad, err := cas.ParseHex(st.Bad)
	if err != nil {
		return nil, err
	}
	set, err := ancestors(r, bad)
	if err != nil {
		return nil, err
	}
	for hx, v := range st.Tested {
		if v != "good" && v != "bad" {
			continue
		}
		h, err := cas.ParseHex(hx)
		if err != nil {
			continue
		}
		anc, err := ancestors(r, h)
		if err != nil {
			return nil, err
		}
		if v == "good" {
			for a := range anc {
				delete(set, a)
			}
		} else {
			for s := range set {
				if !anc[s] {
					delete(set, s)
				}
			}
		}
	}
	return set, nil
}

func canNarrowGood(r *store.Repo, set map[cas.Hash]bool, h cas.Hash) bool {
	anc, err := ancestors(r, h)
	if err != nil {
		return false
	}
	for a := range anc {
		if set[a] {
			return true
		}
	}
	return false
}

func canNarrowBad(r *store.Repo, set map[cas.Hash]bool, h cas.Hash) bool {
	anc, err := ancestors(r, h)
	if err != nil {
		return false
	}
	for s := range set {
		if !anc[s] {
			return true
		}
	}
	return false
}

// ancestors returns h plus every commit reachable from it.
func ancestors(r *store.Repo, h cas.Hash) (map[cas.Hash]bool, error) {
	hashes, _, err := r.DAG.WalkTipOrder(h, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[cas.Hash]bool, len(hashes))
	for _, x := range hashes {
		out[x] = true
	}
	return out, nil
}

// topoOrder linearizes set oldest-first (Kahn, timestamp+hex tie-break).
func topoOrder(r *store.Repo, set map[cas.Hash]bool) ([]cas.Hash, error) {
	stamps := map[cas.Hash]int64{}
	children := map[cas.Hash][]cas.Hash{}
	indeg := map[cas.Hash]int{}
	for h := range set {
		indeg[h] = 0
		c, err := r.DAG.Get(h)
		if err != nil {
			return nil, err
		}
		stamps[h] = c.Timestamp
	}
	for h := range set {
		c, err := r.DAG.Get(h)
		if err != nil {
			return nil, err
		}
		for _, ph := range c.Parents {
			p, err := cas.ParseHex(ph)
			if err != nil || !set[p] {
				continue
			}
			children[p] = append(children[p], h)
			indeg[h]++
		}
	}
	var ready []cas.Hash
	for h, d := range indeg {
		if d == 0 {
			ready = append(ready, h)
		}
	}
	less := func(a, b cas.Hash) bool {
		if stamps[a] != stamps[b] {
			return stamps[a] < stamps[b]
		}
		return cas.Hex(a) < cas.Hex(b)
	}
	var out []cas.Hash
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		h := ready[0]
		ready = ready[1:]
		out = append(out, h)
		for _, ch := range children[h] {
			indeg[ch]--
			if indeg[ch] == 0 {
				ready = append(ready, ch)
			}
		}
	}
	if len(out) != len(set) {
		return nil, fmt.Errorf("cycle in history (corrupt DAG?)")
	}
	return out, nil
}

// checkoutScratch puts the workdir on the scratch branch at hex.
func checkoutScratch(r *store.Repo, hex string) error {
	if err := requireClean(r); err != nil {
		return fmt.Errorf("cannot switch candidate: %w", err)
	}
	h, err := cas.ParseHex(hex)
	if err != nil {
		return err
	}
	if err := r.SetRef(ScratchBranch, hex); err != nil {
		return err
	}
	if err := r.SetHeadBranch(ScratchBranch); err != nil {
		return err
	}
	return sync.Checkout(r, h)
}

// BisectReset returns to the original branch and clears all state.
func BisectReset(r *store.Repo) (string, error) {
	st, err := loadBisect(r)
	if err != nil {
		return "", err
	}
	if err := requireClean(r); err != nil {
		return "", fmt.Errorf("cannot reset: %w (stash or commit first)", err)
	}
	_ = os.Remove(filepath.Join(r.LrmDir, "refs", "heads", ScratchBranch))
	if err := r.SetHeadBranch(st.OrigBranch); err != nil {
		return "", err
	}
	tipHex, _ := r.GetRef(st.OrigBranch)
	if tipHex != "" {
		h, err := cas.ParseHex(tipHex)
		if err != nil {
			return "", err
		}
		if err := sync.Checkout(r, h); err != nil {
			return "", err
		}
	}
	_ = os.Remove(bisectStatePath(r))
	return st.OrigBranch, nil
}

// BisectRun loops a command until the culprit is found. The command runs
// directly (no shell — wrap in sh -c for pipes); dir is the workdir.
// Exit 0 = good, 125 = skip, anything else = bad (git's convention).
func BisectRun(r *store.Repo, cmd []string, log func(string)) (*BisectStep, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("usage: lrm bisect run <command...>")
	}
	for {
		st, err := loadBisect(r)
		if err != nil {
			return nil, err
		}
		if st.Concluded != "" {
			c, _ := r.DAG.Get(mustHash(st.Concluded))
			msg := fmt.Sprintf("%s is the first bad commit", short12(st.Concluded))
			if c != nil {
				msg += fmt.Sprintf(" — %s", firstLineOf(c.Message))
			}
			return &BisectStep{Done: true, Culprit: st.Concluded, Message: msg}, nil
		}
		shell := exec.Command(cmd[0], cmd[1:]...)
		shell.Dir = r.Root
		out, err := shell.CombinedOutput()
		log(strings.TrimSpace(string(out)))
		verdict := "bad"
		if err == nil {
			verdict = "good"
		} else if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 125 {
			verdict = "skip"
		}
		log(fmt.Sprintf("→ %s (%s)", short12(st.Current), verdict))
		step, err := BisectMark(r, verdict, cas.Nil, false)
		if err != nil {
			return nil, err
		}
		log(step.Message)
		if step.Done || len(step.Range) > 0 {
			return step, nil
		}
	}
}

func requireClean(r *store.Repo) error {
	res, err := r.Index.Scan(r.Root, r.CAS)
	if err != nil {
		return err
	}
	if !res.Clean {
		return fmt.Errorf("workdir dirty (%d change(s)): commit or stash first", len(res.Changes))
	}
	return nil
}

func short12(hx string) string {
	if len(hx) > 12 {
		return hx[:12]
	}
	return hx
}

func mustHash(hx string) cas.Hash {
	h, _ := cas.ParseHex(hx)
	return h
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
