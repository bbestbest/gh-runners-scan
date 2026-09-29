package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Org     string
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
		case "days":
			n, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			cfg.Days = n
			cfg.DaysSet = true
		}
	}
	return cfg
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
	return writeFileAtomic(path, replaceOrg(string(existing), org))
}

func replaceOrg(text, org string) string {
	line := "org = " + org + "\n"
	if strings.TrimSpace(text) == "" {
		return line
	}
	lines := strings.SplitAfter(text, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "org" {
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
