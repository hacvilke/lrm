package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/lrm-project/lrm/internal/update"
)

// cmdUpdate replaces this binary with the newest release.
//
// It is the same code the desktop panel calls, so a fix to one is a fix to
// both. Unlike the panel it can be scripted: `lrm update --check` exits 10
// when an update exists, which is what you want in a cron job.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	checkOnly := fs.Bool("check", false, "only report whether a newer release exists")
	repo := fs.String("repo", update.DefaultRepo, "release repository (owner/name)")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *checkOnly {
		f := &update.HTTPFetcher{}
		rel, err := f.LatestRelease(ctx, *repo)
		if err != nil {
			return err
		}
		self, _ := os.Executable()
		flavor := update.FlavorOf(self)
		plan := update.PlanFlavor(Version, rel, runtime.GOOS, runtime.GOARCH, flavor)
		if !plan.Available {
			fmt.Printf("up to date (%s is the newest release)\n", rel.TagName)
			return nil
		}
		fmt.Printf("update available: %s -> %s\n", Version, rel.TagName)
		fmt.Printf("  asset: %s\n", plan.AssetName)
		fmt.Printf("  notes: %s\n", plan.ReleaseURL)
		// Distinct exit code so a script can tell "newer exists" from
		// "up to date" without parsing English.
		os.Exit(10)
		return nil
	}

	res, err := update.Apply(ctx, update.Options{
		Repo:    *repo,
		Current: Version,
		Out:     os.Stdout,
	})
	if err != nil {
		return err
	}
	if !res.Updated {
		fmt.Println("already up to date (" + res.From + ")")
		return nil
	}
	fmt.Printf("updated %s -> %s\n", res.From, res.To)
	fmt.Println("run `lrm version` to confirm; restart any running daemon or panel")
	return nil
}
