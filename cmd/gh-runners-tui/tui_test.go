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
	m := newModel(options{org: "org-c", interval: 30, repo: "repo-a"})
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

func typeRunes(m model, s string) model {
	for _, r := range s {
		m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func TestOrgOverlayStartupState(t *testing.T) {
	tests := []struct {
		name string
		org  string
		want bool
	}{
		{"empty org opens overlay", "", true},
		{"supplied org skips overlay", "org-a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(options{org: tt.org, interval: 30})
			if m.orgActive != tt.want {
				t.Errorf("orgActive = %v, want %v", m.orgActive, tt.want)
			}
			if m.scanning == (tt.org == "") {
				t.Errorf("scanning = %v for org %q", m.scanning, tt.org)
			}
		})
	}
}

func TestOrgOverlayEnterStartsScan(t *testing.T) {
	m := newModel(options{interval: 30})
	m = typeRunes(m, "org-a")

	next, cmd := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "org-a" {
		t.Fatalf("org = %q, want org-a", m.opts.org)
	}
	if m.orgActive {
		t.Error("overlay still active after enter")
	}
	if cmd == nil {
		t.Error("enter with valid org did not trigger a scan")
	}
	if !m.scanning {
		t.Error("scanning = false after accepting an org")
	}
}

func TestOrgOverlayRejectsBlank(t *testing.T) {
	for _, in := range []string{"", "   "} {
		m := newModel(options{interval: 30})
		m = typeRunes(m, in)

		next, cmd := m.Update(key("enter"))
		m = next.(model)

		if !m.orgActive {
			t.Errorf("overlay closed on blank input %q", in)
		}
		if m.opts.org != "" {
			t.Errorf("org = %q after blank input %q", m.opts.org, in)
		}
		if cmd != nil {
			t.Errorf("blank input %q returned a command", in)
		}
		if m.scanning {
			t.Errorf("scanning started on blank input %q", in)
		}
	}
}

func TestOrgOverlayEscape(t *testing.T) {
	m := newModel(options{interval: 30})
	_, cmd := m.Update(key("esc"))
	if cmd == nil {
		t.Fatal("esc with no org set did not quit")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Errorf("esc with no org sent %T, want quit", msg)
	}

	withOrg := newTestModel(t)
	withOrg = send(withOrg, key("O"))
	if !withOrg.orgActive {
		t.Fatal("O did not open the overlay")
	}
	if got := withOrg.orgInput.Value(); got != "org-c" {
		t.Errorf("overlay value = %q, want org-c", got)
	}

	next, cmd := withOrg.Update(key("esc"))
	withOrg = next.(model)
	if withOrg.orgActive {
		t.Error("overlay still active after esc with an org set")
	}
	if withOrg.opts.org != "org-c" {
		t.Errorf("org = %q after esc, want org-c", withOrg.opts.org)
	}
	if cmd != nil {
		t.Error("esc with an org set returned a command")
	}
}

func TestOrgOverlayChangeWhileRunning(t *testing.T) {
	m := newTestModel(t)
	m = send(m, key("O"))
	for range "org-c" {
		m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeRunes(m, "org-b")

	next, cmd := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "org-b" {
		t.Fatalf("org = %q, want org-b", m.opts.org)
	}
	if cmd == nil {
		t.Error("changing org did not trigger a rescan")
	}
	if len(m.jobs) != 0 {
		t.Errorf("jobs = %d after org change, want 0", len(m.jobs))
	}
	if !strings.Contains(m.headerView(), "org-b") {
		t.Error("header does not show the new org")
	}
}

func TestOrgOverlayViewReplacesBody(t *testing.T) {
	m := newModel(options{interval: 30})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	view := next.(model).View()

	if !strings.Contains(view, "org:") {
		t.Error("overlay view missing the org prompt")
	}
	if strings.Contains(view, "Running (") {
		t.Error("panels rendered while the overlay is active")
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
