package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testClient(base string) *client {
	return &client{base: base, token: "test-token", http: &http.Client{}, cache: newETagCache()}
}

func TestETag304ReusesCachedBody(t *testing.T) {
	var hits atomic.Int64
	var conditional atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `W/"v1"` {
			conditional.Add(1)
			w.Header().Set("ETag", `W/"v1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `W/"v1"`)
		fmt.Fprint(w, `{"value":"first"}`)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	body, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if string(body) != `{"value":"first"}` {
		t.Fatalf("first body = %q", body)
	}

	body2, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if string(body2) != `{"value":"first"}` {
		t.Fatalf("304 did not reuse cached body, got %q", body2)
	}
	if conditional.Load() != 1 {
		t.Fatalf("expected 1 conditional request, got %d", conditional.Load())
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 server hits, got %d", hits.Load())
	}
}

func TestETag200ReplacesCachedBody(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := version.Load()
		tag := fmt.Sprintf(`W/"v%d"`, v)
		if r.Header.Get("If-None-Match") == tag {
			w.Header().Set("ETag", tag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", tag)
		fmt.Fprintf(w, `{"value":"body%d"}`, v)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if body, _, err := c.fetch(context.Background(), srv.URL+"/x"); err != nil || string(body) != `{"value":"body1"}` {
		t.Fatalf("first = %q %v", body, err)
	}
	version.Store(2)
	body, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatalf("changed fetch: %v", err)
	}
	if string(body) != `{"value":"body2"}` {
		t.Fatalf("200 did not replace cached body, got %q", body)
	}
	if body3, _, _ := c.fetch(context.Background(), srv.URL+"/x"); string(body3) != `{"value":"body2"}` {
		t.Fatalf("new etag not stored, got %q", body3)
	}
}

func TestETagCacheConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `W/"c"` {
			w.Header().Set("ETag", `W/"c"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `W/"c"`)
		fmt.Fprintf(w, `{"p":%q}`, r.URL.Path)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		for round := 0; round < 4; round++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				want := fmt.Sprintf(`{"p":"/r%d"}`, i)
				body, _, err := c.fetch(context.Background(), fmt.Sprintf("%s/r%d", srv.URL, i))
				if err != nil || string(body) != want {
					t.Errorf("got %q %v want %q", body, err, want)
				}
			}(i)
		}
	}
	wg.Wait()
}

func TestRateLimitErrorFrom403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if !isRateLimit(err) {
		t.Fatalf("403 with remaining=0 should be rate limit, got %v", err)
	}
}

func TestNonRateLimitErrorNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if err == nil || isRateLimit(err) {
		t.Fatalf("404 should be a plain error, got %v", err)
	}
}

func TestAuthHeaderSentAndTokenNotLeaked(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_, _, err := c.fetch(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bearer test-token" {
		t.Fatalf("auth header = %q", got)
	}
	rl := &rateLimitError{}
	if msg := rl.Error(); msg != "github api rate limited" {
		t.Fatalf("error text = %q", msg)
	}
}

func TestNextPageURL(t *testing.T) {
	cases := []struct {
		name string
		link string
		want string
	}{
		{"empty", "", ""},
		{"next only", `<https://api.github.com/x?page=2>; rel="next"`, "https://api.github.com/x?page=2"},
		{"next and last", `<https://a/x?page=2>; rel="next", <https://a/x?page=9>; rel="last"`, "https://a/x?page=2"},
		{"prev and last only", `<https://a/x?page=1>; rel="prev", <https://a/x?page=9>; rel="last"`, ""},
		{"malformed", `garbage`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPageURL(tc.link); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRepoEntryDecodesRESTFieldNames(t *testing.T) {
	var entries []repoEntry
	raw := `[{"name":"alpha","pushed_at":"2026-09-20T10:00:00Z","archived":false}]`
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" {
		t.Fatalf("entries = %+v", entries)
	}
	if parseTime(entries[0].PushedAt).IsZero() {
		t.Fatal("pushed_at did not decode; struct tag likely still gh-style pushedAt")
	}
}

func TestRunsResponseDecodesRESTShape(t *testing.T) {
	var runs runsResponse
	raw := `{"total_count":1,"workflow_runs":[{"id":4242,"name":"ci"}]}`
	if err := json.Unmarshal([]byte(raw), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs.WorkflowRuns) != 1 || runs.WorkflowRuns[0].DatabaseID != 4242 {
		t.Fatalf("runs = %+v; REST uses id inside workflow_runs, not databaseId", runs)
	}
}

func TestCancelRunStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		check   func(error) bool
	}{
		{"accepted", http.StatusAccepted, nil, func(err error) bool { return err == nil }},
		{"conflict", http.StatusConflict, nil, func(err error) bool { return errors.Is(err, ErrRunFinished) }},
		{"forbidden without rate headers", http.StatusForbidden, nil, func(err error) bool { return errors.Is(err, ErrNoCancelPermission) }},
		{"not found", http.StatusNotFound, nil, func(err error) bool { return errors.Is(err, ErrNoCancelPermission) }},
		{"forbidden rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, isRateLimit},
		{"too many requests", http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, isRateLimit},
		{"server error", http.StatusInternalServerError, nil, func(err error) bool {
			return err != nil && !isRateLimit(err) && !errors.Is(err, ErrNoCancelPermission) && !errors.Is(err, ErrRunFinished)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var method, path, accept, version, auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				accept = r.Header.Get("Accept")
				version = r.Header.Get("X-GitHub-Api-Version")
				auth = r.Header.Get("Authorization")
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := testClient(srv.URL)
			err := c.cancelRun(context.Background(), "org-a", "web", 99)
			if !tc.check(err) {
				t.Fatalf("status %d mapped to %v", tc.status, err)
			}
			if err != nil && strings.Contains(err.Error(), "test-token") {
				t.Fatalf("token leaked in error: %v", err)
			}
			if method != http.MethodPost || path != "/repos/org-a/web/actions/runs/99/cancel" {
				t.Fatalf("request = %s %s", method, path)
			}
			if accept != "application/vnd.github+json" || version != "2022-11-28" || auth != "Bearer test-token" {
				t.Fatalf("headers = %q %q %q", accept, version, auth)
			}
		})
	}
}

func TestCancelRunLeavesETagCacheUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("POST sent If-None-Match")
			}
			w.Header().Set("ETag", `W/"post"`)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("ETag", `W/"v1"`)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if _, _, err := c.fetch(context.Background(), srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := c.cancelRun(context.Background(), "org-a", "web", 5); err != nil {
		t.Fatal(err)
	}
	if len(c.cache.entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(c.cache.entries))
	}
	if entry, ok := c.cache.get(srv.URL + "/x"); !ok || entry.etag != `W/"v1"` {
		t.Fatalf("cached GET entry changed: %+v %v", entry, ok)
	}
}

func TestCancelRunReportsRateLimitSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	withClient(t, srv.URL)

	if err := CancelRun(context.Background(), "org-a", "web", 1); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func rateHeaders(w http.ResponseWriter, limit, remaining, used int, reset int64, resource string) {
	w.Header().Set("X-RateLimit-Limit", fmt.Sprint(limit))
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(remaining))
	w.Header().Set("X-RateLimit-Used", fmt.Sprint(used))
	w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset))
	w.Header().Set("X-RateLimit-Resource", resource)
}

func TestUsageRecordsHeadersAndCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			rateHeaders(w, 5000, 4900, 100, 1900000000, "core")
			w.Header().Set("ETag", `W/"v1"`)
			fmt.Fprint(w, `{}`)
		case "/cached":
			if r.Header.Get("If-None-Match") != "" {
				rateHeaders(w, 5000, 4900, 100, 1900000000, "core")
				w.WriteHeader(http.StatusNotModified)
				return
			}
			rateHeaders(w, 5000, 4901, 99, 1900000000, "core")
			w.Header().Set("ETag", `W/"v2"`)
			fmt.Fprint(w, `{}`)
		case "/bare":
			fmt.Fprint(w, `{}`)
		case "/limited":
			rateHeaders(w, 5000, 0, 5000, 1900000500, "core")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if got := c.usage.snapshot(); got.HasData || got.Requests != 0 {
		t.Fatalf("fresh snapshot = %+v", got)
	}

	if _, _, err := c.fetch(context.Background(), srv.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	got := c.usage.snapshot()
	if !got.HasData || got.Limit != 5000 || got.Remaining != 4900 || got.Used != 100 || got.Resource != "core" || got.Reset.Unix() != 1900000000 {
		t.Fatalf("after 200 = %+v", got)
	}
	if got.Requests != 1 || got.NotModified != 0 || got.RateLimited != 0 {
		t.Fatalf("counters after 200 = %+v", got)
	}

	if _, _, err := c.fetch(context.Background(), srv.URL+"/cached"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.fetch(context.Background(), srv.URL+"/cached"); err != nil {
		t.Fatal(err)
	}
	got = c.usage.snapshot()
	if got.Requests != 3 || got.NotModified != 1 || got.Remaining != 4900 || got.Used != 100 {
		t.Fatalf("after 304 = %+v", got)
	}

	if _, _, err := c.fetch(context.Background(), srv.URL+"/bare"); err != nil {
		t.Fatal(err)
	}
	bare := c.usage.snapshot()
	if bare.Requests != 4 {
		t.Fatalf("requests = %d", bare.Requests)
	}
	bare.Requests = got.Requests
	if bare != got {
		t.Fatalf("headerless response changed values: %+v vs %+v", bare, got)
	}

	if _, _, err := c.fetch(context.Background(), srv.URL+"/limited"); !isRateLimit(err) {
		t.Fatalf("err = %v", err)
	}
	got = c.usage.snapshot()
	if got.RateLimited != 1 || got.Remaining != 0 || got.Requests != 5 {
		t.Fatalf("after 403 = %+v", got)
	}
}

func TestUsageCountsPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateHeaders(w, 5000, 10, 4990, 1900000000, "core")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	if err := c.post(context.Background(), srv.URL+"/cancel"); err != nil {
		t.Fatal(err)
	}
	got := c.usage.snapshot()
	if got.Requests != 1 || got.Remaining != 10 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestUsageConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateHeaders(w, 5000, 4000, 1000, 1900000000, "core")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				c.fetch(context.Background(), srv.URL+"/x")
				c.usage.snapshot()
			}
		}()
	}
	wg.Wait()
	if got := c.usage.snapshot(); got.Requests != 100 || got.Remaining != 4000 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func usageResp(limit, remaining, used int, reset int64) *http.Response {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", fmt.Sprint(limit))
	h.Set("X-RateLimit-Remaining", fmt.Sprint(remaining))
	h.Set("X-RateLimit-Used", fmt.Sprint(used))
	if reset != 0 {
		h.Set("X-RateLimit-Reset", fmt.Sprint(reset))
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h}
}

func TestUsageKeepsMinRemainingWithinWindow(t *testing.T) {
	var u usageStats
	u.record(usageResp(5000, 4090, 910, 1900000000))
	u.record(usageResp(5000, 4080, 920, 1900000000))
	u.record(usageResp(5000, 4085, 915, 1900000000))
	got := u.snapshot()
	if got.Remaining != 4080 || got.Used != 920 || got.Limit != 5000 || got.Reset.Unix() != 1900000000 || got.Requests != 3 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestUsageNewWindowReplaces(t *testing.T) {
	var u usageStats
	u.record(usageResp(5000, 4080, 920, 1900000000))
	u.record(usageResp(5000, 5000, 0, 1900003600))
	got := u.snapshot()
	if got.Remaining != 5000 || got.Used != 0 || got.Reset.Unix() != 1900003600 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestUsageStaleWindowIgnored(t *testing.T) {
	var u usageStats
	u.record(usageResp(5000, 4990, 10, 1900003600))
	u.record(usageResp(5000, 100, 4900, 1900000000))
	got := u.snapshot()
	if got.Remaining != 4990 || got.Used != 10 || got.Reset.Unix() != 1900003600 || got.Requests != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestUsageMissingResetAppliesMinMax(t *testing.T) {
	var u usageStats
	u.record(usageResp(5000, 4000, 1000, 1900000000))
	u.record(usageResp(5000, 4500, 500, 0))
	u.record(usageResp(5000, 3900, 1100, 0))
	got := u.snapshot()
	if got.Remaining != 3900 || got.Used != 1100 || got.Reset.Unix() != 1900000000 {
		t.Fatalf("snapshot = %+v", got)
	}
}
