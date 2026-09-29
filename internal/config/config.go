package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxOrgs = 10

type Config struct {
	Org     string
	Orgs    []string
	Days    int
	DaysSet bool
}

func Path() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gh-runners", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh-runners", "config")
}

func Load() Config {
	path := Path()
	if path == "" {
		return Config{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}
	}
	return parse(string(data))
}

func parse(text string) Config {
	var cfg Config
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "org":
			cfg.Org = value
		case "orgs":
			cfg.Orgs = splitOrgs(value)
		case "days":
			n, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			cfg.Days = n
			cfg.DaysSet = true
		}
	}
	if cfg.Org != "" {
		cfg.Orgs = mergeOrgs(cfg.Orgs, cfg.Org)
	}
	return cfg
}

func splitOrgs(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func mergeOrgs(orgs []string, org string) []string {
	out := []string{org}
	for _, o := range orgs {
		if o != org {
			out = append(out, o)
		}
	}
	if len(out) > maxOrgs {
		out = out[:maxOrgs]
	}
	return out
}

func SetOrg(org string) error {
	path := Path()
	if path == "" {
		return errors.New("config: no path for the config file")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(existing)
	orgs := mergeOrgs(parse(text).Orgs, org)
	text = replaceKey(text, "org", org)
	text = replaceKey(text, "orgs", strings.Join(orgs, ", "))
	return writeFileAtomic(path, text)
}

func replaceKey(text, key, value string) string {
	line := key + " = " + value + "\n"
	if strings.TrimSpace(text) == "" {
		return line
	}
	lines := strings.SplitAfter(text, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		lines[i] = line
		return strings.Join(lines, "")
	}
	out := strings.Join(lines, "")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + line
}

func writeFileAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
