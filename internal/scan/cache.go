package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

const cacheVersion = 2

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

func cachePath() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "gh-runners", "etags.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "gh-runners", "etags.json")
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
	return c.saveCache()
}
