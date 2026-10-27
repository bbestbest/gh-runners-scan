package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTuiBinaryFollowsSymlinkToSibling(t *testing.T) {
	bin := t.TempDir()
	links := t.TempDir()
	cli := filepath.Join(bin, "gh-runners")
	tui := filepath.Join(bin, "gh-runner")
	for _, p := range []string{cli, tui} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(links, "gh-runners")
	if err := os.Symlink(cli, link); err != nil {
		t.Fatal(err)
	}
	got, ok := tuiBinary(link)
	want, _ := filepath.EvalSymlinks(tui)
	if !ok || got != want {
		t.Fatalf("tuiBinary(%q) = %q, %v; want %q, true", link, got, ok, want)
	}
}

func TestTuiBinaryMissingOrNotExecutable(t *testing.T) {
	bin := t.TempDir()
	cli := filepath.Join(bin, "gh-runners")
	if err := os.WriteFile(cli, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := tuiBinary(cli); ok {
		t.Fatal("want false when gh-runner is missing")
	}
	if err := os.WriteFile(filepath.Join(bin, "gh-runner"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := tuiBinary(cli); ok {
		t.Fatal("want false when gh-runner is not executable")
	}
	if err := os.Remove(filepath.Join(bin, "gh-runner")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "gh-runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := tuiBinary(cli); ok {
		t.Fatal("want false when gh-runner is a directory")
	}
}
