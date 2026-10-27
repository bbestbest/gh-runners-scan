package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func etagServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"v1"`)
		if r.Header.Get("If-None-Match") == `W/"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"path":"`+r.URL.Path+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cacheClient(token string) *client {
	return &client{token: token, http: &http.Client{}, cache: loadETagCache(token)}
}

func useCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

func TestCacheRoundTrip(t *testing.T) {
	useCacheDir(t)
	srv := etagServer(t)

	a := cacheClient("tok")
	if _, err := getMap(a, srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}

	b := cacheClient("tok")
	body, err := getMap(b, srv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if js(body) != `{"path":"/x"}` {
		t.Fatalf("body = %v", body)
	}
	if got := b.usage.snapshot().NotModified; got != 1 {
		t.Fatalf("NotModified = %d, want 1", got)
	}
}

func TestCacheStartsColdWithoutError(t *testing.T) {
	srv := etagServer(t)
	good := func(t *testing.T) string {
		dir := useCacheDir(t)
		a := cacheClient("tok")
		if _, err := getMap(a, srv.URL+"/x"); err != nil {
			t.Fatal(err)
		}
		if err := a.saveCache(); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "gh-runners", "etags.json")
	}

	cases := []struct {
		name  string
		token string
		prep  func(t *testing.T, path string)
	}{
		{"corrupt", "tok", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong version", "tok", func(t *testing.T, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk diskCache
			if err := json.Unmarshal(data, &disk); err != nil {
				t.Fatal(err)
			}
			disk.Version = cacheVersion + 1
			data, _ = json.Marshal(disk)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"different token", "other", func(t *testing.T, path string) {}},
		{"missing file", "tok", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := good(t)
			tc.prep(t, path)
			c := cacheClient(tc.token)
			if len(c.cache.entries) != 0 {
				t.Fatalf("expected cold start, got %d entries", len(c.cache.entries))
			}
			if _, err := getMap(c, srv.URL+"/x"); err != nil {
				t.Fatal(err)
			}
			if got := c.usage.snapshot().NotModified; got != 0 {
				t.Fatalf("NotModified = %d, want 0", got)
			}
		})
	}
}

func TestCacheWritesOnlyUsedEntries(t *testing.T) {
	dir := useCacheDir(t)
	srv := etagServer(t)

	a := cacheClient("tok")
	for _, p := range []string{"/old", "/kept"} {
		if _, err := getMap(a, srv.URL+p); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}

	b := cacheClient("tok")
	if _, err := getMap(b, srv.URL+"/kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := getMap(b, srv.URL+"/new"); err != nil {
		t.Fatal(err)
	}
	if err := b.saveCache(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "gh-runners", "etags.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk diskCache
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %v", len(disk.Entries), disk.Entries)
	}
	if _, ok := disk.Entries[srv.URL+"/old"]; ok {
		t.Fatal("unused entry was written")
	}
	for _, p := range []string{"/kept", "/new"} {
		if _, ok := disk.Entries[srv.URL+p]; !ok {
			t.Fatalf("missing %s", p)
		}
	}
}

func TestCacheNeverWritesToken(t *testing.T) {
	dir := useCacheDir(t)
	srv := etagServer(t)
	a := cacheClient("super-secret-token")
	if _, err := getMap(a, srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gh-runners", "etags.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-secret-token") {
		t.Fatal("token written to cache file")
	}
}

func TestCacheFileModes(t *testing.T) {
	dir := useCacheDir(t)
	srv := etagServer(t)
	a := cacheClient("tok")
	if _, err := getMap(a, srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "gh-runners")
	di, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(sub, "etags.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(sub, ".etags-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestCachePathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".cache", "gh-runners", "etags.json")
	if got := cachePath(); got != want {
		t.Fatalf("cachePath = %q, want %q", got, want)
	}
}

func useClient(t *testing.T, c *client) {
	t.Helper()
	prev := defaultClient
	prevErr := defaultClientErr
	prevActive := activeClient.Load()
	defaultClientOnce.Do(func() {})
	defaultClient = c
	defaultClientErr = nil
	activeClient.Store(c)
	t.Cleanup(func() {
		activeClient.Store(prevActive)
		defaultClient = prev
		defaultClientErr = prevErr
	})
}

func sensitiveServer(t *testing.T, conditional *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"s1"`)
		if r.Header.Get("If-None-Match") == `W/"s1"` {
			*conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos"):
			fmt.Fprint(w, `[{"name":"alpha","pushed_at":"2999-01-01T00:00:00Z","archived":false,"description":"SECRET-DESC","owner":{"login":"SECRET-OWNER"}}]`)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":77,"actor":{"login":"SECRET-ACTOR"},"triggering_actor":{"login":"SECRET-TRIG"},"head_commit":{"message":"SECRET-MSG","author":{"name":"SECRET-NAME","email":"secret@example.com"}}}]}`)
		default:
			fmt.Fprint(w, `{"total_count":1,"jobs":[{"run_id":77,"status":"queued","workflow_name":"ci","head_branch":"main","name":"build","runner_name":"r1","labels":["self-hosted","linux"],"started_at":"2026-01-02T03:04:05Z","created_at":"2026-01-02T03:00:00Z","html_url":"https://github.com/o/alpha/actions/runs/77/job/1","steps":[{"name":"Run tests","status":"in_progress","number":1,"conclusion":null}],"actor":{"login":"SECRET-JOBACTOR"}}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCacheStoresOnlyUsedFields(t *testing.T) {
	dir := useCacheDir(t)
	var conditional int
	srv := sensitiveServer(t, &conditional)

	a := cacheClient("tok")
	a.base = srv.URL
	useClient(t, a)
	var limited atomic.Bool
	jobs := scanRepo(context.Background(), &limited, "o", "alpha")
	if len(jobs) == 0 {
		t.Fatal("no jobs")
	}
	var repos []repoEntry
	if _, ok := getJSON(context.Background(), &limited, &repos, "/orgs/o/repos"); !ok {
		t.Fatal("repos fetch failed")
	}
	if err := SaveCache(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "gh-runners", "etags.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, bad := range []string{"SECRET", "secret@example.com", "email", "message", "login", "description", "head_commit", "actor", "conclusion", "number"} {
		if strings.Contains(text, bad) {
			t.Fatalf("cache file contains %q: %s", bad, text)
		}
	}
	for _, want := range []string{"alpha", "pushed_at", "archived", `\"id\":77`, "run_id", "queued", "workflow_name", "head_branch", "build", "runner_name", "self-hosted", "started_at", "created_at", "html_url", "actions/runs/77/job/1", `W/\"s1\"`, "Run tests"} {
		if !strings.Contains(text, want) {
			t.Fatalf("cache file missing %q: %s", want, text)
		}
	}
}

func TestCache304FromTrimmedCacheMatchesOriginal(t *testing.T) {
	useCacheDir(t)
	var conditional int
	srv := sensitiveServer(t, &conditional)

	a := cacheClient("tok")
	a.base = srv.URL
	useClient(t, a)
	var limited atomic.Bool
	first := scanRepo(context.Background(), &limited, "o", "alpha")
	if len(first) != 2 {
		t.Fatalf("first jobs = %d", len(first))
	}
	if err := SaveCache(); err != nil {
		t.Fatal(err)
	}

	b := cacheClient("tok")
	b.base = srv.URL
	useClient(t, b)
	conditional = 0
	second := scanRepo(context.Background(), &limited, "o", "alpha")
	if conditional == 0 {
		t.Fatal("no 304 served")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("jobs differ:\n%+v\n%+v", first, second)
	}
}

func TestCacheOldVersionFileStartsCold(t *testing.T) {
	dir := useCacheDir(t)
	path := filepath.Join(dir, "gh-runners", "etags.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"version":1,"token_hash":%q,"entries":{"http://x/y":{"etag":"W/\"v1\"","body":"{\"head_commit\":{\"message\":\"SECRET\"}}"}}}`, tokenHash("tok"))
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c := cacheClient("tok")
	if len(c.cache.entries) != 0 {
		t.Fatalf("expected cold start, got %d entries", len(c.cache.entries))
	}
}

func reposServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func persistRepos(t *testing.T, token, key string, names []string, expires time.Time) {
	t.Helper()
	resetRepoCache(t)
	repoCacheMu.Lock()
	repoCache[key] = repoCacheEntry{names: names, expires: expires}
	repoCacheMu.Unlock()
	if err := cacheClient(token).saveRepoCache(); err != nil {
		t.Fatal(err)
	}
	resetRepoCache(t)
}

func TestRepoCacheRoundTripServesWithoutRequests(t *testing.T) {
	useCacheDir(t)
	var hits atomic.Int64
	srv := reposServer(t, &hits)
	withClient(t, srv.URL)
	persistRepos(t, "tok", "o\x007", []string{"alpha", "beta"}, time.Now().Add(time.Minute))

	loadRepoCache("tok")
	var limited atomic.Bool
	names := listRepos(context.Background(), &limited, "o", 7)
	if !reflect.DeepEqual(names, []string{"alpha", "beta"}) {
		t.Fatalf("names = %v", names)
	}
	if hits.Load() != 0 {
		t.Fatalf("requests = %d, want 0", hits.Load())
	}
}

func TestRepoCacheColdCases(t *testing.T) {
	cases := []struct {
		name  string
		token string
		key   string
		ttl   time.Duration
		prep  func(t *testing.T, path string)
	}{
		{"expired", "tok", "o\x007", -time.Minute, nil},
		{"different days", "tok", "o\x0030", time.Minute, nil},
		{"different token", "other", "o\x007", time.Minute, nil},
		{"missing file", "tok", "o\x007", time.Minute, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt", "tok", "o\x007", time.Minute, func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong version", "tok", "o\x007", time.Minute, func(t *testing.T, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk diskRepos
			if err := json.Unmarshal(data, &disk); err != nil {
				t.Fatal(err)
			}
			disk.Version = repoCacheVersion + 1
			data, _ = json.Marshal(disk)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := useCacheDir(t)
			persistRepos(t, "tok", "o\x007", []string{"alpha"}, time.Now().Add(tc.ttl))
			if tc.prep != nil {
				tc.prep(t, filepath.Join(dir, "gh-runners", "repos.json"))
			}
			if tc.token == "tok" && tc.key == "o\x0030" {
				loadRepoCache("tok")
				if _, ok := cachedRepos(tc.key); ok {
					t.Fatal("served wrong days")
				}
				return
			}
			loadRepoCache(tc.token)
			if _, ok := cachedRepos("o\x007"); ok {
				t.Fatal("expected cold start")
			}
		})
	}
}

func TestRepoCacheSurvivesRepoOnlyRun(t *testing.T) {
	useCacheDir(t)
	srv := etagServer(t)
	persistRepos(t, "tok", "o\x007", []string{"alpha"}, time.Now().Add(time.Minute))

	loadRepoCache("tok")
	a := cacheClient("tok")
	a.base = srv.URL
	useClient(t, a)
	if _, err := getMap(a, srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := SaveCache(); err != nil {
		t.Fatal(err)
	}

	resetRepoCache(t)
	loadRepoCache("tok")
	if names, ok := cachedRepos("o\x007"); !ok || !reflect.DeepEqual(names, []string{"alpha"}) {
		t.Fatalf("repo list dropped: %v %v", names, ok)
	}
}

func TestRepoCacheFileModesAndNoToken(t *testing.T) {
	dir := useCacheDir(t)
	persistRepos(t, "super-secret-token", "o\x007", []string{"alpha"}, time.Now().Add(time.Minute))
	sub := filepath.Join(dir, "gh-runners")
	di, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	path := filepath.Join(sub, "repos.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-secret-token") {
		t.Fatal("token written to repo cache file")
	}
	left, _ := filepath.Glob(filepath.Join(sub, ".etags-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestListReposLoadsPersistedListBeforeClientExists(t *testing.T) {
	useCacheDir(t)
	t.Setenv("GH_TOKEN", "tok")
	persistRepos(t, "tok", "o\x007", []string{"alpha"}, time.Now().Add(time.Minute))

	prev, prevErr := defaultClient, defaultClientErr
	prevActive := activeClient.Load()
	defaultClientOnce = sync.Once{}
	defaultClient, defaultClientErr = nil, nil
	t.Cleanup(func() {
		defaultClientOnce.Do(func() {})
		defaultClient, defaultClientErr = prev, prevErr
		activeClient.Store(prevActive)
	})

	var limited atomic.Bool
	names := listRepos(context.Background(), &limited, "o", 7)
	if !reflect.DeepEqual(names, []string{"alpha"}) {
		t.Fatalf("names = %v", names)
	}
}
