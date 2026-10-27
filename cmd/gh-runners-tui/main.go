package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gh-runners/internal/config"
	"gh-runners/internal/scan"
)

func main() {
	cfg := config.Load()

	org, args := splitOrg(os.Args[1:], envOrg(cfg))

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gh-runner [org] [flags]")
		flag.PrintDefaults()
	}
	label := flag.String("label", "", "filter jobs by runner label substring")
	days := flag.Int("days", envDays(cfg), "only scan repos pushed within N days (0 = all)")
	interval := flag.Int("interval", 15, "rescan interval in seconds")
	repo := flag.String("repo", "", "scan only this repo (owner/repo or a bare name) and highlight it")
	once := flag.Bool("once", false, "print tables once and exit")
	flag.CommandLine.Parse(args)

	opts := options{org: org, label: *label, days: *days, interval: *interval, repo: *repo}

	if *once {
		if org == "" {
			fmt.Fprintln(os.Stderr, "gh-runner: "+noOrgHint())
			os.Exit(1)
		}
		printOnce(opts)
		return
	}

	if err := Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "gh-runner:", err)
		os.Exit(1)
	}
}

func noOrgHint() string {
	if path := config.Path(); path != "" {
		return "no org given; pass one as the first argument, set GH_RUNNERS_ORG, or set org in " + path
	}
	return "no org given; pass one as the first argument or set GH_RUNNERS_ORG"
}

func envOrg(cfg config.Config) string {
	if v := os.Getenv("GH_RUNNERS_ORG"); v != "" {
		return v
	}
	return cfg.Org
}

func envDays(cfg config.Config) int {
	if v := os.Getenv("GH_RUNNERS_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if cfg.DaysSet {
		return cfg.Days
	}
	return 7
}

func splitOrg(argv []string, fallback string) (string, []string) {
	org := ""
	args := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			args = append(args, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(arg, "=") && (name == "label" || name == "days" || name == "interval" || name == "repo") && i+1 < len(argv) {
				i++
				args = append(args, argv[i])
			}
			continue
		}
		if org == "" {
			org = arg
			continue
		}
		args = append(args, arg)
	}
	if org == "" {
		org = fallback
	}
	return org, args
}

func scanTarget(ctx context.Context, opts options, progress func(done, total int)) ([]scan.Job, error) {
	if opts.repo != "" {
		return scan.ScanRepo(ctx, opts.org, opts.repo, progress)
	}
	return scan.Scan(ctx, opts.org, opts.days, progress)
}

func printOnce(opts options) {
	jobs, scanErr := scanLabeled(context.Background(), opts, func(done, total int) {
		if total < 0 {
			fmt.Fprintf(os.Stderr, "\r\033[Klisting repos in %s", opts.org)
			return
		}
		fmt.Fprintf(os.Stderr, "\r\033[Kscanning %s  %d/%d repos", opts.org, done, total)
	})
	fmt.Fprint(os.Stderr, "\r\033[K")
	scan.SaveCache()

	running, queued := scan.SplitJobs(jobs)
	now := time.Now().UTC()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "Running (%d)\n", len(running))
	fmt.Fprintln(w, strings.Join(runningCols, "\t"))
	for _, j := range running {
		elapsed := "-"
		if !j.StartedAt.IsZero() {
			elapsed = fmtElapsed(now.Sub(j.StartedAt))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Runner, j.Repo, j.Workflow, j.Branch, j.Name, elapsed)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Queued (%d)\n", len(queued))
	fmt.Fprintln(w, strings.Join(queuedCols, "\t"))
	for _, j := range queued {
		wait := "-"
		if !j.CreatedAt.IsZero() {
			wait = fmt.Sprintf("%dm", int(now.Sub(j.CreatedAt).Minutes()))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", wait, j.Repo, j.Workflow, j.Branch, j.Name, j.Labels)
	}

	w.Flush()

	if scanErr != nil {
		fmt.Fprintln(os.Stderr, "gh-runner: warning:", scanErr)
	}
}

func scanLabeled(ctx context.Context, opts options, progress func(done, total int)) ([]scan.Job, error) {
	jobs, scanErr := scanTarget(ctx, opts, progress)
	if opts.label == "" {
		return jobs, scanErr
	}
	needle := strings.ToLower(opts.label)
	var kept []scan.Job
	for _, j := range jobs {
		if strings.Contains(strings.ToLower(j.Labels), needle) {
			kept = append(kept, j)
		}
	}
	return kept, scanErr
}
