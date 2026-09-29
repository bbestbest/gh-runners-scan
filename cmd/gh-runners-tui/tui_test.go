package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gh-runners/internal/config"
	"gh-runners/internal/scan"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-runners-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
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

func TestOrgOverlayPersistsOrg(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newModel(options{interval: 30})
	m = typeRunes(m, "org-a")
	next, _ := m.Update(key("enter"))
	m = next.(model)

	if m.saveErr != nil {
		t.Fatalf("saveErr = %v, want nil", m.saveErr)
	}
	if got := config.Load().Org; got != "org-a" {
		t.Errorf("persisted org = %q, want org-a", got)
	}
}

func TestOrgOverlaySwitchPersistsOrg(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.SetOrg("org-c"); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m = send(m, key("O"))
	for range "org-c" {
		m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeRunes(m, "org-b")
	next, _ := m.Update(key("enter"))
	m = next.(model)

	if got := config.Load().Org; got != "org-b" {
		t.Errorf("persisted org = %q, want org-b", got)
	}
}

func TestOrgOverlayReconfirmPersistsOrg(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestModel(t)
	m = send(m, key("O"))
	next, _ := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "org-c" {
		t.Fatalf("org = %q, want org-c", m.opts.org)
	}
	if got := config.Load().Org; got != "org-c" {
		t.Errorf("persisted org = %q, want org-c", got)
	}
}

func TestOrgOverlaySaveFailureKeepsSessionUsable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "gh-runners"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newModel(options{interval: 30})
	m = typeRunes(m, "org-a")
	next, cmd := m.Update(key("enter"))
	m = next.(model)

	if m.saveErr == nil {
		t.Fatal("saveErr = nil, want an error")
	}
	if m.opts.org != "org-a" {
		t.Errorf("org = %q, want org-a applied in-session despite the save failure", m.opts.org)
	}
	if m.orgActive {
		t.Error("overlay still active after a failed save")
	}
	if !m.scanning {
		t.Error("scanning = false after a failed save")
	}
	if cmd == nil {
		t.Error("a failed save suppressed the scan")
	}
	if !strings.Contains(m.statusView(), "org not saved") {
		t.Error("status line does not surface the save failure")
	}
}

func TestOrgOverlayBlankDoesNotPersist(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newModel(options{interval: 30})
	m = typeRunes(m, "   ")
	next, _ := m.Update(key("enter"))
	m = next.(model)

	if got := config.Load().Org; got != "" {
		t.Errorf("persisted org = %q, want empty", got)
	}
	if !m.orgActive {
		t.Error("overlay closed on blank input")
	}
}

func TestOrgOverlayEscDoesNotPersist(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestModel(t)
	m = send(m, key("O"))
	for range "org-c" {
		m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeRunes(m, "org-b")
	next, _ := m.Update(key("esc"))
	m = next.(model)

	if got := config.Load().Org; got != "" {
		t.Errorf("persisted org = %q, want empty after esc", got)
	}
	if m.opts.org != "org-c" {
		t.Errorf("org = %q, want org-c", m.opts.org)
	}
}

func orgModel(t *testing.T, org string, history ...string) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for i := len(history) - 1; i >= 0; i-- {
		if err := config.SetOrg(history[i]); err != nil {
			t.Fatal(err)
		}
	}
	if org != "" {
		if err := config.SetOrg(org); err != nil {
			t.Fatal(err)
		}
	}
	m := newModel(options{org: org, interval: 30})
	if org != "" {
		m = send(m, key("O"))
	}
	return m
}

func TestOrgOverlayCursorWalksHistory(t *testing.T) {
	m := orgModel(t, "one", "two", "three")

	if want := []string{"one", "two", "three"}; !reflect.DeepEqual(m.orgs, want) {
		t.Fatalf("orgs = %v, want %v", m.orgs, want)
	}
	if m.orgCursor != -1 {
		t.Fatalf("initial cursor = %d, want -1", m.orgCursor)
	}

	steps := []struct {
		key    string
		cursor int
		value  string
	}{
		{"down", 0, "one"},
		{"down", 1, "two"},
		{"down", 2, "three"},
		{"down", 2, "three"},
		{"up", 1, "two"},
		{"up", 0, "one"},
		{"up", -1, "one"},
	}
	for _, s := range steps {
		m = send(m, key(s.key))
		if m.orgCursor != s.cursor {
			t.Errorf("after %s cursor = %d, want %d", s.key, m.orgCursor, s.cursor)
		}
		if got := m.orgInput.Value(); got != s.value {
			t.Errorf("after %s value = %q, want %q", s.key, got, s.value)
		}
	}
}

func TestOrgOverlayEnterAcceptsHighlightedRow(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	m = send(m, key("down"))
	m = send(m, key("down"))

	next, cmd := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "two" {
		t.Fatalf("org = %q, want two", m.opts.org)
	}
	if cmd == nil {
		t.Error("picking a different org did not trigger a rescan")
	}
	if got := config.Load().Org; got != "two" {
		t.Errorf("persisted org = %q, want two", got)
	}
	if m.orgCursor != -1 {
		t.Errorf("cursor = %d after accepting, want -1", m.orgCursor)
	}
	if want := []string{"two", "one", "three"}; !reflect.DeepEqual(m.orgs, want) {
		t.Errorf("orgs = %v, want %v", m.orgs, want)
	}
}

func TestOrgOverlayTypingBeatsHighlightedRow(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	m = send(m, key("down"))
	m = send(m, key("down"))
	if m.orgCursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.orgCursor)
	}

	for range "two" {
		m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeRunes(m, "typed")
	if m.orgCursor != -1 {
		t.Fatalf("cursor = %d after typing, want -1", m.orgCursor)
	}

	next, _ := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "typed" {
		t.Errorf("org = %q, want typed", m.opts.org)
	}
}

func TestOrgOverlayArrowsDoNotBreakTyping(t *testing.T) {
	m := orgModel(t, "one", "two")
	for range "one" {
		m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeRunes(m, "jk")

	if got := m.orgInput.Value(); got != "jk" {
		t.Errorf("value = %q, want jk typed literally", got)
	}
	if m.orgCursor != -1 {
		t.Errorf("cursor = %d, want -1 after typing", m.orgCursor)
	}
}

func TestOrgOverlayEmptyHistoryRendersCleanly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newModel(options{interval: 30})
	if len(m.orgs) != 0 {
		t.Fatalf("orgs = %v, want empty", m.orgs)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)

	if got := m.orgListView(); got != "" {
		t.Errorf("org list = %q, want empty", got)
	}
	overlay := m.orgOverlayView()
	if !strings.Contains(overlay, "org:") {
		t.Error("overlay missing the org prompt")
	}
	if strings.Contains(overlay, "> ") {
		t.Error("empty history rendered a cursor row")
	}
	if strings.Contains(overlay, "(saved)") {
		t.Error("empty history rendered a saved marker")
	}

	m = send(m, key("down"))
	if m.orgCursor != -1 {
		t.Errorf("cursor = %d on empty history, want -1", m.orgCursor)
	}
}

func TestOrgOverlayListMarksSavedAndCursor(t *testing.T) {
	m := orgModel(t, "one", "two")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)

	list := m.orgListView()
	if !strings.Contains(list, "(saved)") {
		t.Errorf("list does not mark the saved org: %q", list)
	}
	if strings.Contains(list, ">") {
		t.Errorf("list shows a cursor before navigating: %q", list)
	}

	m = send(m, key("down"))
	if got := m.orgListView(); !strings.Contains(got, "> one") {
		t.Errorf("list does not mark the cursor row: %q", got)
	}
}

func TestOrgOverlayListCapsRenderedRows(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for i := 12; i >= 1; i-- {
		if err := config.SetOrg("org" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	m := newModel(options{org: "org1", interval: 30})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)
	m = send(m, key("O"))

	if len(m.orgs) != 10 {
		t.Fatalf("orgs = %d, want 10", len(m.orgs))
	}
	rows := strings.Count(m.orgListView(), "\n") + 1
	if rows > maxOrgRows {
		t.Errorf("rendered rows = %d, want at most %d", rows, maxOrgRows)
	}

	for i := 0; i < 10; i++ {
		m = send(m, key("down"))
	}
	if !strings.Contains(m.orgListView(), "> "+m.orgs[len(m.orgs)-1]) {
		t.Errorf("last org not visible after scrolling: %q", m.orgListView())
	}
}

func TestOrgOverlayShortTerminalStillRendersOneRow(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m = next.(model)

	rows := strings.Count(m.orgListView(), "\n") + 1
	if rows < 1 {
		t.Errorf("rendered rows = %d, want at least 1", rows)
	}
	if rows > maxOrgRows {
		t.Errorf("rendered rows = %d, want at most %d", rows, maxOrgRows)
	}
}
