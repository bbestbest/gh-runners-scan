package scan

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const workers = 6

const rateLimitAttempts = 3

const rateLimitBackoff = 3 * time.Second

const repoCacheTTL = 12 * time.Minute

var ErrRateLimited = errors.New("github api rate limited; results are incomplete")

type Job struct {
	Status    string
	Repo      string
	Workflow  string
	Branch    string
	Name      string
	Runner    string
	Labels    string
	StartedAt time.Time
	CreatedAt time.Time
	URL       string
}

type repoEntry struct {
	Name     string `json:"name"`
	PushedAt string `json:"pushedAt"`
}

type runEntry struct {
	DatabaseID int64 `json:"databaseId"`
}

type jobsResponse struct {
	Jobs []struct {
		Status       string   `json:"status"`
		WorkflowName string   `json:"workflow_name"`
		HeadBranch   string   `json:"head_branch"`
		Name         string   `json:"name"`
		RunnerName   string   `json:"runner_name"`
		Labels       []string `json:"labels"`
		StartedAt    string   `json:"started_at"`
		CreatedAt    string   `json:"created_at"`
		HTMLURL      string   `json:"html_url"`
	} `json:"jobs"`
}

func isRateLimit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	text := strings.ToLower(string(exitErr.Stderr))
	return strings.Contains(text, "rate limit") ||
		strings.Contains(text, "secondary rate") ||
		strings.Contains(text, "403")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func gh(ctx context.Context, limited *atomic.Bool, out any, args ...string) bool {
	for attempt := 0; ; attempt++ {
		data, err := exec.CommandContext(ctx, "gh", args...).Output()
		if err == nil {
			return json.Unmarshal(data, out) == nil
		}
		if !isRateLimit(err) {
			return false
		}
		if attempt >= rateLimitAttempts-1 {
			limited.Store(true)
			return false
		}
		if !sleepCtx(ctx, time.Duration(attempt+1)*rateLimitBackoff) {
			limited.Store(true)
			return false
		}
	}
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

type repoCacheEntry struct {
	names   []string
	expires time.Time
}

var (
	repoCacheMu sync.Mutex
	repoCache   = map[string]repoCacheEntry{}
)

func cachedRepos(key string) ([]string, bool) {
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	entry, ok := repoCache[key]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.names, true
}

func storeRepos(key string, names []string) {
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	repoCache[key] = repoCacheEntry{names: names, expires: time.Now().Add(repoCacheTTL)}
}

func listRepos(ctx context.Context, limited *atomic.Bool, org string, days int) []string {
	key := org + "\x00" + strconv.Itoa(days)
	if names, ok := cachedRepos(key); ok {
		return names
	}

	var entries []repoEntry
	if !gh(ctx, limited, &entries, "repo", "list", org, "--limit", "100", "--no-archived", "--json", "name,pushedAt") {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if days <= 0 {
			names = append(names, e.Name)
			continue
		}
		if ts := parseTime(e.PushedAt); !ts.IsZero() && ts.After(cutoff) {
			names = append(names, e.Name)
		}
	}
	storeRepos(key, names)
	return names
}

func scanRepo(ctx context.Context, limited *atomic.Bool, org, repo string) []Job {
	var jobs []Job
	for _, status := range []string{"in_progress", "queued"} {
		var runs []runEntry
		if !gh(ctx, limited, &runs, "run", "list", "-R", org+"/"+repo, "--status", status, "--limit", "20", "--json", "databaseId") {
			continue
		}
		for _, run := range runs {
			var resp jobsResponse
			path := "repos/" + org + "/" + repo + "/actions/runs/" + strconv.FormatInt(run.DatabaseID, 10) + "/jobs"
			if !gh(ctx, limited, &resp, "api", path) {
				continue
			}
			for _, j := range resp.Jobs {
				if j.Status == "completed" {
					continue
				}
				jobs = append(jobs, Job{
					Status:    j.Status,
					Repo:      repo,
					Workflow:  j.WorkflowName,
					Branch:    j.HeadBranch,
					Name:      j.Name,
					Runner:    j.RunnerName,
					Labels:    strings.Join(j.Labels, ","),
					StartedAt: parseTime(j.StartedAt),
					CreatedAt: parseTime(j.CreatedAt),
					URL:       j.HTMLURL,
				})
			}
		}
	}
	return jobs
}

const TotalUnknown = -1

func Scan(ctx context.Context, org string, days int, progress func(done, total int)) ([]Job, error) {
	var limited atomic.Bool

	if progress != nil {
		progress(0, TotalUnknown)
	}

	repos := listRepos(ctx, &limited, org, days)
	total := len(repos)
	if progress != nil {
		progress(0, total)
	}

	results := make([][]Job, total)
	done := make(chan int, total)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i, repo := range repos {
		g.Go(func() error {
			results[i] = scanRepo(gctx, &limited, org, repo)
			done <- 1
			return nil
		})
	}

	finished := make(chan struct{})
	go func() {
		count := 0
		for range done {
			count++
			if progress != nil {
				progress(count, total)
			}
		}
		close(finished)
	}()

	g.Wait()
	close(done)
	<-finished

	var jobs []Job
	for _, r := range results {
		jobs = append(jobs, r...)
	}

	if limited.Load() {
		return jobs, ErrRateLimited
	}
	return jobs, nil
}

func SplitJobs(jobs []Job) (running, queued []Job) {
	for _, j := range jobs {
		if j.Status == "in_progress" {
			running = append(running, j)
		} else {
			queued = append(queued, j)
		}
	}
	sort.SliceStable(running, func(a, b int) bool {
		return running[a].Runner < running[b].Runner
	})
	sort.SliceStable(queued, func(a, b int) bool {
		return queued[a].CreatedAt.Before(queued[b].CreatedAt)
	})
	return running, queued
}

func FilterJobs(jobs []Job, needle string) []Job {
	if needle == "" {
		return jobs
	}
	n := strings.ToLower(needle)
	var kept []Job
	for _, j := range jobs {
		fields := []string{j.Repo, j.Workflow, j.Branch, j.Name, j.Runner, j.Labels}
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), n) {
				kept = append(kept, j)
				break
			}
		}
	}
	return kept
}
