package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
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
	if cached, ok := cachedRepos("org-a", 7); !ok || !reflect.DeepEqual(cached, []string{"api"}) {
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
	jobs := scanRepo(context.Background(), &limited, "org-a", "web", false)
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
	scanRepo(context.Background(), &limited, "org-a", "web", false)
	firstFull := full.Load()
	scanRepo(context.Background(), &limited, "org-a", "web", false)

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
	if n != 4 {
		t.Fatalf("issued %d requests (%v), want 4", n, paths)
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
	jobs := scanRepo(context.Background(), &limited, "org-a", "web", false)
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

func TestScanRoundSkipsQuietRepos(t *testing.T) {
	resetRepoCache(t)
	base := time.Now().Add(-time.Hour).UTC()
	var mu sync.Mutex
	pushedAt := base
	var scanned []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"pushed","pushed_at":%q},{"name":"busy","pushed_at":%q},{"name":"quiet","pushed_at":%q}]`,
				pushedAt.Format(time.RFC3339), base.Format(time.RFC3339), base.Add(-time.Minute).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			fmt.Fprint(w, `{"total_count":1}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/actions/runs")
			if r.URL.Query().Get("status") == "queued" {
				scanned = append(scanned, repo)
			}
			if repo == "org-a/busy" && r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"run_id":7,"status":"queued","name":"build"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	rounds := NewRounds()
	targets := []Target{{Owner: "org-a", All: true}, {Owner: "org-b", Repos: []string{"api"}}}
	round := func() ([]string, []string, []progressCall, []Job) {
		mu.Lock()
		scanned = nil
		mu.Unlock()
		var streamed []string
		var calls []progressCall
		var smu sync.Mutex
		jobs, err := ScanRound(context.Background(), targets, 7, rounds, func(d, total int) {
			smu.Lock()
			calls = append(calls, progressCall{d, total})
			smu.Unlock()
		}, func(owner, repo string, jobs []Job) {
			smu.Lock()
			streamed = append(streamed, owner+"/"+repo)
			smu.Unlock()
		})
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		got := append([]string{}, scanned...)
		sort.Strings(got)
		sort.Strings(streamed)
		return got, streamed, calls, jobs
	}

	all := []string{"org-a/busy", "org-a/pushed", "org-a/quiet", "org-b/api"}
	got, streamed, _, jobs := round()
	if !reflect.DeepEqual(got, all) || !reflect.DeepEqual(streamed, all) || len(jobs) != 1 {
		t.Fatalf("round 1 scanned %v streamed %v jobs %d, want all and 1 job", got, streamed, len(jobs))
	}
	quiet := map[string]int{}
	for r := 2; r < 2+sweepBuckets; r++ {
		got, streamed, calls, jobs := round()
		if !slices.Contains(got, "org-a/busy") || !slices.Contains(got, "org-b/api") || len(jobs) != 1 || !reflect.DeepEqual(got, streamed) {
			t.Fatalf("round %d scanned %v streamed %v jobs %d, want busy and api every round", r, got, streamed, len(jobs))
		}
		if calls[len(calls)-1] != (progressCall{4, 4}) || calls[1].done != 4-len(got) {
			t.Fatalf("round %d progress = %v, want skipped repos counted as done", r, calls)
		}
		for _, g := range got {
			if g == "org-a/pushed" || g == "org-a/quiet" {
				quiet[g]++
			}
		}
	}
	if quiet["org-a/pushed"] != 1 || quiet["org-a/quiet"] != 1 {
		t.Fatalf("quiet repos swept %v over %d rounds, want each exactly once", quiet, sweepBuckets)
	}
	mu.Lock()
	pushedAt = base.Add(time.Minute)
	mu.Unlock()
	if got, _, _, _ := round(); !slices.Contains(got, "org-a/pushed") {
		t.Fatalf("after a push scanned %v, want org-a/pushed", got)
	}
}

func TestScanRoundRetriesRepoAfterFailedFetch(t *testing.T) {
	resetRepoCache(t)
	pushed := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	var mu sync.Mutex
	fail := 0
	scanned := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"busy","pushed_at":%q}]`, pushed)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			fmt.Fprint(w, `{"total_count":1}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if r.URL.Query().Get("status") == "queued" {
				scanned++
				if fail != 0 {
					w.WriteHeader(fail)
					return
				}
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"run_id":7,"status":"queued","name":"build"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	rounds := NewRounds()
	rounds.Hint("org-a", "busy")
	targets := []Target{{Owner: "org-a", All: true}}
	round := func() (int, []Job) {
		mu.Lock()
		scanned = 0
		mu.Unlock()
		jobs, err := ScanRound(context.Background(), targets, 7, rounds, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return scanned, jobs
	}

	if n, jobs := round(); n != 1 || len(jobs) != 1 {
		t.Fatalf("first round scanned %d jobs %d, want the busy repo and its job", n, len(jobs))
	}
	failOnce := func(code int) (int, []Job) {
		for sweepBucket("org-a/busy") == (rounds.count+1)%sweepBuckets {
			round()
		}
		mu.Lock()
		fail = code
		mu.Unlock()
		round()
		mu.Lock()
		fail = 0
		mu.Unlock()
		return round()
	}
	if n, jobs := failOnce(http.StatusBadGateway); n != 1 || len(jobs) != 1 {
		t.Fatalf("round after a 502 scanned %d jobs %d, want the repo checked again", n, len(jobs))
	}
	if n, _ := failOnce(http.StatusForbidden); n != 0 {
		t.Fatalf("round after a 403 scanned %d, want the repo left to its sweep bucket", n)
	}
}

func TestRoundsSaveAndLoad(t *testing.T) {
	useCacheDir(t)
	useClient(t, cacheClient("tok-a"))
	r := NewRounds()
	pushed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.mark("o/busy", pushed, true)
	r.mark("o/quiet", pushed, false)
	r.next()
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	got := LoadRounds()
	if got.count != 1 || !reflect.DeepEqual(got.seen, r.seen) {
		t.Fatalf("loaded count %d seen %v, want %v", got.count, got.seen, r.seen)
	}
	if !got.needs("o/busy", pushed, 1) || got.needs("o/quiet", pushed, sweepBucket("o/quiet")+1) {
		t.Fatal("loaded memory not used")
	}
	useClient(t, cacheClient("tok-b"))
	if other := LoadRounds(); len(other.seen) != 0 {
		t.Fatal("other token loaded the memory")
	}
	useClient(t, cacheClient("tok-a"))
	path := cacheFile("rounds.json")
	data, _ := os.ReadFile(path)
	var disk diskRounds
	json.Unmarshal(data, &disk)
	disk.Saved = time.Now().Add(-roundsMaxAge - time.Minute)
	data, _ = json.Marshal(disk)
	os.WriteFile(path, data, 0o600)
	if old := LoadRounds(); len(old.seen) != 0 {
		t.Fatal("memory older than roundsMaxAge loaded")
	}
}

func TestScanRequestsRunInParallelUnderTheCap(t *testing.T) {
	resetRepoCache(t)
	var open, peak atomic.Int64
	var jobsOpen, jobsPeak atomic.Int64
	var names []string
	for i := 0; i < 12; i++ {
		names = append(names, fmt.Sprintf(`{"name":"r%d","pushed_at":%q}`, i, time.Now().UTC().Format(time.RFC3339)))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := open.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer open.Add(-1)
		isJobs := strings.HasSuffix(r.URL.Path, "/jobs")
		if isJobs {
			jn := jobsOpen.Add(1)
			for {
				p := jobsPeak.Load()
				if jn <= p || jobsPeak.CompareAndSwap(p, jn) {
					break
				}
			}
			defer jobsOpen.Add(-1)
		}
		time.Sleep(40 * time.Millisecond)
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprint(w, "["+strings.Join(names, ",")+"]")
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			if strings.Contains(r.URL.Path, "/r0/") && r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":1},{"id":2},{"id":3}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case isJobs:
			fmt.Fprint(w, `{"jobs":[{"run_id":1,"status":"queued","name":"j"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	jobs, err := ScanTargets(context.Background(), []Target{{Owner: "org-a", All: true}}, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(jobs))
	}
	if got := peak.Load(); got > workers {
		t.Fatalf("peak in-flight requests = %d, want at most %d", got, workers)
	}
	if got := peak.Load(); got < workers {
		t.Fatalf("peak in-flight requests = %d, want the cap %d reached", got, workers)
	}
	if got := jobsPeak.Load(); got < 2 {
		t.Fatalf("one repo's job requests overlapped %d at most, want them in parallel", got)
	}
}

func TestRollingSweepSpreadsQuietRepos(t *testing.T) {
	r := NewRounds()
	pushed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var keys []string
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("org/repo-%d", i)
		keys = append(keys, key)
		r.mark(key, pushed, false)
	}
	r.mark("org/busy", pushed, true)
	seen := map[string]int{}
	for round := 1; round <= sweepBuckets; round++ {
		n := 0
		for _, k := range keys {
			if r.needs(k, pushed, round) {
				seen[k]++
				n++
			}
		}
		if n > 2*len(keys)/sweepBuckets {
			t.Fatalf("round %d swept %d of %d quiet repos, want them spread across rounds", round, n, len(keys))
		}
		if !r.needs("org/busy", pushed, round) {
			t.Fatalf("round %d skipped the busy repo", round)
		}
	}
	for _, k := range keys {
		if seen[k] != 1 {
			t.Fatalf("%s swept %d times in %d rounds, want once", k, seen[k], sweepBuckets)
		}
	}
	if !r.needs("org/repo-0", pushed.Add(time.Second), sweepBucket("org/repo-0")+1) {
		t.Fatal("a newer push did not force a scan")
	}
}

func TestFirstPassIsSliced(t *testing.T) {
	r := NewRounds()
	old := time.Now().Add(-3 * 24 * time.Hour)
	var keys []string
	for i := 0; i < 40; i++ {
		keys = append(keys, fmt.Sprintf("org/old-%d", i))
	}
	r.Hint("Org", "Old-7")
	fresh := time.Now().Add(-time.Hour)
	checked := map[string]int{}
	for round := 0; round < sweepBuckets; round++ {
		var picked []string
		for _, k := range keys {
			if r.needs(k, old, round) {
				picked = append(picked, k)
			}
		}
		if round == 0 {
			if !r.needs("org/fresh", fresh, round) {
				t.Fatal("a repo pushed in the last day was not checked in the first round")
			}
			if !slices.Contains(picked, "org/old-7") {
				t.Fatal("a hinted repo was not checked in the first round")
			}
			if len(picked) > 2*len(keys)/sweepBuckets+1 {
				t.Fatalf("first round checked %d of %d old repos, want a slice", len(picked), len(keys))
			}
		}
		for _, k := range picked {
			checked[k]++
			r.mark(k, old, false)
		}
	}
	for _, k := range keys {
		want := 1
		if k == "org/old-7" && sweepBucket(k) != 0 {
			want = 2
		}
		if checked[k] != want {
			t.Fatalf("%s checked %d times in the first %d rounds, want %d", k, checked[k], sweepBuckets, want)
		}
	}
}

func TestScanRoundReportsUnseenUntilFirstPassEnds(t *testing.T) {
	resetRepoCache(t)
	old := time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	var names []string
	for i := 0; i < 12; i++ {
		names = append(names, fmt.Sprintf(`{"name":"r%d","pushed_at":%q}`, i, old))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprint(w, "["+strings.Join(names, ",")+"]")
		default:
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[]}`)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)
	rounds := NewRounds()
	var unseen []int
	for i := 0; i < sweepBuckets; i++ {
		if _, err := ScanRound(context.Background(), []Target{{Owner: "org-a", All: true}}, 0, rounds, nil, nil); err != nil {
			t.Fatal(err)
		}
		unseen = append(unseen, rounds.Unseen())
	}
	if unseen[0] == 0 || unseen[0] == 12 || unseen[len(unseen)-1] != 0 {
		t.Fatalf("unseen per round = %v, want some left after round 1 and none after %d", unseen, sweepBuckets)
	}
	for i := 1; i < len(unseen); i++ {
		if unseen[i] > unseen[i-1] {
			t.Fatalf("unseen grew: %v", unseen)
		}
	}
}

func TestNoWorkflowRepoIsSkippedUntilPushed(t *testing.T) {
	resetRepoCache(t)
	var mu sync.Mutex
	pushed := time.Now().Add(-3 * 24 * time.Hour)
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"docs","pushed_at":%q}]`, pushed.UTC().Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			asked["workflows"]++
			fmt.Fprint(w, `{"total_count":0}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			asked["runs"]++
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)
	rounds := NewRounds()
	rounds.Hint("org-a", "docs")
	for i := 0; i < 2*sweepBuckets; i++ {
		if _, err := ScanRound(context.Background(), []Target{{Owner: "org-a", All: true}}, 0, rounds, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if asked["workflows"] != 1 || asked["runs"] != 0 {
		t.Fatalf("over %d rounds asked %v, want one workflows check and no runs requests", 2*sweepBuckets, asked)
	}
	pushed = time.Now().Add(time.Minute)
	mu.Unlock()
	if _, err := ScanRound(context.Background(), []Target{{Owner: "org-a", All: true}}, 0, rounds, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["workflows"] != 2 {
		t.Fatalf("after a push asked %v, want the workflows checked again", asked)
	}
}

func TestFinishedRunsListedWithoutKeepingRepoActive(t *testing.T) {
	resetRepoCache(t)
	now := time.Now().UTC()
	var mu sync.Mutex
	var scanned []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/users/org-a":
			fmt.Fprint(w, `{"login":"org-a","type":"Organization"}`)
		case r.URL.Path == "/orgs/org-a/repos":
			fmt.Fprintf(w, `[{"name":"done","pushed_at":%q}]`, now.Add(-2*time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			fmt.Fprint(w, `{"total_count":1}`)
		case r.URL.Path == "/repos/org-a/done/actions/runs":
			status := r.URL.Query().Get("status")
			if status != "completed" {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
				return
			}
			scanned = append(scanned, "done")
			if r.URL.Query().Get("per_page") != fmt.Sprint(finishedPerRepo) {
				t.Errorf("finished per_page = %s", r.URL.Query().Get("per_page"))
			}
			fmt.Fprintf(w, `{"workflow_runs":[{"id":1,"name":"CI","head_branch":"main","conclusion":"failure","updated_at":%q,"html_url":"https://gh/r/1"},{"id":2,"name":"Old","conclusion":"success","updated_at":%q}]}`,
				now.Add(-time.Hour).Format(time.RFC3339), now.Add(-finishedWindow-time.Hour).Format(time.RFC3339))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	rounds := NewRounds()
	targets := []Target{{Owner: "org-a", All: true}}
	round := func() (int, []Job) {
		mu.Lock()
		scanned = nil
		mu.Unlock()
		jobs, err := ScanRound(context.Background(), targets, 7, rounds, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return len(scanned), jobs
	}

	n, jobs := round()
	if n != 1 || len(jobs) != 1 {
		t.Fatalf("first round scanned %d jobs %+v, want one finished run inside the window", n, jobs)
	}
	j := jobs[0]
	if j.Status != "completed" || j.RunID != 1 || j.Conclusion != "failure" || j.Workflow != "CI" || j.FinishedAt.IsZero() || Active(jobs) != 0 {
		t.Fatalf("finished run = %+v", j)
	}
	if running, queued := SplitJobs(jobs); len(running) != 0 || len(queued) != 0 {
		t.Fatalf("finished run split into running %v queued %v", running, queued)
	}
	swept := 0
	for r := 0; r < sweepBuckets; r++ {
		if n, _ := round(); n > 0 {
			swept++
		}
	}
	if swept != 1 {
		t.Fatalf("repo with only finished runs scanned %d times in %d rounds, want once", swept, sweepBuckets)
	}
	for r := 0; r < sweepBuckets; r++ {
		if sweepBucket("org-a/done") != rounds.count%sweepBuckets {
			break
		}
		round()
	}
	rounds.Wake("org-a", "done")
	if n, _ := round(); n != 1 {
		t.Fatal("woken repo not scanned in the next round")
	}
}

func TestFinishedJobsNewestFirst(t *testing.T) {
	now := time.Now()
	jobs := []Job{
		{RunID: 1, Status: "completed", FinishedAt: now.Add(-2 * time.Hour)},
		{RunID: 2, Status: "queued"},
		{RunID: 3, Status: "completed", FinishedAt: now.Add(-time.Minute)},
		{RunID: 4, Status: "in_progress"},
	}
	got := FinishedJobs(jobs)
	if len(got) != 2 || got[0].RunID != 3 || got[1].RunID != 1 {
		t.Fatalf("finished = %+v", got)
	}
	if Active(jobs) != 2 {
		t.Fatalf("active = %d, want 2", Active(jobs))
	}
}

func TestReposStreamOneAfterAnother(t *testing.T) {
	resetRepoCache(t)
	const n = 40
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		defer served.Add(1)
		switch {
		case r.URL.Path == "/users/o":
			fmt.Fprint(w, `{"login":"o","type":"Organization"}`)
		case r.URL.Path == "/orgs/o/repos":
			var parts []string
			for i := 0; i < n; i++ {
				parts = append(parts, fmt.Sprintf(`{"name":"r%02d","pushed_at":%q}`, i, time.Now().Add(-time.Hour).Format(time.RFC3339)))
			}
			fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
		case strings.HasSuffix(r.URL.Path, "/workflows"):
			fmt.Fprint(w, `{"total_count":1}`)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			if r.URL.Query().Get("status") == "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":1}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"run_id":1,"status":"in_progress","name":"b"}]}`)
		}
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	rounds := NewRounds()
	rounds.Hint("o", "r39")
	var mu sync.Mutex
	var order []string
	var firstAt int64 = -1
	_, err := ScanRound(context.Background(), []Target{{Owner: "o", All: true}}, 7, rounds, nil, func(owner, repo string, jobs []Job) {
		mu.Lock()
		defer mu.Unlock()
		if firstAt < 0 {
			firstAt = served.Load()
		}
		order = append(order, repo)
	})
	if err != nil {
		t.Fatal(err)
	}
	total := served.Load()
	if firstAt*4 > total {
		t.Fatalf("first repo shown after %d of %d requests, want within the first quarter", firstAt, total)
	}
	if i := slices.Index(order, "r39"); i < 0 || i > 2 {
		t.Fatalf("hinted busy repo streamed at position %d, want among the first 3: %v", i, order[:5])
	}
}

func TestSlotPoolServesLowerPriorityFirst(t *testing.T) {
	p := &slotPool{free: 1}
	if !p.acquire(context.Background()) {
		t.Fatal("first acquire failed")
	}
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	waiting := func(n int) {
		for {
			p.mu.Lock()
			queued := len(p.wait)
			p.mu.Unlock()
			if queued == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	for i, prio := range []int{5, 2, 9, 0} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.acquire(withPriority(context.Background(), prio)) {
				mu.Lock()
				got = append(got, prio)
				mu.Unlock()
				p.release()
			}
		}()
		waiting(i + 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan bool)
	go func() { cancelled <- p.acquire(withPriority(ctx, -1)) }()
	waiting(5)
	cancel()
	if <-cancelled {
		t.Fatal("cancelled waiter got a slot")
	}
	p.release()
	wg.Wait()
	if !slices.Equal(got, []int{0, 2, 5, 9}) {
		t.Fatalf("served %v, want lowest priority first", got)
	}
	if p.free != 1 || len(p.wait) != 0 {
		t.Fatalf("pool left free=%d waiting=%d, want 1 and 0", p.free, len(p.wait))
	}
}
