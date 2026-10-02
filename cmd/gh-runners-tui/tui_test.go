package main

import (
	"context"
	"errors"
	"math/rand"
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
	if got := withOrg.orgInput.Value(); got != "" {
		t.Errorf("overlay value = %q, want an empty field", got)
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
	m = typeRunes(m, "org-c")
	next, _ := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "org-c" {
		t.Fatalf("org = %q, want org-c", m.opts.org)
	}
	if got := config.Load().Org; got != "org-c" {
		t.Errorf("persisted org = %q, want org-c", got)
	}
}

func TestOrgOverlayEmptyEnterKeepsOrgWithoutSaving(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestModel(t)
	m = send(m, key("O"))
	next, cmd := m.Update(key("enter"))
	m = next.(model)

	if m.opts.org != "org-c" {
		t.Errorf("org = %q, want org-c kept in session", m.opts.org)
	}
	if m.orgActive {
		t.Error("overlay still active after an empty enter")
	}
	if cmd != nil {
		t.Error("an empty enter triggered a rescan")
	}
	if got := config.Load().Org; got != "" {
		t.Errorf("persisted org = %q, want empty: an empty enter is a cancel", got)
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
		{"up", -1, ""},
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

func TestOrgOverlayDeleteRemovesHighlightedRow(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	m = send(m, key("down"))
	m = send(m, key("down"))
	if m.orgCursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.orgCursor)
	}

	next, cmd := m.Update(key("x"))
	m = next.(model)

	if cmd != nil {
		t.Error("deleting an org returned a command")
	}
	if want := []string{"one", "three"}; !reflect.DeepEqual(m.orgs, want) {
		t.Fatalf("orgs = %v, want %v", m.orgs, want)
	}
	if m.saveErr != nil {
		t.Errorf("saveErr = %v, want nil", m.saveErr)
	}
	if got := config.Load().Orgs; !reflect.DeepEqual(got, []string{"one", "three"}) {
		t.Errorf("persisted orgs = %v, want [one three]", got)
	}
}

func TestOrgOverlayDeleteWithNoHighlightTypesTheCharacter(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	if m.orgCursor != -1 {
		t.Fatalf("cursor = %d, want -1", m.orgCursor)
	}
	before := append([]string(nil), m.orgs...)

	m = send(m, key("x"))

	if got := m.orgInput.Value(); !strings.Contains(got, "x") {
		t.Errorf("input = %q, want it to contain a literal x", got)
	}
	if !reflect.DeepEqual(m.orgs, before) {
		t.Errorf("orgs = %v, want unchanged %v", m.orgs, before)
	}
	if m.orgCursor != -1 {
		t.Errorf("cursor = %d, want -1", m.orgCursor)
	}
	if got := config.Load().Orgs; !reflect.DeepEqual(got, before) {
		t.Errorf("persisted orgs = %v, want unchanged %v", got, before)
	}
}

func TestOrgOverlayDeleteSavedOrgKeepsSession(t *testing.T) {
	m := orgModel(t, "one", "two")
	m = send(m, key("down"))
	if m.orgs[m.orgCursor] != "one" {
		t.Fatalf("cursor row = %q, want one", m.orgs[m.orgCursor])
	}

	next, cmd := m.Update(key("x"))
	m = next.(model)

	if cmd != nil {
		t.Error("deleting the saved org triggered a command")
	}
	if m.opts.org != "one" {
		t.Errorf("opts.org = %q, want one unchanged", m.opts.org)
	}
	if want := []string{"two"}; !reflect.DeepEqual(m.orgs, want) {
		t.Errorf("orgs = %v, want %v", m.orgs, want)
	}
	if got := config.Load().Org; got != "one" {
		t.Errorf("persisted org = %q, want one unchanged", got)
	}
	if !m.orgActive {
		t.Error("overlay closed after a delete")
	}
}

func TestOrgOverlayDeleteClampsCursor(t *testing.T) {
	m := orgModel(t, "one", "two", "three")
	for i := 0; i < 3; i++ {
		m = send(m, key("down"))
	}
	if m.orgCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.orgCursor)
	}

	m = send(m, key("x"))

	if m.orgCursor != 1 {
		t.Errorf("cursor = %d, want 1 clamped into range", m.orgCursor)
	}
	if m.orgCursor >= len(m.orgs) {
		t.Fatalf("cursor %d out of range for %v", m.orgCursor, m.orgs)
	}
	if got := m.orgInput.Value(); got != m.orgs[m.orgCursor] {
		t.Errorf("input = %q, want the new cursor row %q", got, m.orgs[m.orgCursor])
	}
	if got := m.orgListView(); strings.Contains(got, "three") {
		t.Errorf("list still shows the deleted org: %q", got)
	}
}

func TestOrgOverlayDeleteToEmptyRendersCleanly(t *testing.T) {
	m := orgModel(t, "one", "two")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)

	for len(m.orgs) > 0 {
		m = send(m, key("down"))
		m = send(m, key("x"))
	}

	if m.orgCursor != -1 {
		t.Errorf("cursor = %d on an empty list, want -1", m.orgCursor)
	}
	if got := m.orgListView(); got != "" {
		t.Errorf("list = %q, want empty", got)
	}
	if got := m.orgInput.Value(); got != "" {
		t.Errorf("input = %q, want an empty field", got)
	}
	overlay := m.orgOverlayView()
	if !strings.Contains(overlay, "org:") {
		t.Error("overlay missing the org prompt after emptying the list")
	}
	if strings.Contains(overlay, "> ") {
		t.Error("empty list rendered a cursor row")
	}
	if m.opts.org != "one" {
		t.Errorf("opts.org = %q, want one unchanged", m.opts.org)
	}
}

func TestOrgOverlayDeleteHintIsShown(t *testing.T) {
	m := orgModel(t, "one", "two")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)
	if !strings.Contains(m.orgOverlayView(), "x delete") {
		t.Error("overlay hint does not mention x delete")
	}
}

func TestOrgOverlayOpensWithAnEmptyField(t *testing.T) {
	t.Run("at startup with no org", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		m := newModel(options{interval: 30})
		if got := m.orgInput.Value(); got != "" {
			t.Errorf("value = %q, want empty", got)
		}
	})

	t.Run("at startup with a saved org", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		m := newModel(options{org: "org-c", interval: 30})
		if got := m.orgInput.Value(); got != "" {
			t.Errorf("value = %q, want empty even though org is set", got)
		}
	})

	t.Run("reopened with O", func(t *testing.T) {
		m := orgModel(t, "one", "two")
		if got := m.orgInput.Value(); got != "" {
			t.Errorf("value = %q, want empty after O", got)
		}
		if !m.orgActive {
			t.Error("O did not open the overlay")
		}
	})

	t.Run("saved org still listed", func(t *testing.T) {
		m := orgModel(t, "one", "two")
		next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
		m = next.(model)
		list := m.orgListView()
		if !strings.Contains(list, "one") {
			t.Errorf("list does not show the saved org: %q", list)
		}
		if !strings.Contains(list, "(saved)") {
			t.Errorf("list does not mark the saved org: %q", list)
		}
	})

	t.Run("placeholder visible while empty", func(t *testing.T) {
		m := orgModel(t, "one", "two")
		next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
		m = next.(model)
		if !strings.Contains(m.orgOverlayView(), "organisation") {
			t.Error("overlay does not show the organisation placeholder")
		}
	})

	t.Run("reopening after a pick is empty again", func(t *testing.T) {
		m := orgModel(t, "one", "two")
		m = send(m, key("down"))
		m = send(m, key("down"))
		next, _ := m.Update(key("enter"))
		m = next.(model)
		if m.opts.org != "two" {
			t.Fatalf("org = %q, want two", m.opts.org)
		}
		m = send(m, key("O"))
		if got := m.orgInput.Value(); got != "" {
			t.Errorf("value = %q, want empty on reopen", got)
		}
	})
}

func TestOrgOverlayBlankEnterUnchangedWithEmptyField(t *testing.T) {
	t.Run("no org set rejects", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		m := newModel(options{interval: 30})
		next, cmd := m.Update(key("enter"))
		m = next.(model)
		if !m.orgActive {
			t.Error("overlay closed on an empty field with no org set")
		}
		if cmd != nil {
			t.Error("empty field with no org set returned a command")
		}
		if m.opts.org != "" {
			t.Errorf("org = %q, want empty", m.opts.org)
		}
	})

	t.Run("org set keeps it and closes", func(t *testing.T) {
		m := orgModel(t, "one", "two")
		next, cmd := m.Update(key("enter"))
		m = next.(model)
		if m.orgActive {
			t.Error("overlay still active after enter on an empty field")
		}
		if m.opts.org != "one" {
			t.Errorf("org = %q, want one kept", m.opts.org)
		}
		if cmd != nil {
			t.Error("empty-field enter triggered a rescan")
		}
	})
}

func TestOrgOverlayArrowPastTopClearsField(t *testing.T) {
	m := orgModel(t, "one", "two")
	m = send(m, key("down"))
	if got := m.orgInput.Value(); got != "one" {
		t.Fatalf("value = %q, want one copied in", got)
	}
	m = send(m, key("up"))
	if m.orgCursor != -1 {
		t.Fatalf("cursor = %d, want -1", m.orgCursor)
	}
	if got := m.orgInput.Value(); got != "" {
		t.Errorf("value = %q, want empty after arrowing past the top", got)
	}
}

func manyJobs(n int) []scan.Job {
	now := time.Now().UTC()
	jobs := make([]scan.Job, n)
	for i := range jobs {
		jobs[i] = scan.Job{
			Owner:     "org-a",
			RunID:     int64(1000 + i),
			Status:    "in_progress",
			Repo:      "repo",
			Workflow:  "wf" + strconv.Itoa(i),
			Branch:    "main",
			Name:      "job-" + strconv.Itoa(1000+i),
			Runner:    "r" + strconv.Itoa(1000+i),
			StartedAt: now.Add(-time.Minute),
		}
	}
	return jobs
}

func scrollModel(t *testing.T, n int) model {
	t.Helper()
	m := newModel(options{org: "org-c", interval: 30})
	m.jobs = manyJobs(n)
	m.refresh()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return next.(model)
}

func assertSelectedVisible(t *testing.T, m model, context string) {
	t.Helper()
	tbl := m.focusedTable()
	if len(tbl.Rows()) == 0 {
		return
	}
	jobs := m.running
	if m.focus == 1 {
		jobs = m.queued
	}
	name := jobs[tbl.Cursor()].Name
	if !strings.Contains(tbl.View(), name) {
		t.Fatalf("%s: selected row %q (cursor %d of %d) not in table view:\n%s", context, name, tbl.Cursor(), len(tbl.Rows()), tbl.View())
	}
}

func TestMoveDownToLastRowKeepsItVisible(t *testing.T) {
	m := scrollModel(t, 30)
	for i := 0; i < 29; i++ {
		m = send(m, key("j"))
		assertSelectedVisible(t, m, "j step "+strconv.Itoa(i+1))
	}
	if got := m.runTable.Cursor(); got != 29 {
		t.Fatalf("cursor = %d, want 29", got)
	}
	if !strings.Contains(m.runTable.View(), "job-1029") {
		t.Fatal("last row not rendered")
	}
}

func TestJumpKeysKeepSelectionVisible(t *testing.T) {
	cases := []struct {
		name string
		keys []tea.KeyMsg
		want int
	}{
		{"G", []tea.KeyMsg{key("G")}, 29},
		{"end", []tea.KeyMsg{{Type: tea.KeyEnd}}, 29},
		{"G then gg", []tea.KeyMsg{key("G"), key("g"), key("g")}, 0},
		{"G then home", []tea.KeyMsg{key("G"), {Type: tea.KeyHome}}, 0},
		{"pgdown", []tea.KeyMsg{{Type: tea.KeyPgDown}}, 13},
		{"pgdown twice", []tea.KeyMsg{{Type: tea.KeyPgDown}, {Type: tea.KeyPgDown}}, 26},
		{"pgdown thrice clamps", []tea.KeyMsg{{Type: tea.KeyPgDown}, {Type: tea.KeyPgDown}, {Type: tea.KeyPgDown}}, 29},
		{"gg from middle", []tea.KeyMsg{{Type: tea.KeyPgDown}, key("g"), key("g")}, 0},
		{"ctrl+d", []tea.KeyMsg{{Type: tea.KeyCtrlD}}, 6},
		{"ctrl+d twice", []tea.KeyMsg{{Type: tea.KeyCtrlD}, {Type: tea.KeyCtrlD}}, 12},
		{"G then ctrl+u", []tea.KeyMsg{key("G"), {Type: tea.KeyCtrlU}}, 23},
		{"ctrl+f", []tea.KeyMsg{{Type: tea.KeyCtrlF}}, 13},
		{"G then ctrl+b", []tea.KeyMsg{key("G"), {Type: tea.KeyCtrlB}}, 16},
		{"H at top", []tea.KeyMsg{key("H")}, 0},
		{"M at top", []tea.KeyMsg{key("M")}, 6},
		{"L at top", []tea.KeyMsg{key("L")}, 12},
		{"G then H", []tea.KeyMsg{key("G"), key("H")}, 17},
		{"G then M", []tea.KeyMsg{key("G"), key("M")}, 23},
		{"G then L", []tea.KeyMsg{key("G"), key("L")}, 29},
		{"pgdown then H", []tea.KeyMsg{{Type: tea.KeyPgDown}, key("H")}, 1},
		{"pgdown then M", []tea.KeyMsg{{Type: tea.KeyPgDown}, key("M")}, 7},
		{"pgdown then L", []tea.KeyMsg{{Type: tea.KeyPgDown}, key("L")}, 13},
		{"G then pgup", []tea.KeyMsg{key("G"), {Type: tea.KeyPgUp}}, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := scrollModel(t, 30)
			for _, k := range tc.keys {
				m = send(m, k)
				assertSelectedVisible(t, m, tc.name)
			}
			if got := m.runTable.Cursor(); got != tc.want {
				t.Fatalf("cursor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRefreshShrinkKeepsSelectionVisible(t *testing.T) {
	m := scrollModel(t, 30)
	for i := 0; i < 29; i++ {
		m = send(m, key("j"))
	}
	m.jobs = manyJobs(20)
	m.refresh()
	if got := m.runTable.Cursor(); got != 19 {
		t.Fatalf("cursor after shrink = %d, want 19", got)
	}
	assertSelectedVisible(t, m, "after shrink")
	if !strings.Contains(m.runTable.View(), "job-1019") {
		t.Fatal("last row not rendered after shrink")
	}
}

func TestScrollRandomWalkKeepsSelectionVisible(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keys := []tea.KeyMsg{key("j"), key("k"), key("down"), key("up"), key("g"), key("G"), {Type: tea.KeyPgDown}, {Type: tea.KeyPgUp}, key("H"), key("M"), key("L"), {Type: tea.KeyCtrlD}, {Type: tea.KeyCtrlU}}
	heights := []int{8, 12, 16, 24, 40, 60}
	for round := 0; round < 40; round++ {
		m := scrollModel(t, 5+rng.Intn(60))
		for step := 0; step < 150; step++ {
			switch rng.Intn(10) {
			case 0:
				next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: heights[rng.Intn(len(heights))]})
				m = next.(model)
			case 1:
				m.jobs = manyJobs(1 + rng.Intn(60))
				m.refresh()
			default:
				m = send(m, keys[rng.Intn(len(keys))])
			}
			assertSelectedVisible(t, m, "round "+strconv.Itoa(round)+" step "+strconv.Itoa(step))
		}
	}
}

type cancelCall struct {
	owner string
	repo  string
	runID int64
}

func stubCancel(t *testing.T, result error) *[]cancelCall {
	t.Helper()
	var calls []cancelCall
	prev := cancelRun
	cancelRun = func(_ context.Context, owner, repo string, runID int64) error {
		calls = append(calls, cancelCall{owner, repo, runID})
		return result
	}
	t.Cleanup(func() { cancelRun = prev })
	return &calls
}

func sendCmd(m model, k tea.KeyMsg) (model, tea.Cmd) {
	next, cmd := m.Update(k)
	return next.(model), cmd
}

func TestCancelConfirmThenYesSendsOnce(t *testing.T) {
	calls := stubCancel(t, nil)
	m := scrollModel(t, 3)
	m = send(m, key("j"))

	m, cmd := sendCmd(m, key("c"))
	if cmd != nil || !m.confirming {
		t.Fatalf("c: confirming=%v cmd=%v, want prompt and no cmd", m.confirming, cmd)
	}
	if view := m.View(); !strings.Contains(view, "cancel run wf1 (repo)? stops every job in this run · y/n") {
		t.Fatalf("prompt missing from view:\n%s", view)
	}
	if len(*calls) != 0 {
		t.Fatal("request sent before confirmation")
	}

	m, cmd = sendCmd(m, key("y"))
	if cmd == nil || m.confirming {
		t.Fatalf("y: confirming=%v cmd=%v, want a cancel cmd", m.confirming, cmd)
	}
	msg := cmd()
	if len(*calls) != 1 || (*calls)[0] != (cancelCall{"org-a", "repo", 1001}) {
		t.Fatalf("calls = %+v", *calls)
	}
	next, _ := m.Update(msg)
	m = next.(model)
	if !strings.Contains(m.notice, "cancel accepted") {
		t.Fatalf("notice = %q", m.notice)
	}
	if !strings.Contains(m.runTable.View(), "cancelling") {
		t.Fatal("row not marked cancelling")
	}
}

func TestCancelConfirmNoSendsNothing(t *testing.T) {
	calls := stubCancel(t, nil)
	for _, k := range []string{"n", "x", "esc", "enter"} {
		t.Run(k, func(t *testing.T) {
			m := scrollModel(t, 3)
			m = send(m, key("c"))
			m, cmd := sendCmd(m, key(k))
			if cmd != nil || m.confirming || len(m.cancels) != 0 {
				t.Fatalf("confirming=%v cmd=%v cancels=%v", m.confirming, cmd, m.cancels)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
}

func TestCancelSecondRequestWhileInFlightSendsNothing(t *testing.T) {
	calls := stubCancel(t, nil)
	m := scrollModel(t, 3)
	m = send(m, key("c"))
	m, cmd := sendCmd(m, key("y"))
	if cmd == nil {
		t.Fatal("first y produced no cmd")
	}

	m, cmd = sendCmd(m, key("c"))
	if cmd != nil || m.confirming {
		t.Fatalf("second c: confirming=%v cmd=%v", m.confirming, cmd)
	}
	if !strings.Contains(m.notice, "already requested") {
		t.Fatalf("notice = %q", m.notice)
	}
	if len(*calls) != 0 {
		t.Fatalf("cmd was not executed, calls = %+v", *calls)
	}

}

func TestCancelStaysBlockedAfterAcceptedUntilJobGone(t *testing.T) {
	stubCancel(t, nil)
	m := scrollModel(t, 3)
	m = send(m, key("c"))
	m, _ = sendCmd(m, key("y"))
	next, _ := m.Update(cancelDoneMsg{job: m.confirmJob})
	m = next.(model)

	m, cmd := sendCmd(m, key("c"))
	if cmd != nil || m.confirming {
		t.Fatalf("c after accepted: confirming=%v cmd=%v", m.confirming, cmd)
	}
}

func TestCancelOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantNotice string
		wantMarked bool
	}{
		{"accepted", nil, "cancel accepted", true},
		{"finished", scan.ErrRunFinished, "run already finished", true},
		{"no permission", scan.ErrNoCancelPermission, "no permission to cancel", false},
		{"rate limited", scan.ErrRateLimited, "rate limited", false},
		{"other", errors.New("boom"), "cancel failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := scrollModel(t, 3)
			m = send(m, key("c"))
			job := m.confirmJob
			m = send(m, key("y"))
			next, _ := m.Update(cancelDoneMsg{job: job, err: tc.err})
			m = next.(model)
			if !strings.Contains(m.notice, tc.wantNotice) {
				t.Fatalf("notice = %q, want %q", m.notice, tc.wantNotice)
			}
			if m.cancels[job.RunID] != tc.wantMarked {
				t.Fatalf("marked = %v, want %v", m.cancels[job.RunID], tc.wantMarked)
			}
			if got := strings.Contains(m.runTable.View(), "cancelling"); got != tc.wantMarked {
				t.Fatalf("row shows cancelling = %v, want %v", got, tc.wantMarked)
			}
			if strings.Contains(m.statusView(), "test-token") {
				t.Fatal("token in status line")
			}
		})
	}
}

func TestCancelKeysIgnoredWhileFilterActive(t *testing.T) {
	stubCancel(t, nil)
	m := scrollModel(t, 3)
	m = send(m, key("/"))
	m = typeRunes(m, "cy")
	if m.confirming || len(m.cancels) != 0 {
		t.Fatalf("filter typing triggered cancel: confirming=%v cancels=%v", m.confirming, m.cancels)
	}
	if got := m.filterInput.Value(); got != "cy" {
		t.Fatalf("filter input = %q, want cy", got)
	}
}

func TestCancelKeysIgnoredWhileOrgOverlayActive(t *testing.T) {
	stubCancel(t, nil)
	m := scrollModel(t, 3)
	m = send(m, key("O"))
	m = typeRunes(m, "cy")
	if m.confirming || len(m.cancels) != 0 {
		t.Fatalf("overlay typing triggered cancel: confirming=%v cancels=%v", m.confirming, m.cancels)
	}
	if got := m.orgInput.Value(); got != "cy" {
		t.Fatalf("org input = %q, want cy", got)
	}
}

func TestCancelNeedsSelectionWithRunID(t *testing.T) {
	m := newModel(options{org: "org-c", interval: 30})
	m = send(m, key("c"))
	if m.confirming {
		t.Fatal("c with no rows opened a prompt")
	}

	m.jobs = []scan.Job{{Status: "in_progress", Repo: "r", Workflow: "w", Name: "j", Runner: "x"}}
	m.refresh()
	m = send(m, key("c"))
	if m.confirming {
		t.Fatal("c on a job without a run id opened a prompt")
	}
}

func TestCancelWorksFromQueuedPanel(t *testing.T) {
	calls := stubCancel(t, nil)
	m := newModel(options{org: "org-c", interval: 30})
	m.jobs = []scan.Job{{Owner: "org-a", RunID: 55, Status: "queued", Repo: "web", Workflow: "deploy", Name: "j", Labels: "x", CreatedAt: time.Now().UTC()}}
	m.refresh()
	m = send(m, key("tab"))
	m = send(m, key("c"))
	m, cmd := sendCmd(m, key("y"))
	if cmd == nil {
		t.Fatal("no cmd from queued panel")
	}
	cmd()
	if len(*calls) != 1 || (*calls)[0] != (cancelCall{"org-a", "web", 55}) {
		t.Fatalf("calls = %+v", *calls)
	}
	if !strings.Contains(m.qTable.View(), "cancelling") {
		t.Fatal("queued row not marked cancelling")
	}
}

func TestOrgChangeWhileScanningCancelsAndIgnoresStaleScan(t *testing.T) {
	m := newTestModel(t)
	stopped := false
	m.scanning = true
	m.stopScan = func() { stopped = true }
	oldID := m.scanID

	m = send(m, key("O"))
	m = typeRunes(m, "org-b")
	m = send(m, key("enter"))

	if !stopped {
		t.Fatal("previous scan was not cancelled")
	}
	if m.scanID == oldID || !m.scanning {
		t.Fatalf("scanID=%d old=%d scanning=%v", m.scanID, oldID, m.scanning)
	}

	next, _ := m.Update(scanDoneMsg{id: oldID, jobs: testJobs()})
	m = next.(model)
	if !m.scanning || len(m.jobs) != 0 {
		t.Fatalf("stale scan applied: scanning=%v jobs=%d", m.scanning, len(m.jobs))
	}

	next, _ = m.Update(scanDoneMsg{id: m.scanID, jobs: testJobs()})
	m = next.(model)
	if m.scanning || len(m.jobs) != 3 {
		t.Fatalf("current scan not applied: scanning=%v jobs=%d", m.scanning, len(m.jobs))
	}
}

func TestRescanKeyIgnoredWhileScanning(t *testing.T) {
	m := newTestModel(t)
	m.scanning = true
	m, cmd := sendCmd(m, key("r"))
	if cmd != nil {
		t.Fatal("r started a second scan while one was running")
	}
}

func TestSingleGDoesNotJump(t *testing.T) {
	cases := []struct {
		name string
		keys []tea.KeyMsg
		want int
	}{
		{"g then j", []tea.KeyMsg{key("G"), key("k"), key("g"), key("j")}, 29},
		{"g then k", []tea.KeyMsg{key("G"), key("g"), key("k")}, 28},
		{"g then j then g", []tea.KeyMsg{key("G"), key("g"), key("j"), key("g")}, 29},
		{"g then tab then g", []tea.KeyMsg{key("G"), key("g"), key("tab"), key("tab"), key("g")}, 29},
		{"lone g", []tea.KeyMsg{key("G"), key("g")}, 29},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := scrollModel(t, 30)
			for _, k := range tc.keys {
				m = send(m, k)
			}
			if got := m.runTable.Cursor(); got != tc.want {
				t.Fatalf("cursor = %d, want %d", got, tc.want)
			}
			assertSelectedVisible(t, m, tc.name)
		})
	}
}

func TestVimKeysIgnoredWhileFilterActive(t *testing.T) {
	m := scrollModel(t, 30)
	m = send(m, key("G"))
	m = send(m, key("/"))
	m = typeRunes(m, "ggHML")
	if got := m.filterInput.Value(); got != "ggHML" {
		t.Fatalf("filter input = %q, want ggHML", got)
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyCtrlD}, {Type: tea.KeyCtrlU}, {Type: tea.KeyCtrlF}, {Type: tea.KeyCtrlB}} {
		m = send(m, k)
	}
	if got := m.runTable.Cursor(); got != 29 {
		t.Fatalf("cursor moved to %d while the filter was active", got)
	}
}

func TestVimKeysIgnoredWhileOrgOverlayActive(t *testing.T) {
	m := scrollModel(t, 30)
	m = send(m, key("G"))
	m = send(m, key("O"))
	for _, k := range []tea.KeyMsg{key("g"), key("g"), key("H"), key("M"), key("L")} {
		m = send(m, k)
	}
	if got := m.runTable.Cursor(); got != 29 {
		t.Fatalf("cursor moved to %d while the org overlay was active", got)
	}
	if got := m.orgInput.Value(); got != "ggHML" {
		t.Fatalf("org input = %q, want ggHML", got)
	}
}

func TestVimKeysIgnoredWhileCancelConfirmActive(t *testing.T) {
	stubCancel(t, nil)
	m := scrollModel(t, 30)
	m = send(m, key("G"))
	m = send(m, key("c"))
	m, cmd := sendCmd(m, key("H"))
	if cmd != nil || m.confirming || m.runTable.Cursor() != 29 {
		t.Fatalf("H during confirm: confirming=%v cursor=%d cmd=%v", m.confirming, m.runTable.Cursor(), cmd)
	}

	m = send(m, key("c"))
	m = send(m, key("g"))
	m = send(m, key("g"))
	if m.runTable.Cursor() != 29 {
		t.Fatalf("g during confirm moved the cursor to %d", m.runTable.Cursor())
	}
}

func TestVisibleRowsMatchesRenderedWindow(t *testing.T) {
	m := scrollModel(t, 30)
	for _, k := range []tea.KeyMsg{key("G"), {Type: tea.KeyPgUp}, key("k"), key("k")} {
		m = send(m, k)
		first, last := visibleRows(m.runTable)
		for i := 0; i < 30; i++ {
			shown := strings.Contains(m.runTable.View(), "job-"+strconv.Itoa(1000+i))
			if want := i >= first && i <= last; shown != want {
				t.Fatalf("row %d shown=%v but visibleRows = %d..%d", i, shown, first, last)
			}
		}
	}
}
