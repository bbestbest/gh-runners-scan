package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		text string
		want Config
	}{
		{"empty", "", Config{}},
		{"org and days", "org = org-a\ndays = 30\n", Config{Org: "org-a", Days: 30, DaysSet: true}},
		{"comments and blanks", "# comment\n\n  # indented\norg = org-a\n", Config{Org: "org-a"}},
		{"days zero is set", "days = 0\n", Config{Days: 0, DaysSet: true}},
		{"malformed days ignored", "org = org-a\ndays = soon\n", Config{Org: "org-a"}},
		{"unknown key ignored", "colour = red\norg = org-a\n", Config{Org: "org-a"}},
		{"line without separator ignored", "org\norg = org-a\n", Config{Org: "org-a"}},
		{"whitespace trimmed", "   org   =   org-a   \n", Config{Org: "org-a"}},
		{"value with equals keeps remainder", "org = a=b\n", Config{Org: "a=b"}},
		{"last wins", "org = one\norg = two\n", Config{Org: "two"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parse(c.text)
			if got != c.want {
				t.Fatalf("parse(%q) = %+v, want %+v", c.text, got, c.want)
			}
		})
	}
}

func TestLoadMissingFileIsZero(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := Load(); got != (Config{}) {
		t.Fatalf("Load() = %+v, want zero", got)
	}
}

func TestLoadReadsXDGPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "gh-runners"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh-runners", "config"), []byte("org = org-a\ndays = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := Config{Org: "org-a", Days: 3, DaysSet: true}
	if got := Load(); got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadUnreadableFileIsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "gh-runners", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got != (Config{}) {
		t.Fatalf("Load() = %+v, want zero", got)
	}
}

func TestSetOrgCreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "nested", "deeper"))
	if err := SetOrg("org-a"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "org = org-a\n" {
		t.Errorf("file = %q, want %q", string(data), "org = org-a\n")
	}
	if got := Load(); got.Org != "org-a" {
		t.Errorf("Load().Org = %q, want org-a", got.Org)
	}
}

func TestSetOrgRewrite(t *testing.T) {
	cases := []struct {
		name   string
		before string
		org    string
		want   string
	}{
		{
			"preserves days comments and unknown keys",
			"# scan this org\norg = old\n\ndays = 30\ncolour = red\n",
			"org-a",
			"# scan this org\norg = org-a\n\ndays = 30\ncolour = red\n",
		},
		{
			"appends when no org line exists",
			"# header\ndays = 30\n",
			"org-a",
			"# header\ndays = 30\norg = org-a\n",
		},
		{
			"appends newline before appending org",
			"days = 30",
			"org-a",
			"days = 30\norg = org-a\n",
		},
		{
			"replaces weird spacing without duplicating",
			"  org  =  old  \ndays = 2\n",
			"org-a",
			"org = org-a\ndays = 2\n",
		},
		{
			"replaces only the first org line",
			"org = one\ndays = 2\norg = two\n",
			"org-a",
			"org = org-a\ndays = 2\norg = two\n",
		},
		{
			"ignores a commented org line",
			"# org = commented\ndays = 2\n",
			"org-a",
			"# org = commented\ndays = 2\norg = org-a\n",
		},
		{
			"whitespace-only file becomes just the org line",
			"\n\n",
			"org-a",
			"org = org-a\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			if err := os.MkdirAll(filepath.Join(dir, "gh-runners"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "gh-runners", "config")
			if err := os.WriteFile(path, []byte(c.before), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := SetOrg(c.org); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.want {
				t.Errorf("file = %q, want %q", string(data), c.want)
			}
		})
	}
}

func TestSetOrgTwiceDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := SetOrg("one"); err != nil {
		t.Fatal(err)
	}
	if err := SetOrg("two"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		key, _, ok := strings.Cut(l, "=")
		if ok && strings.TrimSpace(key) == "org" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("org lines = %d, want 1 (file = %q)", n, string(data))
	}
	if got := Load(); got.Org != "two" {
		t.Errorf("Load().Org = %q, want two", got.Org)
	}
}

func TestSetOrgRoundTripKeepsDays(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "gh-runners"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh-runners", "config"), []byte("days = 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetOrg("x"); err != nil {
		t.Fatal(err)
	}
	want := Config{Org: "x", Days: 30, DaysSet: true}
	if got := Load(); got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestSetOrgLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := SetOrg("org-a"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "gh-runners"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config" {
		t.Errorf("dir entries = %v, want just config", entries)
	}
}

func TestSetOrgFileMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := SetOrg("org-a"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestSetOrgErrorsWhenPathUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	blocker := filepath.Join(dir, "gh-runners")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetOrg("org-a"); err == nil {
		t.Fatal("SetOrg() = nil, want an error when the parent path is a file")
	}
}
