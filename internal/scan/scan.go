package scan

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const workers = 6

const repoWorkers = 4 * workers

const rateLimitAttempts = 3

const rateLimitBackoff = 3 * time.Second

const repoCacheTTL = 12 * time.Minute

const finishedPerRepo = 5

const finishedWindow = 24 * time.Hour

var ErrRateLimited = errors.New("github api rate limited; results are incomplete")

type Job struct {
	Owner      string
	RunID      int64
	Status     string
	Repo       string
	Workflow   string
	Branch     string
	Name       string
	Runner     string
	Labels     string
	StartedAt  time.Time
	CreatedAt  time.Time
	URL        string
	Step       Step
	Conclusion string
	FinishedAt time.Time
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

type finishedResponse struct {
	WorkflowRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadBranch string `json:"head_branch"`
		Conclusion string `json:"conclusion"`
		StartedAt  string `json:"run_started_at"`
		UpdatedAt  string `json:"updated_at"`
		HTMLURL    string `json:"html_url"`
	} `json:"workflow_runs"`
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

type slotWaiter struct {
	prio    int
	seq     int
	ready   chan struct{}
	granted bool
	gone    bool
}

type slotPool struct {
	mu   sync.Mutex
	free int
	seq  int
	wait []*slotWaiter
}

var requestSlots = &slotPool{free: workers}

type priorityKey struct{}

func withPriority(ctx context.Context, prio int) context.Context {
	return context.WithValue(ctx, priorityKey{}, prio)
}

func priorityOf(ctx context.Context) int {
	prio, _ := ctx.Value(priorityKey{}).(int)
	return prio
}

type failedKey struct{}

func withFailed(ctx context.Context, failed *atomic.Bool) context.Context {
	return context.WithValue(ctx, failedKey{}, failed)
}

func markFailed(ctx context.Context) {
	if failed, ok := ctx.Value(failedKey{}).(*atomic.Bool); ok {
		failed.Store(true)
	}
}

func (p *slotPool) acquire(ctx context.Context) bool {
	p.mu.Lock()
	if p.free > 0 && len(p.wait) == 0 {
		p.free--
		p.mu.Unlock()
		return true
	}
	w := &slotWaiter{prio: priorityOf(ctx), seq: p.seq, ready: make(chan struct{})}
	p.seq++
	i := sort.Search(len(p.wait), func(i int) bool {
		return p.wait[i].prio > w.prio || (p.wait[i].prio == w.prio && p.wait[i].seq > w.seq)
	})
	p.wait = slices.Insert(p.wait, i, w)
	p.mu.Unlock()
	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
		p.mu.Lock()
		granted := w.granted
		w.gone = true
		p.mu.Unlock()
		if granted {
			p.release()
		}
		return false
	}
}

func (p *slotPool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.wait) > 0 {
		w := p.wait[0]
		p.wait = p.wait[1:]
		if w.gone {
			continue
		}
		w.granted = true
		close(w.ready)
		return
	}
	p.free++
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
		if !requestSlots.acquire(ctx) {
			return "", false
		}
		link, err := c.getInto(ctx, full, out)
		requestSlots.release()
		if err == nil {
			return link, true
		}
		if !isRateLimit(err) {
			var se *statusError
			if !errors.As(err, &se) || se.code >= 500 {
				markFailed(ctx)
			}
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

type repoInfo struct {
	Name   string    `json:"name"`
	Pushed time.Time `json:"pushed"`
}

type repoCacheEntry struct {
	repos   []repoInfo
	days    int
	fetched time.Time
}

func covers(cached, days int) bool {
	return cached == 0 || (days > 0 && days <= cached)
}

var (
	repoCacheMu sync.Mutex
	repoCache   = map[string]repoCacheEntry{}
)

func cachedRepoInfos(owner string, days int) ([]repoInfo, bool) {
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	entry, ok := repoCache[strings.ToLower(owner)]
	if !ok || time.Since(entry.fetched) > repoCacheTTL || !covers(entry.days, days) {
		return nil, false
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	var out []repoInfo
	for _, r := range entry.repos {
		if days <= 0 || r.Pushed.After(cutoff) {
			out = append(out, r)
		}
	}
	return out, true
}

func cachedRepos(owner string, days int) ([]string, bool) {
	infos, ok := cachedRepoInfos(owner, days)
	if !ok {
		return nil, false
	}
	return repoNames(infos), true
}

func repoNames(infos []repoInfo) []string {
	var names []string
	for _, r := range infos {
		names = append(names, r.Name)
	}
	return names
}

func storeRepos(owner string, days int, repos []repoInfo) {
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	repoCache[strings.ToLower(owner)] = repoCacheEntry{repos: repos, days: days, fetched: time.Now()}
}

func listRepos(ctx context.Context, limited *atomic.Bool, org string, days int) []string {
	sharedClient()
	if names, ok := cachedRepos(org, days); ok {
		return names
	}
	return repoNames(fetchRepos(ctx, limited, org, days))
}

func fetchRepos(ctx context.Context, limited *atomic.Bool, org string, days int) []repoInfo {
	next, ok := reposPath(ctx, limited, org)
	if !ok {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var repos []repoInfo
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
			ts := parseTime(e.PushedAt)
			if days <= 0 {
				repos = append(repos, repoInfo{Name: e.Name, Pushed: ts})
				continue
			}
			if ts.IsZero() {
				continue
			}
			if !ts.After(cutoff) {
				older = true
				break
			}
			repos = append(repos, repoInfo{Name: e.Name, Pushed: ts})
		}
		if older {
			break
		}
		next = nextPageURL(link)
	}
	storeRepos(org, days, repos)
	return repos
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
	if names, ok := cachedRepos(owner, days); ok {
		return names, nil
	}
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

func scanRepo(ctx context.Context, limited *atomic.Bool, org, repo string, finished bool) []Job {
	base := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/actions/runs"
	statuses := []string{"in_progress", "queued"}
	lists := make([][]runEntry, len(statuses))
	var done []Job
	var wg sync.WaitGroup
	if finished {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done = finishedRuns(ctx, limited, org, repo, base+"?status=completed&per_page="+strconv.Itoa(finishedPerRepo))
		}()
	}
	for i, status := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var runs runsResponse
			if _, ok := getJSON(ctx, limited, &runs, base+"?status="+url.QueryEscape(status)+"&per_page=20"); ok {
				lists[i] = runs.WorkflowRuns
			}
		}()
	}
	wg.Wait()

	var runs []runEntry
	for _, l := range lists {
		runs = append(runs, l...)
	}
	perRun := make([][]Job, len(runs))
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			perRun[i] = runJobs(ctx, limited, org, repo, base+"/"+strconv.FormatInt(run.DatabaseID, 10)+"/jobs?per_page=100")
		}()
	}
	wg.Wait()

	var jobs []Job
	for _, js := range perRun {
		jobs = append(jobs, js...)
	}
	return append(jobs, done...)
}

func finishedRuns(ctx context.Context, limited *atomic.Bool, org, repo, path string) []Job {
	var resp finishedResponse
	if _, ok := getJSON(ctx, limited, &resp, path); !ok {
		return nil
	}
	var jobs []Job
	for _, r := range resp.WorkflowRuns {
		at := parseTime(r.UpdatedAt)
		if at.IsZero() || time.Since(at) > finishedWindow {
			continue
		}
		jobs = append(jobs, Job{
			Owner:      org,
			RunID:      r.ID,
			Status:     "completed",
			Repo:       repo,
			Workflow:   r.Name,
			Branch:     r.HeadBranch,
			StartedAt:  parseTime(r.StartedAt),
			URL:        r.HTMLURL,
			Conclusion: r.Conclusion,
			FinishedAt: at,
		})
	}
	return jobs
}

func Active(jobs []Job) int {
	n := 0
	for _, j := range jobs {
		if j.Status != "completed" {
			n++
		}
	}
	return n
}

func runJobs(ctx context.Context, limited *atomic.Bool, org, repo, next string) []Job {
	var jobs []Job
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

	jobs := scanRepo(ctx, &limited, owner, name, true)

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

const sweepBuckets = 8

const workflowsTTL = 24 * time.Hour

const freshWindow = 24 * time.Hour

type repoMemory struct {
	Pushed time.Time `json:"pushed"`
	Active bool      `json:"active"`
}

type workflowMemory struct {
	Count   int       `json:"count"`
	Checked time.Time `json:"checked"`
}

type Rounds struct {
	mu        sync.Mutex
	seen      map[string]repoMemory
	hints     map[string]bool
	workflows map[string]workflowMemory
	count     int
	unseen    int
}

func NewRounds() *Rounds {
	return &Rounds{seen: map[string]repoMemory{}, hints: map[string]bool{}, workflows: map[string]workflowMemory{}}
}

func (r *Rounds) next() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.count
	r.count++
	return n
}

func sweepBucket(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() % sweepBuckets)
}

func (r *Rounds) needs(key string, pushed time.Time, round int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.seen[key]
	inBucket := sweepBucket(key) == round%sweepBuckets
	if !ok {
		return r.hints[key] || pushed.IsZero() || time.Since(pushed) <= freshWindow || inBucket
	}
	return m.Active || pushed.IsZero() || pushed.After(m.Pushed) || inBucket
}

func (r *Rounds) hasWorkflows(ctx context.Context, limited *atomic.Bool, owner, repo string, pushed time.Time) bool {
	key := strings.ToLower(owner + "/" + repo)
	r.mu.Lock()
	w, ok := r.workflows[key]
	r.mu.Unlock()
	if ok && time.Since(w.Checked) <= workflowsTTL && !pushed.After(w.Checked) {
		return w.Count > 0
	}
	var resp struct {
		TotalCount int `json:"total_count"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/workflows?per_page=1"
	if _, ok := getJSON(ctx, limited, &resp, path); !ok {
		if limited.Load() {
			return true
		}
		resp.TotalCount = 1
	}
	r.mu.Lock()
	r.workflows[key] = workflowMemory{Count: resp.TotalCount, Checked: time.Now()}
	r.mu.Unlock()
	return resp.TotalCount > 0
}

func (r *Rounds) busy(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[key].Active || r.hints[key]
}

func (r *Rounds) known(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.seen[key]
	return ok
}

func (r *Rounds) Hint(owner, repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hints[strings.ToLower(owner+"/"+repo)] = true
}

func (r *Rounds) Wake(owner, repo string) {
	key := strings.ToLower(owner + "/" + repo)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.seen[key]; ok {
		m.Active = true
		r.seen[key] = m
		return
	}
	r.hints[key] = true
}

func (r *Rounds) Unseen() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unseen
}

func (r *Rounds) mark(key string, pushed time.Time, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[key] = repoMemory{Pushed: pushed, Active: active}
}

func ScanTargets(ctx context.Context, targets []Target, days int, progress func(done, total int)) ([]Job, error) {
	return ScanRound(ctx, targets, days, nil, progress, nil)
}

func ScanRound(ctx context.Context, targets []Target, days int, rounds *Rounds, progress func(done, total int), onRepo func(owner, repo string, jobs []Job)) ([]Job, error) {
	var limited atomic.Bool

	if progress != nil {
		progress(0, TotalUnknown)
	}

	round := 0
	if rounds != nil {
		round = rounds.next()
	}
	type pair struct {
		owner, repo string
		pushed      time.Time
	}
	var pairs []pair
	for _, t := range targets {
		if !t.All {
			for _, repo := range t.Repos {
				pairs = append(pairs, pair{owner: t.Owner, repo: repo})
			}
			continue
		}
		var infos []repoInfo
		if rounds == nil {
			if cached, ok := cachedRepoInfos(t.Owner, days); ok {
				infos = cached
			}
		}
		if infos == nil {
			infos = fetchRepos(ctx, &limited, t.Owner, days)
		}
		if infos == nil {
			infos, _ = cachedRepoInfos(t.Owner, days)
		}
		for _, r := range infos {
			pairs = append(pairs, pair{owner: t.Owner, repo: r.Name, pushed: r.Pushed})
		}
	}
	total := len(pairs)

	var todo []pair
	for _, p := range pairs {
		if rounds == nil || rounds.needs(strings.ToLower(p.owner+"/"+p.repo), p.pushed, round) {
			todo = append(todo, p)
		}
	}
	skipped := total - len(todo)
	if rounds != nil {
		chosen := make(map[string]bool, len(todo))
		for _, p := range todo {
			chosen[strings.ToLower(p.owner+"/"+p.repo)] = true
		}
		unseen := 0
		for _, p := range pairs {
			key := strings.ToLower(p.owner + "/" + p.repo)
			if !chosen[key] && !rounds.known(key) {
				unseen++
			}
		}
		rounds.mu.Lock()
		rounds.unseen = unseen
		rounds.mu.Unlock()
	}
	if progress != nil {
		progress(skipped, total)
	}

	if rounds != nil {
		sort.SliceStable(todo, func(a, b int) bool {
			return rounds.busy(strings.ToLower(todo[a].owner+"/"+todo[a].repo)) && !rounds.busy(strings.ToLower(todo[b].owner+"/"+todo[b].repo))
		})
	}
	results := make([][]Job, len(todo))
	done := make(chan int, len(todo))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(repoWorkers)
	for i, p := range todo {
		g.Go(func() error {
			var failed atomic.Bool
			gctx := withFailed(withPriority(gctx, i), &failed)
			var jobs []Job
			if rounds == nil || rounds.hasWorkflows(gctx, &limited, p.owner, p.repo, p.pushed) {
				jobs = scanRepo(gctx, &limited, p.owner, p.repo, rounds != nil)
			}
			results[i] = jobs
			if rounds != nil && !limited.Load() && gctx.Err() == nil {
				rounds.mark(strings.ToLower(p.owner+"/"+p.repo), p.pushed, Active(jobs) > 0 || failed.Load())
			}
			if onRepo != nil {
				onRepo(p.owner, p.repo, jobs)
			}
			done <- 1
			return nil
		})
	}

	finished := make(chan struct{})
	go func() {
		count := skipped
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
		switch j.Status {
		case "completed":
		case "in_progress":
			running = append(running, j)
		default:
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

func FinishedJobs(jobs []Job) []Job {
	var out []Job
	for _, j := range jobs {
		if j.Status == "completed" {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].FinishedAt.After(out[b].FinishedAt)
	})
	return out
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
