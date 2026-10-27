package scan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type progressCall struct {
	done  int
	total int
}

func withClient(t *testing.T, base string) {
	t.Helper()
	prev := defaultClient
	prevErr := defaultClientErr
	defaultClientOnce.Do(func() {})
	defaultClient = testClient(base)
	defaultClientErr = nil
	t.Cleanup(func() {
		defaultClient = prev
		defaultClientErr = prevErr
	})
}

func resetRepoCache(t *testing.T) {
	t.Helper()
	repoCacheMu.Lock()
	repoCache = map[string]repoCacheEntry{}
	repoCacheMu.Unlock()
}

func TestListReposFollowsPagination(t *testing.T) {
	resetRepoCache(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/org-a" {
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/orgs/org-a/repos") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintf(w, `[{"name":"gamma","pushed_at":%q,"archived":false},{"name":"stale","pushed_at":"2020-01-01T00:00:00Z","archived":false}]`, now)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/org-a/repos?page=2>; rel="next"`, srv.URL))
		fmt.Fprintf(w, `[{"name":"alpha","pushed_at":%q,"archived":false},{"name":"old","pushed_at":%q,"archived":true}]`, now, now)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	names := listRepos(context.Background(), &limited, "org-a", 7)
	want := []string{"alpha", "gamma"}
	if len(names) != len(want) {
		t.Fatalf("names = %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v want %v", names, want)
		}
	}
	if limited.Load() {
		t.Fatal("should not be rate limited")
	}
}

func TestListReposStopsAtFirstRepoOutsideDays(t *testing.T) {
	for _, tc := range []struct {
		days      int
		want      []string
		wantPages int32
	}{
		{days: 7, want: []string{"alpha", "beta"}, wantPages: 1},
		{days: 0, want: []string{"alpha", "empty", "beta", "stale", "gamma"}, wantPages: 2},
	} {
		resetRepoCache(t)
		var pages atomic.Int32
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/users/org-a" {
				fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
				return
			}
			pages.Add(1)
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"name":"gamma","pushed_at":"2019-01-01T00:00:00Z","archived":false}]`)
				return
			}
			now := time.Now().UTC().Format(time.RFC3339)
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/org-a/repos?page=2>; rel="next"`, srv.URL))
			fmt.Fprintf(w, `[{"name":"alpha","pushed_at":%q,"archived":false},{"name":"empty","pushed_at":null,"archived":false},{"name":"beta","pushed_at":%q,"archived":false},{"name":"stale","pushed_at":"2020-01-01T00:00:00Z","archived":false}]`, now, now)
		}))
		withClient(t, srv.URL)

		var limited atomic.Bool
		names := listRepos(context.Background(), &limited, "org-a", tc.days)
		srv.Close()
		if !reflect.DeepEqual(names, tc.want) {
			t.Fatalf("days=%d: names = %v, want %v", tc.days, names, tc.want)
		}
		if got := pages.Load(); got != tc.wantPages {
			t.Fatalf("days=%d: fetched %d pages, want %d", tc.days, got, tc.wantPages)
		}
	}
}

func TestListReposPicksEndpointForOwnerType(t *testing.T) {
	for _, tc := range []struct {
		owner, kind, me, wantPath, wantQuery string
	}{
		{"org-a", "Organization", "me-user", "/orgs/org-a/repos", "direction=desc&per_page=100&sort=pushed"},
		{"me-user", "User", "me-user", "/user/repos", "affiliation=owner&direction=desc&per_page=100&sort=pushed"},
		{"Me-User", "User", "me-user", "/user/repos", "affiliation=owner&direction=desc&per_page=100&sort=pushed"},
		{"other-user", "User", "me-user", "/users/other-user/repos", "direction=desc&per_page=100&sort=pushed&type=owner"},
	} {
		resetRepoCache(t)
		var listed string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/users/" + tc.owner:
				fmt.Fprintf(w, `{"login":%q,"type":%q}`, tc.owner, tc.kind)
			case "/user":
				fmt.Fprintf(w, `{"login":%q,"type":"User"}`, tc.me)
			default:
				listed = r.URL.Path + "?" + r.URL.Query().Encode()
				fmt.Fprintf(w, `[{"name":"alpha","pushed_at":%q,"archived":false}]`, time.Now().UTC().Format(time.RFC3339))
			}
		}))
		withClient(t, srv.URL)

		var limited atomic.Bool
		names := listRepos(context.Background(), &limited, tc.owner, 7)
		srv.Close()
		if !reflect.DeepEqual(names, []string{"alpha"}) {
			t.Fatalf("%s: names = %v", tc.owner, names)
		}
		if want := tc.wantPath + "?" + tc.wantQuery; listed != want {
			t.Fatalf("%s: listed %q, want %q", tc.owner, listed, want)
		}
	}
}

func TestLoginReadsSignedInAccount(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{http.StatusOK, `{"login":"me-user","type":"User"}`, "me-user", false},
		{http.StatusUnauthorized, `{"message":"Bad credentials"}`, "", true},
		{http.StatusOK, `{}`, "", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/user" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		withClient(t, srv.URL)
		got, err := Login(context.Background())
		srv.Close()
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Fatalf("status %d: Login() = %q, %v; want %q, err=%v", tc.status, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestAccountsListsUserThenOrgs(t *testing.T) {
	for _, tc := range []struct {
		orgsStatus int
		want       []Account
	}{
		{http.StatusOK, []Account{{Login: "me-user"}, {Login: "org-a", Org: true}, {Login: "org-b", Org: true}}},
		{http.StatusForbidden, []Account{{Login: "me-user"}}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/user":
				fmt.Fprint(w, `{"login":"me-user","type":"User"}`)
			case "/user/orgs":
				w.WriteHeader(tc.orgsStatus)
				fmt.Fprint(w, `[{"login":"org-a"},{"login":"org-b"}]`)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))
		withClient(t, srv.URL)
		got, err := Accounts(context.Background())
		srv.Close()
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("orgs %d: Accounts() = %v, %v; want %v", tc.orgsStatus, got, err, tc.want)
		}
	}
}

func TestReposListsOwnerAndReportsUnknown(t *testing.T) {
	resetRepoCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"api","pushed_at":%q,"archived":false},{"name":"old","pushed_at":"2019-01-01T00:00:00Z","archived":false}]`, time.Now().UTC().Format(time.RFC3339))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	names, err := Repos(context.Background(), "org-a", 7)
	if err != nil || !reflect.DeepEqual(names, []string{"api"}) {
		t.Fatalf("Repos(org-a) = %v, %v", names, err)
	}
	if cached, ok := cachedRepos("org-a\x007"); !ok || !reflect.DeepEqual(cached, []string{"api"}) {
		t.Fatalf("repo list not cached for the first scan: %v %v", cached, ok)
	}
	if names, err := Repos(context.Background(), "nobody", 7); err == nil || names != nil {
		t.Fatalf("Repos(nobody) = %v, %v; want an error", names, err)
	}
}

func TestParseAndFormatTargets(t *testing.T) {
	got := ParseTargets([]string{"org-a/*", "org-b/api", " org-b / web ", "ORG-A/extra", "", "/x", "org-c"})
	want := []Target{{Owner: "org-a", All: true}, {Owner: "org-b", Repos: []string{"api", "web"}}, {Owner: "org-c", All: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTargets = %+v, want %+v", got, want)
	}
	if out := FormatTargets(want); !reflect.DeepEqual(out, []string{"org-a/*", "org-b/api", "org-b/web", "org-c/*"}) {
		t.Fatalf("FormatTargets = %v", out)
	}
}

func TestScanTargetsMixesWholeOwnersAndSingleRepos(t *testing.T) {
	resetRepoCache(t)
	var mu sync.Mutex
	var runs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"one","pushed_at":%q,"archived":false},{"name":"two","pushed_at":%q,"archived":false}]`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			mu.Lock()
			runs = append(runs, strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/actions/runs"), "/repos/")+"?"+r.URL.Query().Get("status"))
			mu.Unlock()
			if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"run_id":7,"status":"queued","name":"build","labels":["x"]}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var calls []progressCall
	jobs, err := ScanTargets(context.Background(), []Target{{Owner: "org-a", All: true}, {Owner: "org-b", Repos: []string{"api"}}}, 7, func(d, total int) {
		mu.Lock()
		calls = append(calls, progressCall{d, total})
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(runs)
	want := []string{"org-a/one?in_progress", "org-a/one?queued", "org-a/two?in_progress", "org-a/two?queued", "org-b/api?in_progress", "org-b/api?queued"}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs requested = %v, want %v", runs, want)
	}
	owners := map[string]bool{}
	for _, j := range jobs {
		owners[j.Owner+"/"+j.Repo] = true
	}
	if len(jobs) != 3 || !owners["org-a/one"] || !owners["org-a/two"] || !owners["org-b/api"] {
		t.Fatalf("jobs = %+v", jobs)
	}
	if calls[0] != (progressCall{0, TotalUnknown}) || calls[1] != (progressCall{0, 3}) || calls[len(calls)-1] != (progressCall{3, 3}) {
		t.Fatalf("progress = %v", calls)
	}
}

func TestListReposUnknownOwnerReturnsNothing(t *testing.T) {
	resetRepoCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/nobody" {
			t.Errorf("listed repos for an unknown owner: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	if names := listRepos(context.Background(), &limited, "nobody", 7); names != nil {
		t.Fatalf("names = %v, want nil", names)
	}
}

func TestScanRepoMapsRESTEndpoints(t *testing.T) {
	var jobsCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/org-a/web/actions/runs":
			status := r.URL.Query().Get("status")
			if r.URL.Query().Get("per_page") != "20" {
				t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
			}
			if status == "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/org-a/web/actions/runs/7/jobs":
			jobsCalls.Add(1)
			fmt.Fprint(w, `{"jobs":[
				{"run_id":7,"status":"in_progress","workflow_name":"CI","head_branch":"main","name":"build","runner_name":"rnr-1","labels":["self-hosted","linux"],"started_at":"2026-09-20T10:00:00Z","created_at":"2026-09-20T09:59:00Z","html_url":"https://gh/j/1"},
				{"status":"completed","workflow_name":"CI","head_branch":"main","name":"done","runner_name":"rnr-2","labels":[],"started_at":"","created_at":"","html_url":""}
			]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	jobs := scanRepo(context.Background(), &limited, "org-a", "web")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1 non-completed", jobs)
	}
	j := jobs[0]
	if j.Repo != "web" || j.Workflow != "CI" || j.Runner != "rnr-1" || j.Labels != "self-hosted,linux" {
		t.Fatalf("job = %+v", j)
	}
	if j.StartedAt.IsZero() || j.CreatedAt.IsZero() || j.URL != "https://gh/j/1" {
		t.Fatalf("job times/url = %+v", j)
	}
	if j.RunID != 7 || j.Owner != "org-a" {
		t.Fatalf("job run id/owner = %d %q, want 7 org-a", j.RunID, j.Owner)
	}
	if jobsCalls.Load() != 1 {
		t.Fatalf("jobs endpoint called %d times", jobsCalls.Load())
	}
}

func TestScanRepoSecondPassIsConditional(t *testing.T) {
	var full atomic.Int64
	var notModified atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := `W/"` + r.URL.Path + `"`
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		if strings.HasSuffix(r.URL.Path, "/runs") {
			fmt.Fprint(w, `{"workflow_runs":[]}`)
			return
		}
		fmt.Fprint(w, `{"jobs":[]}`)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	scanRepo(context.Background(), &limited, "org-a", "web")
	firstFull := full.Load()
	scanRepo(context.Background(), &limited, "org-a", "web")

	if full.Load() != firstFull {
		t.Fatalf("second scan issued %d extra full responses, expected all 304", full.Load()-firstFull)
	}
	if notModified.Load() != firstFull {
		t.Fatalf("notModified = %d want %d", notModified.Load(), firstFull)
	}
}

func TestRateLimitExhaustsAttemptsAndFlags(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	var out any
	if _, ok := getJSON(context.Background(), &limited, &out, "/x"); ok {
		t.Fatal("expected failure")
	}
	if !limited.Load() {
		t.Fatal("limited flag not set")
	}
	if calls.Load() != rateLimitAttempts {
		t.Fatalf("calls = %d want %d", calls.Load(), rateLimitAttempts)
	}
}

func TestScanReturnsErrRateLimited(t *testing.T) {
	resetRepoCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC().Format(time.RFC3339)
		if r.URL.Path == "/users/org-a" {
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			fmt.Fprintf(w, `[{"name":"web","pushed_at":%q,"archived":false}]`, now)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	jobs, err := Scan(context.Background(), "org-a", 7, nil)
	if err != ErrRateLimited {
		t.Fatalf("err = %v want ErrRateLimited", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestSplitRepoResolvesBothForms(t *testing.T) {
	cases := []struct {
		org, repo   string
		owner, name string
	}{
		{"org-a", "web", "org-a", "web"},
		{"org-a", "other/web", "other", "web"},
		{"org-a", " org-a/web ", "org-a", "web"},
		{"org-a", "/web", "org-a", "web"},
		{"org-a", "web/", "org-a", "web"},
		{"org-a", "", "org-a", ""},
		{"org-a", "  ", "org-a", ""},
	}
	for _, c := range cases {
		owner, name := SplitRepo(c.org, c.repo)
		if owner != c.owner || name != c.name {
			t.Errorf("SplitRepo(%q, %q) = %q, %q want %q, %q", c.org, c.repo, owner, name, c.owner, c.name)
		}
	}
}

func TestScanRepoSkipsRepoListing(t *testing.T) {
	resetRepoCache(t)
	var orgCalls atomic.Int64
	var paths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			orgCalls.Add(1)
			t.Errorf("single-repo scan listed org repos: %s", r.URL.Path)
			fmt.Fprint(w, `[]`)
			return
		}
		switch r.URL.Path {
		case "/repos/org-a/web/actions/runs":
			if r.URL.Query().Get("status") == "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":11}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case "/repos/org-a/web/actions/runs/11/jobs":
			fmt.Fprint(w, `{"jobs":[{"status":"in_progress","workflow_name":"CI","head_branch":"main","name":"build","runner_name":"rnr-1","labels":["self-hosted"],"started_at":"2026-09-20T10:00:00Z","created_at":"2026-09-20T09:59:00Z","html_url":"https://gh/j/11"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var got []progressCall
	jobs, err := ScanRepo(context.Background(), "org-a", "web", func(done, total int) {
		got = append(got, progressCall{done, total})
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if orgCalls.Load() != 0 {
		t.Fatalf("listRepos was called %d times", orgCalls.Load())
	}
	if len(jobs) != 1 || jobs[0].Repo != "web" || jobs[0].Runner != "rnr-1" {
		t.Fatalf("jobs = %+v", jobs)
	}
	mu.Lock()
	n := len(paths)
	mu.Unlock()
	if n != 3 {
		t.Fatalf("issued %d requests (%v), want 3", n, paths)
	}
	want := []progressCall{{0, 1}, {1, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %v want %v", got, want)
	}
}

func TestScanRepoOwnerFormOverridesOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/other/web/") {
			t.Errorf("owner/repo form hit %s, want the other owner", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			fmt.Fprint(w, `{"workflow_runs":[]}`)
			return
		}
		fmt.Fprint(w, `{"jobs":[]}`)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	if _, err := ScanRepo(context.Background(), "org-a", "other/web", nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestScanRepoEmptyNameScansNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("empty repo name issued a request to %s", r.URL.Path)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	jobs, err := ScanRepo(context.Background(), "org-a", "   ", nil)
	if err != nil || jobs != nil {
		t.Fatalf("jobs = %+v err = %v, want no scan", jobs, err)
	}
}

func TestScanRepoReturnsOnlyThatRepo(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			fmt.Fprint(w, `{"jobs":[{"status":"queued","workflow_name":"deploy","name":"job","html_url":"https://example.com/q"}]}`)
			return
		}
		if r.URL.Query().Get("status") == "in_progress" {
			fmt.Fprint(w, `{"workflow_runs":[{"id":11}]}`)
			return
		}
		fmt.Fprint(w, `{"workflow_runs":[]}`)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	jobs, err := ScanRepo(context.Background(), "org-a", "only-this", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, j := range jobs {
		if j.Repo != "only-this" {
			t.Errorf("job from %q, want only-this", j.Repo)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if !strings.Contains(p, "/repos/org-a/only-this") {
			t.Errorf("requested %q, want only /repos/org-a/only-this", p)
		}
	}
}

func TestScanRepoPropagatesRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	if _, err := ScanRepo(context.Background(), "org-a", "web", nil); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestScanRepoFollowsJobsPagination(t *testing.T) {
	var srv *httptest.Server
	var jobsCalls atomic.Int64
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org-a/web/actions/runs":
			if r.URL.Query().Get("per_page") != "20" {
				t.Errorf("runs per_page = %q, want 20", r.URL.Query().Get("per_page"))
			}
			if r.URL.Query().Get("status") == "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case "/repos/org-a/web/actions/runs/7/jobs":
			jobsCalls.Add(1)
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"jobs":[{"run_id":7,"status":"in_progress","name":"second-page","runner_name":"rnr-2"}]}`)
				return
			}
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("jobs per_page = %q, want 100", r.URL.Query().Get("per_page"))
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/org-a/web/actions/runs/7/jobs?per_page=100&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `{"jobs":[{"run_id":7,"status":"in_progress","name":"first-page","runner_name":"rnr-1"},{"run_id":7,"status":"completed","name":"done"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	jobs := scanRepo(context.Background(), &limited, "org-a", "web")
	if len(jobs) != 2 || jobs[0].Name != "first-page" || jobs[1].Name != "second-page" {
		t.Fatalf("jobs = %+v, want first-page and second-page", jobs)
	}
	if jobsCalls.Load() != 2 {
		t.Fatalf("jobs endpoint called %d times, want 2", jobsCalls.Load())
	}
	if limited.Load() {
		t.Fatal("should not be rate limited")
	}
}

func TestCurrentStep(t *testing.T) {
	cases := []struct {
		name  string
		steps []stepEntry
		want  Step
		frac  string
	}{
		{"no steps", nil, Step{}, "-"},
		{"first running", []stepEntry{{"Set up job", "in_progress"}, {"Build", "queued"}}, Step{1, 2, "Set up job"}, "1/2"},
		{"middle running", []stepEntry{{"Set up job", "completed"}, {"Build", "in_progress"}, {"Test", "queued"}}, Step{2, 3, "Build"}, "2/3"},
		{"all completed", []stepEntry{{"Set up job", "completed"}, {"Build", "completed"}}, Step{2, 2, "Build"}, "2/2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := currentStep(tc.steps)
			if got != tc.want || got.Fraction() != tc.frac {
				t.Fatalf("currentStep = %+v %q, want %+v %q", got, got.Fraction(), tc.want, tc.frac)
			}
		})
	}
}
