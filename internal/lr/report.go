package lr

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Entry is one recorded report line.
type Entry struct {
	Time time.Time
	Kind string // OUT, LOG, NET, ASSERT, ERROR, INFO
	Text string
}

// Recorder collects everything a script run should export to its .txt report:
// printed output, timestamped logs, network events, assertion results.
type Recorder struct {
	mu          sync.Mutex
	entries     []Entry
	AssertsPass int
	AssertsFail int
	Failures    []string
}

// NewRecorder creates an empty recorder.
func NewRecorder() *Recorder { return &Recorder{} }

func (r *Recorder) add(kind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, Entry{Time: time.Now(), Kind: kind, Text: text})
}

// Out records print() output.
func (r *Recorder) Out(s string) { r.add("OUT", s) }

// Log records log() output.
func (r *Recorder) Log(s string) { r.add("LOG", s) }

// Net records a network event (connects, fetches, syncs...).
func (r *Recorder) Net(s string) { r.add("NET", s) }

// Info records runner metadata (script path, verdict...).
func (r *Recorder) Info(s string) { r.add("INFO", s) }

// Error records a fatal script error.
func (r *Recorder) Error(s string) { r.add("ERROR", s) }

// Assert records one assertion outcome.
func (r *Recorder) Assert(pass bool, msg string, pos Pos) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pass {
		r.AssertsPass++
		r.entries = append(r.entries, Entry{Time: time.Now(), Kind: "ASSERT", Text: "PASS " + msg})
	} else {
		r.AssertsFail++
		line := fmt.Sprintf("FAIL %s (%s)", msg, pos)
		r.entries = append(r.entries, Entry{Time: time.Now(), Kind: "ASSERT", Text: line})
		r.Failures = append(r.Failures, line)
	}
}

// Render builds the full .txt report body.
func (r *Recorder) Render(script string, args []string, dur time.Duration, runErr error) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("LRM script report\n")
	sb.WriteString("=================\n")
	fmt.Fprintf(&sb, "script:   %s\n", script)
	fmt.Fprintf(&sb, "args:     %s\n", strings.Join(args, " "))
	fmt.Fprintf(&sb, "started:  %s\n", time.Now().Add(-dur).Format(time.RFC3339))
	fmt.Fprintf(&sb, "duration: %s\n", dur.Round(time.Millisecond))
	sb.WriteString("-----------------\n")
	for _, e := range r.entries {
		fmt.Fprintf(&sb, "[%s] %-6s %s\n", e.Time.Format("15:04:05"), e.Kind, e.Text)
	}
	sb.WriteString("-----------------\n")
	fmt.Fprintf(&sb, "asserts: %d passed, %d failed\n", r.AssertsPass, r.AssertsFail)
	if runErr != nil {
		fmt.Fprintf(&sb, "error:   %s\n", runErr.Error())
	}
	if runErr == nil && r.AssertsFail == 0 {
		sb.WriteString("verdict: PASS\n")
	} else {
		sb.WriteString("verdict: FAIL\n")
	}
	return sb.String()
}
