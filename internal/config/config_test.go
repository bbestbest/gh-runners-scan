package config

import (
	"os"
	"path/filepath"
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
