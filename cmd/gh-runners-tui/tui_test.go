package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gh-runners/internal/scan"
)

func testJobs() []scan.Job {
	now := time.Now().UTC()
	return []scan.Job{
		{Status: "in_progress", Repo: "repo-b", Workflow: "build", Branch: "main", Name: "jobB", Runner: "runner-2", StartedAt: now.Add(-5 * time.Minute), URL: "https://example.com/b"},
		{Status: "in_progress", Repo: "repo-a", Workflow: "test", Branch: "dev", Name: "jobA", Runner: "runner-1", StartedAt: now.Add(-90 * time.Second), URL: "https://example.com/a"},
		{Status: "queued", Repo: "repo-c", Workflow: "deploy", Branch: "release", Name: "jobC", Labels: "self-hosted,linux", CreatedAt: now.Add(-45 * time.Minute), URL: "https://example.com/c"},
	}
}

func newTestModel(t *testing.T) model {
	t.Helper()
	m := newModel(options{org: "test-org", interval: 30, repo: "repo-a"})
	m.jobs = testJobs()
	m.refresh()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return next.(model)
}

func send(m model, key tea.KeyMsg) model {
	next, _ := m.Update(key)
	return next.(model)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestRowCountsAndOrder(t *testing.T) {
	m := newTestModel(t)

	if got := len(m.runTable.Rows()); got != 2 {
		t.Fatalf("running rows = %d, want 2", got)
	}
	if got := len(m.qTable.Rows()); got != 1 {
		t.Fatalf("queued rows = %d, want 1", got)
	}

	if m.running[0].Runner != "runner-1" || m.running[1].Runner != "runner-2" {
		t.Errorf("running not sorted by runner: %s, %s", m.running[0].Runner, m.running[1].Runner)
	}
	if m.queued[0].Name != "jobC" {
		t.Errorf("queued[0] = %s, want jobC", m.queued[0].Name)
	}
}

func TestTabMovesFocus(t *testing.T) {
	m := newTestModel(t)
	if m.focus != 0 {
		t.Fatalf("initial focus = %d, want 0", m.focus)
	}
	m = send(m, key("tab"))
	if m.focus != 1 {
		t.Errorf("focus after tab = %d, want 1", m.focus)
	}
	m = send(m, key("tab"))
	if m.focus != 0 {
		t.Errorf("focus after second tab = %d, want 0", m.focus)
	}
}

func TestFilterAndRestore(t *testing.T) {
	m := newTestModel(t)

	m = send(m, key("/"))
	if !m.filterActive {
		t.Fatal("filter not active after /")
	}
	for _, r := range "jobA" {
		m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m = send(m, key("enter"))

	if m.filter != "jobA" {
		t.Fatalf("filter = %q, want jobA", m.filter)
	}
	if got := len(m.runTable.Rows()); got != 1 {
		t.Errorf("filtered running rows = %d, want 1", got)
	}
	if got := len(m.qTable.Rows()); got != 0 {
		t.Errorf("filtered queued rows = %d, want 0", got)
	}

	m = send(m, key("esc"))
	if m.filter != "" {
		t.Fatalf("filter after esc = %q, want empty", m.filter)
	}
	if got := len(m.runTable.Rows()); got != 2 {
		t.Errorf("restored running rows = %d, want 2", got)
	}
	if got := len(m.qTable.Rows()); got != 1 {
		t.Errorf("restored queued rows = %d, want 1", got)
	}
}

func TestViewContents(t *testing.T) {
	m := newTestModel(t)
	view := m.View()
	for _, want := range []string{"Running (2)", "Queued (1)", "<q> quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q", want)
		}
	}
}
