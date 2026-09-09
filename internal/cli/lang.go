package cli

import (
	"fmt"
	"time"

	"github.com/lrm-project/lrm/internal/lr"
	"github.com/lrm-project/lrm/internal/lrq"
)

// cmdRun executes an LRS script: lrm run <script.lr> [--report P] [--timeout D] [-- args...]
func cmdRun(args []string) error {
	report, args := flagVal(args, "--report")
	timeoutStr, args := flagVal(args, "--timeout")
	var scriptArgs []string
	for i, a := range args {
		if a == "--" {
			scriptArgs = args[i+1:]
			args = args[:i]
			break
		}
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm run <script.lr> [--report PATH] [--timeout 30s] [-- args...]")
	}
	var timeout time.Duration
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return fmt.Errorf("bad --timeout %q (want like 30s, 2m)", timeoutStr)
		}
		timeout = d
	}
	res := lr.RunFile(args[0], lr.Options{ReportPath: report, Timeout: timeout, Args: scriptArgs})
	if !res.Passed {
		if res.Error != "" {
			return fmt.Errorf("script FAILED: %s", res.Error)
		}
		return fmt.Errorf("script FAILED: %d assertion(s) failed (report: %s)", res.AssertsFail, res.ReportPath)
	}
	return nil
}

// cmdQuery executes an LRQ query file: lrm query <q.lrq> [--report PATH]
func cmdQuery(args []string) error {
	report, args := flagVal(args, "--report")
	if len(args) == 0 {
		return fmt.Errorf("usage: lrm query <queries.lrq> [--report PATH]")
	}
	res := lrq.RunFile(args[0], lrq.Options{ReportPath: report})
	if !res.Passed {
		return fmt.Errorf("query FAILED: %s", res.Error)
	}
	return nil
}
