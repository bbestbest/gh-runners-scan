package scan

import (
	"context"
	"errors"
	"fmt"
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
	Owner     string
	RunID     int64
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
	Step      Step
}

type Step struct {
	Num   int
	Total int
	Name  string
}

type stepEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type repoEntry struct {
	Name     string `json:"name"`
	PushedAt string `json:"pushed_at"`
	Archived bool   `json:"archived"`
}

type ownerEntry struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type runEntry struct {
	DatabaseID int64 `json:"id"`
}

type runsResponse struct {
	WorkflowRuns []runEntry `json:"workflow_runs"`
}

type jobsResponse struct {
	Jobs []struct {
		RunID        int64       `json:"run_id"`
		Status       string      `json:"status"`
		WorkflowName string      `json:"workflow_name"`
		HeadBranch   string      `json:"head_branch"`
		Name         string      `json:"name"`
		RunnerName   string      `json:"runner_name"`
		Labels       []string    `json:"labels"`
		StartedAt    string      `json:"started_at"`
		CreatedAt    string      `json:"created_at"`
		HTMLURL      string      `json:"html_url"`
		Steps        []stepEntry `json:"steps"`
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
		link, err := c.getInto(ctx, full, out)
		if err == nil {
			return link, true
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
	sharedClient()
	key := org + "\x00" + strconv.Itoa(days)
	if names, ok := cachedRepos(key); ok {
		return names
	}

	next, ok := reposPath(ctx, limited, org)
	if !ok {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var names []string
	for next != "" {
		var entries []repoEntry
		link, ok := getJSON(ctx, limited, &entries, next)
		if !ok {
			return nil
		}
		older := false
		for _, e := range entries {
			if e.Archived {
				continue
			}
			if days <= 0 {
				names = append(names, e.Name)
				continue
			}
			ts := parseTime(e.PushedAt)
			if ts.IsZero() {
				continue
			}
			if !ts.After(cutoff) {
				older = true
				break
			}
			names = append(names, e.Name)
		}
		if older {
			break
		}
		next = nextPageURL(link)
	}
	storeRepos(key, names)
	return names
}

func Login(ctx context.Context) (string, error) {
	var me ownerEntry
	var limited atomic.Bool
	if _, ok := getJSON(ctx, &limited, &me, "/user"); !ok || me.Login == "" {
		if limited.Load() {
			return "", ErrRateLimited
		}
		return "", errors.New("could not read the signed-in account (check the token)")
	}
	return me.Login, nil
}

type Account struct {
	Login string
	Org   bool
}

func Accounts(ctx context.Context) ([]Account, error) {
	login, err := Login(ctx)
	if err != nil {
		return nil, err
	}
	out := []Account{{Login: login}}
	var orgs []ownerEntry
	var limited atomic.Bool
	if _, ok := getJSON(ctx, &limited, &orgs, "/user/orgs?per_page=100"); ok {
		for _, o := range orgs {
			if o.Login != "" {
				out = append(out, Account{Login: o.Login, Org: true})
			}
		}
	}
	return out, nil
}

func Repos(ctx context.Context, owner string, days int) ([]string, error) {
	var limited atomic.Bool
	if _, ok := reposPath(ctx, &limited, owner); !ok {
		if limited.Load() {
			return nil, ErrRateLimited
		}
		return nil, errors.New("cannot find " + owner)
	}
	names := listRepos(ctx, &limited, owner, days)
	if limited.Load() {
		return names, ErrRateLimited
	}
	return names, nil
}

func reposPath(ctx context.Context, limited *atomic.Bool, owner string) (string, bool) {
	var o ownerEntry
	if _, ok := getJSON(ctx, limited, &o, "/users/"+url.PathEscape(owner)); !ok {
		return "", false
	}
	if o.Type == "Organization" {
		return "/orgs/" + url.PathEscape(owner) + "/repos?per_page=100&sort=pushed&direction=desc", true
	}
	var me ownerEntry
	if _, ok := getJSON(ctx, limited, &me, "/user"); ok && strings.EqualFold(me.Login, owner) {
		return "/user/repos?per_page=100&sort=pushed&direction=desc&affiliation=owner", true
	}
	return "/users/" + url.PathEscape(owner) + "/repos?per_page=100&sort=pushed&direction=desc&type=owner", true
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
			next := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) +
				"/actions/runs/" + strconv.FormatInt(run.DatabaseID, 10) + "/jobs?per_page=100"
			for next != "" {
				var resp jobsResponse
				link, ok := getJSON(ctx, limited, &resp, next)
				if !ok {
					break
				}
				for _, j := range resp.Jobs {
					if j.Status == "completed" {
						continue
					}
					jobs = append(jobs, Job{
						Owner:     org,
						RunID:     j.RunID,
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
						Step:      currentStep(j.Steps),
					})
				}
				next = nextPageURL(link)
			}
		}
	}
	return jobs
}

func currentStep(steps []stepEntry) Step {
	for i, s := range steps {
		if s.Status != "completed" {
			return Step{Num: i + 1, Total: len(steps), Name: s.Name}
		}
	}
	if len(steps) == 0 {
		return Step{}
	}
	return Step{Num: len(steps), Total: len(steps), Name: steps[len(steps)-1].Name}
}

func (s Step) Fraction() string {
	if s.Total == 0 {
		return "-"
	}
	return strconv.Itoa(s.Num) + "/" + strconv.Itoa(s.Total)
}

const TotalUnknown = -1

func SplitRepo(org, repo string) (string, string) {
	repo = strings.TrimSpace(repo)
	repo = strings.Trim(repo, "/")
	if owner, name, ok := strings.Cut(repo, "/"); ok {
		owner = strings.TrimSpace(owner)
		name = strings.TrimSpace(name)
		if owner != "" && name != "" {
			return owner, name
		}
		if name != "" {
			return org, name
		}
		return org, ""
	}
	return org, repo
}

func ScanRepo(ctx context.Context, org, repo string, progress func(done, total int)) ([]Job, error) {
	owner, name := SplitRepo(org, repo)
	if name == "" {
		return nil, nil
	}

	var limited atomic.Bool

	if progress != nil {
		progress(0, 1)
	}

	jobs := scanRepo(ctx, &limited, owner, name)

	if progress != nil {
		progress(1, 1)
	}

	if limited.Load() {
		return jobs, ErrRateLimited
	}
	return jobs, nil
}

func Scan(ctx context.Context, org string, days int, progress func(done, total int)) ([]Job, error) {
	return ScanTargets(ctx, []Target{{Owner: org, All: true}}, days, progress)
}

type Target struct {
	Owner string
	All   bool
	Repos []string
}

func ParseTargets(entries []string) []Target {
	var out []Target
	index := map[string]int{}
	for _, entry := range entries {
		owner, repo, _ := strings.Cut(strings.TrimSpace(entry), "/")
		owner = strings.TrimSpace(owner)
		repo = strings.TrimSpace(repo)
		if owner == "" {
			continue
		}
		key := strings.ToLower(owner)
		i, seen := index[key]
		if !seen {
			i = len(out)
			index[key] = i
			out = append(out, Target{Owner: owner})
		}
		if repo == "" || repo == "*" {
			out[i].All = true
			out[i].Repos = nil
			continue
		}
		if !out[i].All {
			out[i].Repos = append(out[i].Repos, repo)
		}
	}
	return out
}

func FormatTargets(targets []Target) []string {
	var out []string
	for _, t := range targets {
		if t.All {
			out = append(out, t.Owner+"/*")
			continue
		}
		for _, r := range t.Repos {
			out = append(out, t.Owner+"/"+r)
		}
	}
	return out
}

func ResolveTargets(ctx context.Context, owner, repo string, saved []string, savedOrg string) ([]Target, error) {
	if owner == "" && repo == "" {
		if targets := ParseTargets(saved); len(targets) > 0 {
			return targets, nil
		}
	}
	if owner == "" {
		owner = savedOrg
	}
	if owner == "" {
		login, err := Login(ctx)
		if err != nil {
			return nil, err
		}
		owner = login
	}
	if repo != "" {
		if o, name := SplitRepo(owner, repo); name != "" {
			return []Target{{Owner: o, Repos: []string{name}}}, nil
		}
	}
	return []Target{{Owner: owner, All: true}}, nil
}

func TargetLabel(targets []Target) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		switch {
		case t.All:
			parts = append(parts, t.Owner)
		case len(t.Repos) == 1:
			parts = append(parts, t.Owner+"/"+t.Repos[0])
		default:
			parts = append(parts, fmt.Sprintf("%s (%d repos)", t.Owner, len(t.Repos)))
		}
	}
	return strings.Join(parts, ", ")
}

func MultiOwner(targets []Target) bool {
	return len(targets) > 1
}

func ScanTargets(ctx context.Context, targets []Target, days int, progress func(done, total int)) ([]Job, error) {
	var limited atomic.Bool

	if progress != nil {
		progress(0, TotalUnknown)
	}

	type pair struct{ owner, repo string }
	var pairs []pair
	for _, t := range targets {
		if t.All {
			for _, repo := range listRepos(ctx, &limited, t.Owner, days) {
				pairs = append(pairs, pair{t.Owner, repo})
			}
			continue
		}
		for _, repo := range t.Repos {
			pairs = append(pairs, pair{t.Owner, repo})
		}
	}
	total := len(pairs)
	if progress != nil {
		progress(0, total)
	}

	results := make([][]Job, total)
	done := make(chan int, total)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i, p := range pairs {
		g.Go(func() error {
			results[i] = scanRepo(gctx, &limited, p.owner, p.repo)
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
