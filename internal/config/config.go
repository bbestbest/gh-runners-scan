package config

import (
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
