package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		{"org and days", "org = org-a\ndays = 30\n", Config{Org: "org-a", Orgs: []string{"org-a"}, Days: 30, DaysSet: true}},
		{"comments and blanks", "# comment\n\n  # indented\norg = org-a\n", Config{Org: "org-a", Orgs: []string{"org-a"}}},
		{"days zero is set", "days = 0\n", Config{Days: 0, DaysSet: true}},
		{"malformed days ignored", "org = org-a\ndays = soon\n", Config{Org: "org-a", Orgs: []string{"org-a"}}},
		{"unknown key ignored", "colour = red\norg = org-a\n", Config{Org: "org-a", Orgs: []string{"org-a"}}},
		{"line without separator ignored", "org\norg = org-a\n", Config{Org: "org-a", Orgs: []string{"org-a"}}},
		{"whitespace trimmed", "   org   =   org-a   \n", Config{Org: "org-a", Orgs: []string{"org-a"}}},
		{"value with equals keeps remainder", "org = a=b\n", Config{Org: "a=b", Orgs: []string{"a=b"}}},
		{"last wins", "org = one\norg = two\n", Config{Org: "two", Orgs: []string{"two"}}},
		{"orgs list", "orgs = a, b , c\n", Config{Orgs: []string{"a", "b", "c"}}},
		{"orgs drops empties and duplicates", "orgs = a, , b, a, ,b\n", Config{Orgs: []string{"a", "b"}}},
		{"orgs only whitespace", "orgs =   \n", Config{}},
		{"org moves to the front of orgs", "org = c\norgs = a, b, c\n", Config{Org: "c", Orgs: []string{"c", "a", "b"}}},
		{"org absent from orgs is prepended", "org = z\norgs = a, b\n", Config{Org: "z", Orgs: []string{"z", "a", "b"}}},
		{"org with no orgs line migrates to one entry", "org = x\ndays = 4\n", Config{Org: "x", Orgs: []string{"x"}, Days: 4, DaysSet: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parse(c.text)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parse(%q) = %+v, want %+v", c.text, got, c.want)
			}
		})
	}
}

func TestLoadMissingFileIsZero(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := Load(); !reflect.DeepEqual(got, Config{}) {
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
	want := Config{Org: "org-a", Orgs: []string{"org-a"}, Days: 3, DaysSet: true}
	if got := Load(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadUnreadableFileIsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "gh-runners", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Load(); !reflect.DeepEqual(got, Config{}) {
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
	want := "org = org-a\norgs = org-a\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q", string(data), want)
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
			"# scan this org\norg = org-a\n\ndays = 30\ncolour = red\norgs = org-a, old\n",
		},
		{
			"rewrites both keys in place",
			"# header\norg = old\norgs = old, other\ndays = 30\n",
			"org-a",
			"# header\norg = org-a\norgs = org-a, old, other\ndays = 30\n",
		},
		{
			"moves an existing org to the front without duplicating",
			"org = a\norgs = a, b, c\n",
			"b",
			"org = b\norgs = b, a, c\n",
		},
		{
			"appends when no org line exists",
			"# header\ndays = 30\n",
			"org-a",
			"# header\ndays = 30\norg = org-a\norgs = org-a\n",
		},
		{
			"appends newline before appending org",
			"days = 30",
			"org-a",
			"days = 30\norg = org-a\norgs = org-a\n",
		},
		{
			"replaces weird spacing without duplicating",
			"  org  =  old  \ndays = 2\n",
			"org-a",
			"org = org-a\ndays = 2\norgs = org-a, old\n",
		},
		{
			"replaces only the first org line",
			"org = one\ndays = 2\norg = two\n",
			"org-a",
			"org = org-a\ndays = 2\norg = two\norgs = org-a, two\n",
		},
		{
			"ignores a commented org line",
			"# org = commented\ndays = 2\n",
			"org-a",
			"# org = commented\ndays = 2\norg = org-a\norgs = org-a\n",
		},
		{
			"whitespace-only file becomes the org and orgs lines",
			"\n\n",
			"org-a",
			"org = org-a\norgs = org-a\n",
		},
		{
			"migrates a file that has org but no orgs",
			"org = x\ndays = 9\n",
			"x",
			"org = x\ndays = 9\norgs = x\n",
		},
		{
			"caps the history at ten entries dropping the oldest",
			"orgs = o1, o2, o3, o4, o5, o6, o7, o8, o9, o10\n",
			"new",
			"orgs = new, o1, o2, o3, o4, o5, o6, o7, o8, o9\norg = new\n",
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

func TestSetOrgRoundTripLoadsHistory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, org := range []string{"a", "b", "c", "b"} {
		if err := SetOrg(org); err != nil {
			t.Fatal(err)
		}
	}
	got := Load()
	if got.Org != "b" {
		t.Errorf("Org = %q, want b", got.Org)
	}
	if want := []string{"b", "c", "a"}; !reflect.DeepEqual(got.Orgs, want) {
		t.Errorf("Orgs = %v, want %v", got.Orgs, want)
	}
}

func TestSetOrgCapsHistoryAtTen(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for i := 1; i <= 12; i++ {
		if err := SetOrg("org" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	got := Load().Orgs
	if len(got) != maxOrgs {
		t.Fatalf("len(Orgs) = %d, want %d (%v)", len(got), maxOrgs, got)
	}
	want := []string{"org12", "org11", "org10", "org9", "org8", "org7", "org6", "org5", "org4", "org3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Orgs = %v, want %v", got, want)
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
	want := Config{Org: "x", Orgs: []string{"x"}, Days: 30, DaysSet: true}
	if got := Load(); !reflect.DeepEqual(got, want) {
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

func TestSetScanRoundTripKeepsOtherKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "gh-runners", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# mine\norg = org-a\ndays = 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetScan([]string{"org-a/*", "org-b/api"}); err != nil {
		t.Fatal(err)
	}
	cfg := Load()
	if !reflect.DeepEqual(cfg.Scan, []string{"org-a/*", "org-b/api"}) || cfg.Org != "org-a" || cfg.Days != 30 {
		t.Fatalf("cfg = %+v", cfg)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# mine\norg = org-a\ndays = 30\nscan = org-a/*, org-b/api\n") {
		t.Fatalf("file = %q", data)
	}
	if err := SetScan([]string{"org-c/*"}); err != nil {
		t.Fatal(err)
	}
	if cfg := Load(); !reflect.DeepEqual(cfg.Scan, []string{"org-c/*"}) {
		t.Fatalf("second save: %+v", cfg.Scan)
	}
}
