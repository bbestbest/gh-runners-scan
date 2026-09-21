package scan

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
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
	PushedAt string `json:"pushed_at"`
	Archived bool   `json:"archived"`
}

type runEntry struct {
	DatabaseID int64 `json:"id"`
}

type runsResponse struct {
	WorkflowRuns []runEntry `json:"workflow_runs"`
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
	var rl *rateLimitError
	return errors.As(err, &rl)
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

func backoffFor(err error, attempt int) time.Duration {
	var rl *rateLimitError
	if errors.As(err, &rl) && rl.wait > 0 && rl.wait <= time.Duration(rateLimitAttempts)*rateLimitBackoff {
		return rl.wait
	}
	return time.Duration(attempt+1) * rateLimitBackoff
}

func getJSON(ctx context.Context, limited *atomic.Bool, out any, path string) (string, bool) {
	c, err := sharedClient()
	if err != nil {
		limited.Store(false)
		return "", false
	}
	full := path
	if !strings.HasPrefix(full, "http") {
		full = c.base + path
	}
	for attempt := 0; ; attempt++ {
		data, link, err := c.fetch(ctx, full)
		if err == nil {
			if out == nil {
				return link, true
			}
			return link, json.Unmarshal(data, out) == nil
		}
		if !isRateLimit(err) {
			return "", false
		}
		if attempt >= rateLimitAttempts-1 {
			limited.Store(true)
			return "", false
		}
		if !sleepCtx(ctx, backoffFor(err, attempt)) {
			limited.Store(true)
			return "", false
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

	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var names []string
	next := "/orgs/" + url.PathEscape(org) + "/repos?per_page=100&sort=pushed&direction=desc"
	for next != "" {
		var entries []repoEntry
		link, ok := getJSON(ctx, limited, &entries, next)
		if !ok {
			return nil
		}
		for _, e := range entries {
			if e.Archived {
				continue
			}
			if days <= 0 {
				names = append(names, e.Name)
				continue
			}
			if ts := parseTime(e.PushedAt); !ts.IsZero() && ts.After(cutoff) {
				names = append(names, e.Name)
			}
		}
		next = nextPageURL(link)
	}
	storeRepos(key, names)
	return names
}

func scanRepo(ctx context.Context, limited *atomic.Bool, org, repo string) []Job {
	var jobs []Job
	for _, status := range []string{"in_progress", "queued"} {
		var runs runsResponse
		runsPath := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) +
			"/actions/runs?status=" + url.QueryEscape(status) + "&per_page=20"
		if _, ok := getJSON(ctx, limited, &runs, runsPath); !ok {
			continue
		}
		for _, run := range runs.WorkflowRuns {
			var resp jobsResponse
			path := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) +
				"/actions/runs/" + strconv.FormatInt(run.DatabaseID, 10) + "/jobs"
			if _, ok := getJSON(ctx, limited, &resp, path); !ok {
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
