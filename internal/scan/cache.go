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

const repoCacheVersion = 1

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
	Names   []string  `json:"names"`
	Expires time.Time `json:"expires"`
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
	now := time.Now()
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	for key, e := range disk.Entries {
		if now.Before(e.Expires) {
			repoCache[key] = repoCacheEntry{names: e.Names, expires: e.Expires}
		}
	}
}

func (c *client) saveRepoCache() error {
	path := cacheFile("repos.json")
	if path == "" {
		return nil
	}
	now := time.Now()
	repoCacheMu.Lock()
	entries := make(map[string]diskRepoEntry, len(repoCache))
	for key, e := range repoCache {
		if now.Before(e.expires) {
			entries[key] = diskRepoEntry{Names: e.names, Expires: e.expires}
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
