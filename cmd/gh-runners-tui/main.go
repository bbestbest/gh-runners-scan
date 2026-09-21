package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gh-runners/internal/scan"
)

func main() {
	org, args := splitOrg(os.Args[1:], envOrg())
	if org == "" {
		fmt.Fprintln(os.Stderr, "gh-runners-tui: no org given; pass one as the first argument or set GH_RUNNERS_ORG")
		os.Exit(1)
	}

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gh-runners-tui [org] [flags]")
		flag.PrintDefaults()
	}
	label := flag.String("label", "", "filter jobs by runner label substring")
	days := flag.Int("days", 7, "only scan repos pushed within N days (0 = all)")
	interval := flag.Int("interval", 150, "rescan interval in seconds")
	repo := flag.String("repo", "", "highlight this repo")
	once := flag.Bool("once", false, "print tables once and exit")
	flag.CommandLine.Parse(args)

	opts := options{org: org, label: *label, days: *days, interval: *interval, repo: *repo}

	if *once {
		printOnce(opts)
		return
	}

	if err := Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "gh-runners-tui:", err)
		os.Exit(1)
	}
}

func envOrg() string {
	return os.Getenv("GH_RUNNERS_ORG")
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

func printOnce(opts options) {
	jobs := scanLabeled(context.Background(), opts, func(done, total int) {
		if total < 0 {
			fmt.Fprintf(os.Stderr, "\r\033[Klisting repos in %s", opts.org)
			return
		}
		fmt.Fprintf(os.Stderr, "\r\033[Kscanning %s  %d/%d repos", opts.org, done, total)
	})
	fmt.Fprint(os.Stderr, "\r\033[K")

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
}

func scanLabeled(ctx context.Context, opts options, progress func(done, total int)) []scan.Job {
	jobs := scan.Scan(ctx, opts.org, opts.days, progress)
	if opts.label == "" {
		return jobs
	}
	needle := strings.ToLower(opts.label)
	var kept []scan.Job
	for _, j := range jobs {
		if strings.Contains(strings.ToLower(j.Labels), needle) {
			kept = append(kept, j)
		}
	}
	return kept
}
