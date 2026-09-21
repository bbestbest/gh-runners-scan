package scan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
		if !strings.HasPrefix(r.URL.Path, "/orgs/acme/repos") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintf(w, `[{"name":"gamma","pushed_at":%q,"archived":false}]`, now)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?page=2>; rel="next"`, srv.URL))
		fmt.Fprintf(w, `[{"name":"alpha","pushed_at":%q,"archived":false},{"name":"stale","pushed_at":"2020-01-01T00:00:00Z","archived":false},{"name":"old","pushed_at":%q,"archived":true}]`, now, now)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	var limited atomic.Bool
	names := listRepos(context.Background(), &limited, "acme", 7)
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
		case r.URL.Path == "/repos/acme/web/actions/runs":
			status := r.URL.Query().Get("status")
			if r.URL.Query().Get("per_page") != "20" {
				t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
			}
			if status == "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case r.URL.Path == "/repos/acme/web/actions/runs/7/jobs":
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
	jobs := scanRepo(context.Background(), &limited, "acme", "web")
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
	scanRepo(context.Background(), &limited, "acme", "web")
	firstFull := full.Load()
	scanRepo(context.Background(), &limited, "acme", "web")

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

	jobs, err := Scan(context.Background(), "acme", 7, nil)
	if err != ErrRateLimited {
		t.Fatalf("err = %v want ErrRateLimited", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}
