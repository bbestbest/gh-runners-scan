package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const cacheVersion = 3

const repoCacheVersion = 2

type diskEntry struct {
	ETag string `json:"etag"`
	Body string `json:"body"`
}

type diskCache struct {
	Version   int                  `json:"version"`
	TokenHash string               `json:"token_hash"`
	Entries   map[string]diskEntry `json:"entries"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func cacheFile(name string) string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "gh-runners", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "gh-runners", name)
}

func cachePath() string {
	return cacheFile("etags.json")
}

type diskRepoEntry struct {
	Repos   []repoInfo `json:"repos"`
	Days    int        `json:"days"`
	Fetched time.Time  `json:"fetched"`
}

type diskRepos struct {
	Version   int                      `json:"version"`
	TokenHash string                   `json:"token_hash"`
	Entries   map[string]diskRepoEntry `json:"entries"`
}

func loadRepoCache(token string) {
	path := cacheFile("repos.json")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var disk diskRepos
	if json.Unmarshal(data, &disk) != nil {
		return
	}
	if disk.Version != repoCacheVersion || disk.TokenHash != tokenHash(token) {
		return
	}
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	for key, e := range disk.Entries {
		if time.Since(e.Fetched) <= repoCacheTTL {
			repoCache[key] = repoCacheEntry{repos: e.Repos, days: e.Days, fetched: e.Fetched}
		}
	}
}

func (c *client) saveRepoCache() error {
	path := cacheFile("repos.json")
	if path == "" {
		return nil
	}
	repoCacheMu.Lock()
	entries := make(map[string]diskRepoEntry, len(repoCache))
	for key, e := range repoCache {
		if time.Since(e.fetched) <= repoCacheTTL {
			entries[key] = diskRepoEntry{Repos: e.repos, Days: e.days, Fetched: e.fetched}
		}
	}
	repoCacheMu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	data, err := json.Marshal(diskRepos{
		Version:   repoCacheVersion,
		TokenHash: tokenHash(c.token),
		Entries:   entries,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, data)
}

func loadETagCache(token string) *etagCache {
	c := newETagCache()
	path := cachePath()
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var disk diskCache
	if json.Unmarshal(data, &disk) != nil {
		return c
	}
	if disk.Version != cacheVersion || disk.TokenHash != tokenHash(token) {
		return c
	}
	for url, e := range disk.Entries {
		if e.ETag != "" {
			c.entries[url] = etagEntry{etag: e.ETag, body: []byte(e.Body)}
		}
	}
	return c
}

func (c *client) saveCache() error {
	path := cachePath()
	if path == "" {
		return nil
	}
	c.cache.mu.Lock()
	entries := make(map[string]diskEntry, len(c.cache.used))
	for url := range c.cache.used {
		if e, ok := c.cache.entries[url]; ok {
			entries[url] = diskEntry{ETag: e.etag, Body: string(e.body)}
		}
	}
	c.cache.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	data, err := json.Marshal(diskCache{
		Version:   cacheVersion,
		TokenHash: tokenHash(c.token),
		Entries:   entries,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, data)
}

func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".etags-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func SaveCache() error {
	c := activeClient.Load()
	if c == nil {
		return nil
	}
	err := c.saveCache()
	if rerr := c.saveRepoCache(); err == nil {
		err = rerr
	}
	return err
}

const lastJobsVersion = 1

type diskLast struct {
	Version   int       `json:"version"`
	TokenHash string    `json:"token_hash"`
	Key       string    `json:"key"`
	At        time.Time `json:"at"`
	Jobs      []Job     `json:"jobs"`
}

func SaveLastJobs(key string, jobs []Job) error {
	c, err := sharedClient()
	if err != nil {
		return err
	}
	path := cacheFile("last.json")
	if path == "" {
		return nil
	}
	data, err := json.Marshal(diskLast{
		Version:   lastJobsVersion,
		TokenHash: tokenHash(c.token),
		Key:       key,
		At:        time.Now(),
		Jobs:      jobs,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, data)
}

func LoadLastJobs(key string) ([]Job, time.Time, bool) {
	c, err := sharedClient()
	if err != nil {
		return nil, time.Time{}, false
	}
	path := cacheFile("last.json")
	if path == "" {
		return nil, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	var disk diskLast
	if json.Unmarshal(data, &disk) != nil || disk.Version != lastJobsVersion || disk.TokenHash != tokenHash(c.token) || disk.Key != key {
		return nil, time.Time{}, false
	}
	return disk.Jobs, disk.At, true
}

const roundsVersion = 1

const roundsMaxAge = 30 * time.Minute

type diskRounds struct {
	Version   int                       `json:"version"`
	TokenHash string                    `json:"token_hash"`
	Saved     time.Time                 `json:"saved"`
	Count     int                       `json:"count"`
	Seen      map[string]repoMemory     `json:"seen"`
	Workflows map[string]workflowMemory `json:"workflows"`
}

func (r *Rounds) Save() error {
	c, err := sharedClient()
	if err != nil {
		return err
	}
	path := cacheFile("rounds.json")
	if path == "" {
		return nil
	}
	r.mu.Lock()
	seen := make(map[string]repoMemory, len(r.seen))
	for k, v := range r.seen {
		seen[k] = v
	}
	workflows := make(map[string]workflowMemory, len(r.workflows))
	for k, v := range r.workflows {
		workflows[k] = v
	}
	count := r.count
	r.mu.Unlock()
	if len(seen) == 0 && len(workflows) == 0 {
		return nil
	}
	data, err := json.Marshal(diskRounds{Version: roundsVersion, TokenHash: tokenHash(c.token), Saved: time.Now(), Count: count, Seen: seen, Workflows: workflows})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(dir, path, data)
}

func LoadRounds() *Rounds {
	r := NewRounds()
	c, err := sharedClient()
	if err != nil {
		return r
	}
	path := cacheFile("rounds.json")
	if path == "" {
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var disk diskRounds
	if json.Unmarshal(data, &disk) != nil || disk.Version != roundsVersion || disk.TokenHash != tokenHash(c.token) {
		return r
	}
	for k, w := range disk.Workflows {
		if time.Since(w.Checked) <= workflowsTTL {
			r.workflows[k] = w
		}
	}
	if time.Since(disk.Saved) > roundsMaxAge || disk.Seen == nil {
		return r
	}
	r.seen = disk.Seen
	r.count = disk.Count
	return r
}
