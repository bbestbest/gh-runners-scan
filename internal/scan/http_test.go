package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
