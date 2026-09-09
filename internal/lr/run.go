package lr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout bounds every script run (0 disables in Options).
const DefaultTimeout = 5 * time.Minute

// Options configures a script run.
type Options struct {
	WorkDir    string   // repo/workspace (default: cwd)
	Args       []string // script argv (args() builtin)
	ReportPath string   // txt report path (default: auto next to script)
	Timeout    time.Duration
	Out        io.Writer // script stdout (default: os.Stdout)
}

// Result summarizes a run. ReportPath always exists when a report could be
// written — including on parse/runtime errors.
type Result struct {
	Passed        bool
	AssertsPass   int
	AssertsFail   int
	ReportPath    string
	Error         string
	Duration      time.Duration
	ReportSkipped string // set when the report itself could not be written
}

// RunFile parses and executes a .lr script, always exporting a .txt report.
func RunFile(scriptPath string, opts Options) *Result {
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
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.ReportPath == "" {
		opts.ReportPath = defaultReportPath(scriptPath)
	}
	rec := NewRecorder()
	rec.Info("script " + scriptPath)
	finish := func(runErr error) *Result {
		res.Duration = time.Since(start)
		if runErr != nil {
			rec.Error(runErr.Error())
			res.Error = runErr.Error()
		}
		res.AssertsPass = rec.AssertsPass
		res.AssertsFail = rec.AssertsFail
		res.Passed = runErr == nil && rec.AssertsFail == 0
		body := rec.Render(scriptPath, opts.Args, res.Duration, runErr)
		if err := os.MkdirAll(filepath.Dir(opts.ReportPath), 0o755); err == nil {
			if err := os.WriteFile(opts.ReportPath, []byte(body), 0o644); err == nil {
				res.ReportPath = opts.ReportPath
			} else {
				res.ReportSkipped = err.Error()
			}
		} else {
			res.ReportSkipped = err.Error()
		}
		fmt.Fprintf(opts.Out, "---\nreport: %s  verdict: %s\n", res.ReportPath, verdictOf(res.Passed))
		return res
	}

	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		return finish(fmt.Errorf("cannot read script: %w", err))
	}
	prog, err := Parse(string(raw))
	if err != nil {
		return finish(err)
	}
	ev := NewEvaluator(opts.WorkDir, opts.Args, opts.Out, rec)
	if opts.Timeout > 0 {
		ev.Deadline = start.Add(opts.Timeout)
	}
	if err := ev.EvalProgram(prog); err != nil {
		return finish(err)
	}
	return finish(nil)
}

func verdictOf(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}

func defaultReportPath(scriptPath string) string {
	base := strings.TrimSuffix(filepath.Base(scriptPath), filepath.Ext(scriptPath))
	stamp := time.Now().Format("20060102_150405")
	name := fmt.Sprintf("%s_report_%s.txt", base, stamp)
	if dir := filepath.Dir(scriptPath); dir != "." && dir != "" {
		return filepath.Join(dir, name)
	}
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, name)
	}
	return name
}
