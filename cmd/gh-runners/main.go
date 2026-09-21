package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"gh-runners/internal/scan"
)

const header = "STATE\tREPO\tWORKFLOW\tBRANCH\tJOB\tRUNNER\tLABELS\tSINCE"

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func main() {
	org, args := splitOrg(os.Args[1:], envOrg())
	if org == "" {
		fmt.Fprintln(os.Stderr, "gh-runners: no org given; pass one as the first argument or set GH_RUNNERS_ORG")
		os.Exit(1)
	}

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gh-runners [org] [flags]")
		flag.PrintDefaults()
	}
	label := flag.String("label", "", "filter rows by case-insensitive substring")
	days := flag.Int("days", envDays(), "only scan repos pushed within N days (0 = all)")
	var selectMode bool
	flag.BoolVar(&selectMode, "select", false, "pick a job in fzf and open it in the browser")
	flag.BoolVar(&selectMode, "s", false, "pick a job in fzf and open it in the browser")
	flag.CommandLine.Parse(args)

	rows, scanErr := collect(org, *days, *label)

	if selectMode {
		if scanErr != nil {
			fmt.Fprintln(os.Stderr, "gh-runners: warning:", scanErr)
		}
		runSelect(rows)
		if scanErr != nil {
			os.Exit(2)
		}
		return
	}

	fmt.Println(header)
	for _, r := range rows {
		fmt.Println(strings.Join(r[:8], "\t"))
	}
	if scanErr != nil {
		fmt.Fprintln(os.Stderr, "gh-runners: warning:", scanErr)
		os.Exit(2)
	}
}

func splitOrg(argv []string, fallback string) (string, []string) {
	org := ""
	args := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			args = append(args, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(arg, "=") && (name == "label" || name == "days") && i+1 < len(argv) {
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

func envOrg() string {
	return os.Getenv("GH_RUNNERS_ORG")
}

func envDays() int {
	if v := os.Getenv("GH_RUNNERS_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 7
}

func collect(org string, days int, label string) ([][9]string, error) {
	var done, total atomic.Int64
	stop := startSpinner(org, &done, &total)
	jobs, scanErr := scan.Scan(context.Background(), org, days, func(d, t int) {
		done.Store(int64(d))
		total.Store(int64(t))
	})
	stop()

	needle := strings.ToLower(label)
	var rows [][9]string
	for _, j := range jobs {
		runner := j.Runner
		if runner == "" {
			runner = "-"
		}
		since := j.StartedAt
		if since.IsZero() {
			since = j.CreatedAt
		}
		row := [9]string{
			j.Status, j.Repo, j.Workflow, j.Branch, j.Name, runner, j.Labels,
			formatTime(since), j.URL,
		}
		if needle != "" && !strings.Contains(strings.ToLower(strings.Join(row[:], "\t")), needle) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(a, b int) bool {
		return strings.Join(rows[a][:], "\t") < strings.Join(rows[b][:], "\t")
	})
	return rows, scanErr
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z")
}

func startSpinner(org string, done, total *atomic.Int64) func() {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return func() {}
	}
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			case <-time.After(100 * time.Millisecond):
				if t := total.Load(); t < 0 {
					fmt.Fprintf(os.Stderr, "\r\033[K%s listing repos in %s", frames[i%len(frames)], org)
				} else {
					fmt.Fprintf(os.Stderr, "\r\033[K%s scanning %s  %d/%d repos",
						frames[i%len(frames)], org, done.Load(), t)
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-finished
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func runSelect(rows [][9]string) {
	if _, err := exec.LookPath("fzf"); err != nil {
		fmt.Fprintln(os.Stderr, "fzf not installed")
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "nothing running or queued")
		return
	}

	var in strings.Builder
	in.WriteString(header)
	in.WriteString("\n")
	for _, r := range rows {
		in.WriteString(strings.Join(r[:], "\t"))
		in.WriteString("\n")
	}

	cmd := exec.Command("fzf", "--delimiter=\t", "--with-nth=1..8", "--header-lines=1", "--tabstop=2", "--no-sort", "--reverse")
	cmd.Stdin = strings.NewReader(in.String())
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return
	}

	line := strings.TrimRight(string(out), "\n")
	if line == "" {
		return
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 9 || fields[8] == "" {
		return
	}
	exec.Command("open", fields[8]).Run()
}
