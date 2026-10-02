package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, _, err := a.fetch(context.Background(), srv.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}

	b := cacheClient("tok")
	body, _, err := b.fetch(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"path":"/x"}` {
		t.Fatalf("body = %q", body)
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
		if _, _, err := a.fetch(context.Background(), srv.URL+"/x"); err != nil {
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
			if _, _, err := c.fetch(context.Background(), srv.URL+"/x"); err != nil {
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
		if _, _, err := a.fetch(context.Background(), srv.URL+p); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.saveCache(); err != nil {
		t.Fatal(err)
	}

	b := cacheClient("tok")
	if _, _, err := b.fetch(context.Background(), srv.URL+"/kept"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.fetch(context.Background(), srv.URL+"/new"); err != nil {
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
	if _, _, err := a.fetch(context.Background(), srv.URL+"/x"); err != nil {
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
	if _, _, err := a.fetch(context.Background(), srv.URL+"/x"); err != nil {
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
