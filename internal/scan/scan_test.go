package scan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		if !strings.HasPrefix(r.URL.Path, "/orgs/org-a/repos") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintf(w, `[{"name":"gamma","pushed_at":%q,"archived":false}]`, now)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/org-a/repos?page=2>; rel="next"`, srv.URL))
		fmt.Fprintf(w, `[{"name":"alpha","pushed_at":%q,"archived":false},{"name":"stale","pushed_at":"2020-01-01T00:00:00Z","archived":false},{"name":"old","pushed_at":%q,"archived":true}]`, now, now)
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
				{"status":"in_progress","workflow_name":"CI","head_branch":"main","name":"build","runner_name":"rnr-1","labels":["self-hosted","linux"],"started_at":"2026-09-20T10:00:00Z","created_at":"2026-09-20T09:59:00Z","html_url":"https://gh/j/1"},
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
