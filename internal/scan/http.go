package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.github.com"

type etagEntry struct {
	etag string
	body []byte
}

type etagCache struct {
	mu      sync.Mutex
	entries map[string]etagEntry
}

func newETagCache() *etagCache {
	return &etagCache{entries: map[string]etagEntry{}}
}

func (c *etagCache) get(url string) (etagEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[url]
	return entry, ok
}

func (c *etagCache) put(url, etag string, body []byte) {
	if etag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[url] = etagEntry{etag: etag, body: body}
}

type client struct {
	base  string
	token string
	http  *http.Client
	cache *etagCache
}

var (
	defaultClientOnce sync.Once
	defaultClient     *client
	defaultClientErr  error
)

func resolveToken() (string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v, nil
		}
	}
	return "", errors.New("no github token: set GH_TOKEN or GITHUB_TOKEN, or run 'gh auth login'")
}

func sharedClient() (*client, error) {
	defaultClientOnce.Do(func() {
		token, err := resolveToken()
		if err != nil {
			defaultClientErr = err
			return
		}
		defaultClient = &client{
			base:  apiBase,
			token: token,
			http:  &http.Client{Timeout: 30 * time.Second},
			cache: newETagCache(),
		}
	})
	return defaultClient, defaultClientErr
}

func retryAfter(resp *http.Response) (time.Duration, bool) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	v := resp.Header.Get("X-RateLimit-Reset")
	if v == "" {
		return 0, false
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, false
	}
	d := time.Until(time.Unix(epoch, 0))
	if d <= 0 {
		return 0, false
	}
	return d, true
}

func isRateLimitStatus(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	return resp.Header.Get("Retry-After") != ""
}

func (c *client) fetch(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)

	cached, hasCached := c.cache.get(url)
	if hasCached {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.body, resp.Header.Get("Link"), nil
	}

	if isRateLimitStatus(resp) {
		io.Copy(io.Discard, resp.Body)
		if d, ok := retryAfter(resp); ok {
			return nil, "", &rateLimitError{wait: d}
		}
		return nil, "", &rateLimitError{}
	}

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, "", fmt.Errorf("github api: %s for %s", resp.Status, req.URL.Path)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	c.cache.put(url, resp.Header.Get("ETag"), body)
	return body, resp.Header.Get("Link"), nil
}

type rateLimitError struct {
	wait time.Duration
}

func (e *rateLimitError) Error() string { return "github api rate limited" }

func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		raw := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.Contains(strings.ReplaceAll(attr, " ", ""), `rel="next"`) {
				return raw[1 : len(raw)-1]
			}
		}
	}
	return ""
}
