package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	used    map[string]bool
}

func newETagCache() *etagCache {
	return &etagCache{entries: map[string]etagEntry{}, used: map[string]bool{}}
}

func (c *etagCache) get(url string) (etagEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[url]
	if ok {
		c.used[url] = true
	}
	return entry, ok
}

func (c *etagCache) put(url, etag string, body []byte) {
	if etag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[url] = etagEntry{etag: etag, body: body}
	c.used[url] = true
}

type client struct {
	base  string
	token string
	http  *http.Client
	cache *etagCache
	usage usageStats
}

type usageStats struct {
	mu          sync.Mutex
	snap        UsageSnapshot
	requests    int64
	notModified int64
	limited     int64
}

type UsageSnapshot struct {
	HasData     bool
	Resource    string
	Limit       int
	Remaining   int
	Used        int
	Reset       time.Time
	Requests    int64
	NotModified int64
	RateLimited int64
}

func (u *usageStats) record(resp *http.Response) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests++
	if resp.StatusCode == http.StatusNotModified {
		u.notModified++
	}
	if isRateLimitStatus(resp) {
		u.limited++
	}
	h := resp.Header
	limit, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit")))
	if err != nil {
		return
	}
	remaining, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	if err != nil {
		return
	}
	used, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Used")))
	if err != nil {
		used = limit - remaining
	}
	var reset time.Time
	hasReset := false
	if epoch, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil {
		reset = time.Unix(epoch, 0)
		hasReset = true
	}
	if r := h.Get("X-RateLimit-Resource"); r != "" {
		u.snap.Resource = r
	}
	switch {
	case !u.snap.HasData || (hasReset && reset.After(u.snap.Reset)):
		u.snap.HasData = true
		u.snap.Limit = limit
		u.snap.Remaining = remaining
		u.snap.Used = used
		if hasReset {
			u.snap.Reset = reset
		}
	case hasReset && reset.Before(u.snap.Reset):
	default:
		u.snap.Limit = limit
		u.snap.Remaining = min(u.snap.Remaining, remaining)
		u.snap.Used = max(u.snap.Used, used)
	}
}

func (u *usageStats) snapshot() UsageSnapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.snap
	s.Requests = u.requests
	s.NotModified = u.notModified
	s.RateLimited = u.limited
	return s
}

func Usage() UsageSnapshot {
	c, err := sharedClient()
	if err != nil {
		return UsageSnapshot{}
	}
	return c.usage.snapshot()
}

var (
	defaultClientOnce sync.Once
	defaultClient     *client
	defaultClientErr  error
	activeClient      atomic.Pointer[client]
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
			cache: loadETagCache(token),
		}
		activeClient.Store(defaultClient)
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

func (c *client) getInto(ctx context.Context, url string, out any) (string, error) {
	body, link, etag, err := c.fetch(ctx, url)
	if err != nil {
		return "", err
	}
	if out == nil {
		return link, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", err
	}
	trimmed, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	c.cache.put(url, etag, trimmed)
	return link, nil
}

func (c *client) fetch(ctx context.Context, url string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", "", err
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
		return nil, "", "", err
	}
	defer resp.Body.Close()
	c.usage.record(resp)

	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.body, resp.Header.Get("Link"), "", nil
	}

	if isRateLimitStatus(resp) {
		io.Copy(io.Discard, resp.Body)
		if d, ok := retryAfter(resp); ok {
			return nil, "", "", &rateLimitError{wait: d}
		}
		return nil, "", "", &rateLimitError{}
	}

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, "", "", fmt.Errorf("github api: %s for %s", resp.Status, req.URL.Path)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	return body, resp.Header.Get("Link"), resp.Header.Get("ETag"), nil
}

var (
	ErrRunFinished        = errors.New("run already finished or cannot be cancelled")
	ErrNoCancelPermission = errors.New("cancel refused: token likely lacks actions:write (fine-grained) or repo (classic) scope")
)

func (c *client) post(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.usage.record(resp)
	io.Copy(io.Discard, resp.Body)

	if isRateLimitStatus(resp) {
		if d, ok := retryAfter(resp); ok {
			return &rateLimitError{wait: d}
		}
		return &rateLimitError{}
	}

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return ErrRunFinished
	case http.StatusForbidden, http.StatusNotFound:
		return ErrNoCancelPermission
	}
	return fmt.Errorf("github api: %s for %s", resp.Status, req.URL.Path)
}

func (c *client) cancelRun(ctx context.Context, owner, repo string, runID int64) error {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/actions/runs/" + strconv.FormatInt(runID, 10) + "/cancel"
	return c.post(ctx, c.base+path)
}

func CancelRun(ctx context.Context, owner, repo string, runID int64) error {
	c, err := sharedClient()
	if err != nil {
		return err
	}
	err = c.cancelRun(ctx, owner, repo, runID)
	if isRateLimit(err) {
		return ErrRateLimited
	}
	return err
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
