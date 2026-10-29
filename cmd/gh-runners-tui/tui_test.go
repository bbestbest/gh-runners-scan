package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"gh-runners/internal/config"
	"gh-runners/internal/scan"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-runners-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("XDG_CACHE_HOME", dir)
	saveCache = func() error { return nil }
	saveLastJobs = func(string, []scan.Job) error { return nil }
	saveRounds = func(*scan.Rounds) error { return nil }
	loadRounds = scan.NewRounds
	loadLastJobs = func(string) ([]scan.Job, time.Time, bool) { return nil, time.Time{}, false }
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

func TestHLMoveFocus(t *testing.T) {
	cases := map[string][]int{"l": {1, 2, focusRepos, 0}, "h": {focusRepos, 2, 1, 0}}
	for k, want := range cases {
		m := newTestModel(t)
		if m.focus != 0 {
			t.Fatalf("initial focus = %d, want 0", m.focus)
		}
		for i, w := range want {
			m = send(m, key(k))
			if m.focus != w {
				t.Errorf("focus after %d× %s = %d, want %d", i+1, k, m.focus, w)
			}
		}
	}
	if m := send(newTestModel(t), key("3")); m.focus != 2 {
		t.Errorf("focus after 3 = %d, want 2", m.focus)
	}
	if m := send(newTestModel(t), key("0")); m.focus != focusRepos {
		t.Errorf("focus after 0 = %d, want repos", m.focus)
	}
}

func TestTabNoLongerMovesFocus(t *testing.T) {
	m := send(newTestModel(t), key("tab"))
	if m.focus != 0 {
		t.Errorf("focus after tab = %d, want 0", m.focus)
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

func TestViewContents(t *testing.T) {
	m := newTestModel(t)
	view := m.View()
	for _, want := range []string{"≡ [0] Repos", "▶ [1] Running (2)", "⏸ [2] Queued (1)", "✓ [3] Finished · 24h (0)", "?: help"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

func TestKeyBarHintsFollowConfig(t *testing.T) {
	m := newTestModel(t)
	bar := m.statusView()
	if !strings.Contains(bar, "?: help") || strings.Contains(bar, "c: cancel run") {
		t.Fatalf("default key bar = %q, want only ?: help", bar)
	}
	m.opts.hints = "full"
	bar = m.statusView()
	for _, want := range []string{"enter: open", "c: cancel run", "?: help"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("hints = full key bar = %q, missing %q", bar, want)
		}
	}
	m.opts.hints = ""
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 40})
	m = next.(model)
	if bar = m.statusView(); !strings.Contains(bar, "?: help") {
		t.Fatalf("narrow key bar = %q, want ?: help kept", bar)
	}
	m = send(m, key("/"))
	if bar = m.statusView(); !strings.Contains(bar, "enter: keep filter") || strings.Contains(bar, "?: help") {
		t.Fatalf("filter key bar = %q, want the filter hints", bar)
	}
}

func TestHelpPopupGroupsKeysAndSwallowsOthers(t *testing.T) {
	m := newTestModel(t)
	m = send(m, key("?"))
	if !m.helpActive {
		t.Fatal("? did not open help")
	}
	view := m.View()
	for _, want := range []string{"? Keys", "Navigate", "Actions", "View", "cancel run", "re-run finished run", "hints = full", "esc close"} {
		if !strings.Contains(view, want) {
			t.Fatalf("help view missing %q:\n%s", want, view)
		}
	}
	before, scanID := m.focus, m.scanID
	for _, k := range []string{"l", "c", "r", "/"} {
		m = send(m, key(k))
	}
	if !m.helpActive || m.focus != before || m.confirming || m.filterActive || m.scanID != scanID {
		t.Fatalf("keys leaked through help: help=%v focus=%d confirming=%v filter=%v scanID=%d", m.helpActive, m.focus, m.confirming, m.filterActive, m.scanID)
	}
	for _, k := range []string{"esc", "?", "q"} {
		m.helpActive = true
		next, cmd := m.Update(key(k))
		m = next.(model)
		if m.helpActive || cmd != nil {
			t.Fatalf("%s: help open=%v cmd=%v, want closed without quitting", k, m.helpActive, cmd)
		}
	}
	m.opts.hints = "full"
	m = send(m, key("?"))
	if !strings.Contains(m.View(), "hints = short") {
		t.Fatal("help with hints = full does not point at hints = short")
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	m = next.(model)
	if view := m.View(); !strings.Contains(view, "q  ") || !strings.Contains(view, "esc close") || strings.Contains(view, "hints = short") {
		t.Fatalf("help on a 24-row screen lost keys or kept the tip:\n%s", view)
	}
}

func TestRowsCarryStatusGlyphs(t *testing.T) {
	m := finishedModel(t)
	m.reruns = map[int64]bool{2002: true}
	m.refresh()
	if got := m.runTable.Rows()[0][0]; !strings.HasPrefix(got, "● ") {
		t.Fatalf("running cell = %q, want ● prefix", got)
	}
	m.jobs = append(m.jobs, scan.Job{Status: "queued", Repo: "repo", Name: "q", CreatedAt: time.Now().UTC()})
	m.refresh()
	if got := m.qTable.Rows()[0][0]; !strings.HasPrefix(got, "○ ") {
		t.Fatalf("queued cell = %q, want ○ prefix", got)
	}
	got := []string{m.fTable.Rows()[0][3], m.fTable.Rows()[1][3]}
	if got[0] != "✗ failure" || got[1] != "↻ re-running" {
		t.Fatalf("finished results = %q, want ✗ failure and ↻ re-running", got)
	}
	for c, want := range map[string]string{"success": "✓", "failure": "✗", "timed_out": "✗", "startup_failure": "✗", "action_required": "✗", "cancelled": "⊘", "skipped": "–", "neutral": "–", "": "·"} {
		if g := resultGlyph(c); g != want {
			t.Errorf("resultGlyph(%q) = %q, want %q", c, g, want)
		}
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
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 65})
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
		{">", []tea.KeyMsg{key(">")}, 29},
		{"> then <", []tea.KeyMsg{key(">"), key("<")}, 0},
		{"pgdown then <", []tea.KeyMsg{{Type: tea.KeyPgDown}, key("<")}, 0},
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
	if view := m.View(); !strings.Contains(view, "Cancel run #1001 of repo › wf1?") {
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
		{"no permission", scan.ErrNoPermission, "no permission to cancel", false},
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
	m = send(m, key("l"))
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

func space() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
}

func formatted(ts []scan.Target) string {
	return strings.Join(scan.FormatTargets(ts), ",")
}

func TestPickWhileScanningWaitsForNextScan(t *testing.T) {
	m := newTestModel(t)
	stopped := false
	m.scanning = true
	m.stopScan = func() { stopped = true }
	oldID := m.scanID

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m = withAccounts(m, []scan.Account{{Login: "me-user"}, {Login: "org-b", Org: true}}, nil)
	m = send(m, key("O"))
	m = send(m, key("g"))
	m = send(m, key("j"))
	m = send(m, key("enter"))
	m = withRepos(m, []string{"api", "web"}, nil)
	m = send(m, key("j"))
	m = send(m, space())
	m = send(m, key("esc"))
	m = send(m, key("s"))

	if stopped || m.scanID != oldID || formatted(m.opts.targets) != "org-c/repo-a" {
		t.Fatalf("save interrupted the running scan: stopped=%v scanID=%d targets=%v", stopped, m.scanID, m.opts.targets)
	}
	if m.pending == nil || formatted(m.pending.targets) != "org-b/api,org-c/repo-a" {
		t.Fatalf("pending = %+v", m.pending)
	}
	if !strings.Contains(ansi.Strip(m.statusView()), "next scan: org-b/api, org-c/repo-a") {
		t.Fatal("key bar does not show the queued selection")
	}

	next, _ := m.Update(scanDoneMsg{id: oldID, jobs: testJobs()})
	m = next.(model)
	if len(m.jobs) != 3 {
		t.Fatalf("the running scan's results were dropped: jobs=%d", len(m.jobs))
	}

	m.nextIn = 1
	next, cmd := m.Update(tickMsg(time.Now()))
	m = next.(model)
	if cmd == nil || !m.scanning || m.scanID == oldID || formatted(m.opts.targets) != "org-b/api,org-c/repo-a" || m.pending != nil || len(m.jobs) != 0 {
		t.Fatalf("next scan did not switch: scanning=%v targets=%v pending=%v jobs=%d", m.scanning, m.opts.targets, m.pending, len(m.jobs))
	}
}

func TestRescanKeyAppliesQueuedPick(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}, {Login: "org-c", Org: true}}, nil)
	m.scanning = false
	m = send(m, key("O"))
	m = send(m, key("g"))
	m = send(m, space())
	m = send(m, key("j"))
	m = send(m, space())
	m = send(m, key("s"))
	if m.scanning || m.pending == nil || formatted(m.pending.targets) != "me-user/*" {
		t.Fatalf("save started a scan at once or wrong pending: scanning=%v pending=%v", m.scanning, m.pending)
	}
	m = send(m, key("r"))
	if !m.scanning || formatted(m.opts.targets) != "me-user/*" || m.pending != nil {
		t.Fatalf("r: scanning=%v targets=%v pending=%v", m.scanning, m.opts.targets, m.pending)
	}
}

func TestFirstPickWithoutOwnerScansAtOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withAccounts(newModel(options{interval: 30}), []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, space())
	m, cmd := sendCmd(m, key("s"))
	if cmd == nil || !m.scanning || formatted(m.opts.targets) != "me-user/*" || m.pending != nil {
		t.Fatalf("scanning=%v targets=%v pending=%v", m.scanning, m.opts.targets, m.pending)
	}
	if got := config.Load().Scan; strings.Join(got, ",") != "me-user/*" {
		t.Fatalf("saved scan = %v", got)
	}
}

func TestSaveNeedsASelection(t *testing.T) {
	m := withAccounts(newModel(options{interval: 30}), []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, key("s"))
	if !m.picking || m.scanning || !strings.Contains(popupText(m), "select at least one account or repo") {
		t.Fatal("empty selection was accepted")
	}
}

func TestChecklistMarksAllSomeNone(t *testing.T) {
	m := withAccounts(layoutModel(t, 140, 40), []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}, {Login: "org-b", Org: true}}, nil)
	m.opts.targets = []scan.Target{{Owner: "me-user", All: true}, {Owner: "org-a", Repos: []string{"api", "web"}}}
	m = send(m, key("O"))
	view := popupText(m)
	for _, want := range []string{"Choose what to scan", "[x] me-user  you   all repos", "[~] org-a    org   2 repos", "[ ] org-b    org", "space all/none · enter pick repos", "s save · esc cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("checklist lacks %q", want)
		}
	}
}

func TestToggleRepoWhileAllSelectsTheRest(t *testing.T) {
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, key("O"))
	m = send(m, key("g"))
	m = send(m, space())
	m = send(m, key("enter"))
	m = withRepos(m, []string{"a", "b", "c"}, nil)
	if !strings.Contains(popupText(m), "[x] all repositories") || !strings.Contains(popupText(m), "[x] b") {
		t.Fatal("all repositories not shown as checked")
	}
	m = send(m, key("j"))
	m = send(m, key("j"))
	m = send(m, space())
	c := m.sel["me-user"]
	if c.all || len(c.repos) != 2 || !c.repos["a"] || !c.repos["c"] {
		t.Fatalf("after unchecking b: all=%v repos=%v", c.all, c.repos)
	}
	m = send(m, key("k"))
	m = send(m, space())
	m = send(m, key("j"))
	m = send(m, key("j"))
	m = send(m, space())
	if _, ok := m.sel["me-user"]; ok {
		t.Fatal("unchecking every repo should drop the account")
	}
}

func TestSavedReposOutsideTheListStayListed(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", Repos: []string{"old-repo"}}}, interval: 30})
	m = withAccounts(m, []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}}, nil)
	m = send(m, key("O"))
	m = send(m, key("enter"))
	m = withRepos(m, []string{"fresh"}, nil)
	if view := popupText(m); !strings.Contains(view, "[ ] fresh") || !strings.Contains(view, "[x] old-repo") {
		t.Fatal("a saved repo older than the days filter disappeared from the list")
	}
}

func TestMultipleOwnersShowOwnerSlashRepo(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", All: true}, {Owner: "org-b", All: true}}, interval: 30})
	m.scanning = false
	now := time.Now().UTC()
	m.jobs = []scan.Job{
		{Status: "in_progress", Owner: "org-a", Repo: "api", Name: "build", Runner: "r1", RunID: 1, StartedAt: now},
		{Status: "queued", Owner: "org-b", Repo: "api", Name: "test", Labels: "x", RunID: 2, CreatedAt: now},
	}
	m.refresh()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)
	view := ansi.Strip(m.View())
	for _, want := range []string{"org-a/api", "org-b/api", "org-a, org-b"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	if len(m.repos) != 2 {
		t.Fatalf("repo counts merged two owners' api repos: %+v", m.repos)
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
		{"g then l then g", []tea.KeyMsg{key("G"), key("g"), key("l"), key("l"), key("g")}, 29},
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
	m = typeRunes(m, "jo")
	if got := m.filterInput.Value(); got != "jo" {
		t.Fatalf("filter input = %q, want jo", got)
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyCtrlD}, {Type: tea.KeyCtrlU}, {Type: tea.KeyCtrlF}, {Type: tea.KeyCtrlB}} {
		m = send(m, k)
	}
	if got := m.runTable.Cursor(); got != 29 {
		t.Fatalf("cursor moved to %d while the filter was active", got)
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

func stubUsage(t *testing.T, u scan.UsageSnapshot) {
	t.Helper()
	orig := usage
	usage = func() scan.UsageSnapshot { return u }
	t.Cleanup(func() { usage = orig })
}

func TestUsageOverlayOpenClose(t *testing.T) {
	stubUsage(t, scan.UsageSnapshot{})
	for _, closer := range []string{"esc", "u", "q"} {
		m := send(newTestModel(t), key("u"))
		if !m.usageActive {
			t.Fatalf("u did not open overlay")
		}
		m = send(m, key(closer))
		if m.usageActive {
			t.Fatalf("%s did not close overlay", closer)
		}
	}
}

func TestUsageOverlayIgnoresTableKeys(t *testing.T) {
	stubUsage(t, scan.UsageSnapshot{})
	m := send(newTestModel(t), key("u"))
	cursor := m.runTable.Cursor()
	focus := m.focus
	for _, k := range []string{"j", "k", "G", "g", "h", "l", "2", "c", "/", "O", "r", "enter"} {
		m = send(m, key(k))
	}
	if !m.usageActive || m.runTable.Cursor() != cursor || m.focus != focus || m.confirming || m.filterActive || m.picking {
		t.Fatalf("keys leaked through overlay: %+v", m.usageActive)
	}
}

func TestUsageOverlayNoData(t *testing.T) {
	stubUsage(t, scan.UsageSnapshot{})
	m := send(newTestModel(t), key("u"))
	view := m.View()
	if !strings.Contains(view, "no data yet") {
		t.Fatalf("view lacks no-data text:\n%s", view)
	}
	if !strings.Contains(view, "api –") {
		t.Fatalf("status line lacks api –")
	}
}

func TestUsageOverlayContents(t *testing.T) {
	stubUsage(t, scan.UsageSnapshot{
		HasData: true, Resource: "core", Limit: 5000, Remaining: 4084, Used: 916,
		Reset: time.Now().Add(10*time.Minute + 30*time.Second), Requests: 120, NotModified: 100, RateLimited: 2,
	})
	m := newTestModel(t)
	m.lastCost = &scanCost{requests: 40, free: 30}
	m = send(m, key("u"))
	view := m.View()
	for _, want := range []string{"core", "5000", "916", "4084", "in 10m", "120", "100", "40 requests, 30 free (304)", "30s"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay lacks %q", want)
		}
	}
}

func TestUsageIndicator(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		remaining int
		style     string
	}{{4084, "dim"}, {900, "warn"}, {100, "err"}} {
		stubUsage(t, scan.UsageSnapshot{HasData: true, Limit: 5000, Remaining: tc.remaining, Reset: reset})
		m := newTestModel(t)
		want := "api " + strconv.Itoa(tc.remaining) + "/5000 · resets " + reset.Local().Format("15:04")
		if !strings.Contains(m.statusView(), want) {
			t.Fatalf("status line lacks %q: %q", want, m.statusView())
		}
		u := usage()
		var got string
		switch usageStyle(u).GetForeground() {
		case colorDim:
			got = "dim"
		case colorYellow:
			got = "warn"
		case colorRed:
			got = "err"
		}
		if got != tc.style {
			t.Errorf("remaining %d style = %s, want %s", tc.remaining, got, tc.style)
		}
	}
}

func TestScanDoneRecordsCost(t *testing.T) {
	stubUsage(t, scan.UsageSnapshot{})
	m := newTestModel(t)
	m.scanID = 3
	next, _ := m.Update(scanDoneMsg{id: 3, requests: 12, free: 9})
	m = next.(model)
	if m.lastCost == nil || m.lastCost.requests != 12 || m.lastCost.free != 9 {
		t.Fatalf("lastCost = %+v", m.lastCost)
	}
}

func stubSaveCache(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	orig := saveCache
	saveCache = func() error {
		*calls++
		return nil
	}
	t.Cleanup(func() { saveCache = orig })
	return calls
}

func runBatch(cmd tea.Cmd) []tea.Msg {
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		if c != nil {
			out = append(out, runBatch(c)...)
		}
	}
	return out
}

func TestScanDoneSetsWindowTitle(t *testing.T) {
	stubSaveCache(t)
	cases := []struct {
		name string
		jobs []scan.Job
		want string
	}{
		{"none queued", []scan.Job{{Status: "in_progress", Runner: "r1"}}, "gh-runner"},
		{"two queued", []scan.Job{{Status: "queued"}, {Status: "queued"}, {Status: "in_progress"}}, "(2 queued) gh-runner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			_, cmd := m.Update(scanDoneMsg{id: m.scanID, jobs: tc.jobs})
			found := false
			for _, msg := range runBatch(cmd) {
				if fmt.Sprint(msg) == fmt.Sprint(tea.SetWindowTitle(tc.want)()) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no window title %q in %v", tc.want, runBatch(cmd))
			}
		})
	}
}

func TestColumnWidthsFitPanel(t *testing.T) {
	for inner := 30; inner <= 120; inner++ {
		total := 0
		for _, c := range columnWidths(runningPanelCols, inner) {
			if c.Width < colMin(c.Title) {
				t.Fatalf("inner %d: %s width %d < min %d", inner, c.Title, c.Width, colMin(c.Title))
			}
			total += c.Width
		}
		avail := inner - 2 - (len(runningPanelCols)-1)*2
		if avail >= 5*len(runningPanelCols)+5 && total > avail {
			t.Fatalf("inner %d: columns %d wider than %d", inner, total, avail)
		}
	}
}

func TestStepShownForRunningJob(t *testing.T) {
	m := newTestModel(t)
	m.jobs = []scan.Job{{Status: "in_progress", Runner: "r1", Repo: "repo-a", Name: "build", Step: scan.Step{Num: 4, Total: 7, Name: "Run tests"}}}
	m.refresh()
	if !strings.Contains(m.runTable.View(), "4/7") {
		t.Fatalf("running table missing step:\n%s", m.runTable.View())
	}
	if !strings.Contains(ansi.Strip(m.detailPanel(80, 30)), "Step       4/7 · Run tests") {
		t.Fatalf("detail missing step:\n%s", ansi.Strip(m.detailPanel(80, 30)))
	}
}

func TestLastResultsShownStaleUntilScan(t *testing.T) {
	prevLoad, prevSave := loadLastJobs, saveLastJobs
	t.Cleanup(func() { loadLastJobs, saveLastJobs = prevLoad, prevSave })
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	var asked string
	loadLastJobs = func(key string) ([]scan.Job, time.Time, bool) {
		asked = key
		return testJobs(), at, true
	}
	var saved []string
	saveLastJobs = func(key string, jobs []scan.Job) error {
		saved = append(saved, key)
		return nil
	}
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", All: true}}, days: 7, interval: 30})
	m.restoreLast()
	if asked != "org-a/*|7|" {
		t.Fatalf("view key = %q", asked)
	}
	if !m.stale || len(m.jobs) != len(testJobs()) {
		t.Fatalf("stale=%v jobs=%d, want restored", m.stale, len(m.jobs))
	}
	if got := ansi.Strip(m.statusPanelLines(60)[1]); got != " last results from "+clock(at, "15:04:05") {
		t.Fatalf("status line = %q", got)
	}

	next, cmd := m.Update(scanDoneMsg{id: m.scanID, jobs: testJobs()[:1], err: scan.ErrRateLimited})
	m = next.(model)
	runBatch(cmd)
	if m.stale || len(saved) != 0 {
		t.Fatalf("after limited scan: stale=%v saved=%v, want fresh and not saved", m.stale, saved)
	}
	_, cmd = m.Update(scanDoneMsg{id: m.scanID, jobs: testJobs()})
	runBatch(cmd)
	if len(saved) != 1 || saved[0] != "org-a/*|7|" {
		t.Fatalf("saved = %v, want one save for the view", saved)
	}
}

func TestNoLastResultsKeepsEmpty(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", All: true}}, days: 7, interval: 30})
	m.restoreLast()
	if m.stale || len(m.jobs) != 0 {
		t.Fatalf("stale=%v jobs=%d, want nothing restored", m.stale, len(m.jobs))
	}
}

func TestRepoJobsMsgReplacesThatRepoOnly(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", All: true}}, interval: 30})
	m.jobs = []scan.Job{
		{Owner: "org-a", Repo: "api", Status: "queued", Name: "old-api"},
		{Owner: "org-a", Repo: "web", Status: "queued", Name: "web-job"},
	}
	next, _ := m.Update(repoJobsMsg{id: m.scanID, owner: "org-a", repo: "API", jobs: []scan.Job{{Owner: "org-a", Repo: "api", Status: "in_progress", Runner: "r1", Name: "new-api"}}})
	m = next.(model)
	var names []string
	for _, j := range m.jobs {
		names = append(names, j.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"new-api", "web-job"}) {
		t.Fatalf("jobs = %v, want new-api and web-job", names)
	}
	if len(m.running) != 1 || len(m.queued) != 1 {
		t.Fatalf("tables not refreshed: running %d queued %d", len(m.running), len(m.queued))
	}
	next, _ = m.Update(repoJobsMsg{id: m.scanID + 1, owner: "org-a", repo: "web", jobs: nil})
	if got := len(next.(model).jobs); got != 2 {
		t.Fatalf("stale scan changed jobs: %d", got)
	}
}

func TestScanLabeledFiltersStreamedRepos(t *testing.T) {
	var got []scan.Job
	opts := labelStream(options{label: "arm", onRepo: func(owner, repo string, jobs []scan.Job) { got = append(got, jobs...) }})
	opts.onRepo("o", "r", []scan.Job{{Name: "a", Labels: "self-hosted,ARM64"}, {Name: "b", Labels: "x64"}})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("streamed = %+v, want only the arm job", got)
	}
	if kept := filterLabel("", []scan.Job{{Name: "a"}}); len(kept) != 1 {
		t.Fatal("empty label dropped jobs")
	}
}

func TestListingKeepsLastRepoCount(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", All: true}}, interval: 30})
	next, _ := m.Update(progressMsg{0, 75})
	next, _ = next.(model).Update(progressMsg{0, scan.TotalUnknown})
	m = next.(model)
	if m.repoCount != 75 {
		t.Fatalf("repoCount = %d during listing, want 75 kept", m.repoCount)
	}
	if got := ansi.Strip(m.statusPanelLines(60)[1]); !strings.HasPrefix(got, " 75 repos") {
		t.Fatalf("status = %q", got)
	}
}

func TestScanDoneSavesCache(t *testing.T) {
	calls := stubSaveCache(t)
	m := newTestModel(t)
	_, cmd := m.Update(scanDoneMsg{id: m.scanID, jobs: testJobs()})
	if cmd == nil {
		t.Fatal("scanDoneMsg returned no save command")
	}
	runBatch(cmd)
	if *calls != 1 {
		t.Fatalf("save calls after scan = %d, want 1", *calls)
	}

	_, cmd = m.Update(scanDoneMsg{id: m.scanID + 99})
	if cmd != nil {
		t.Fatal("stale scanDoneMsg triggered a save")
	}
}

func TestQuitSavesCache(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		calls := stubSaveCache(t)
		m := newTestModel(t)
		_, cmd := m.Update(key(k))
		if cmd == nil {
			t.Fatalf("%s returned no command", k)
		}
		if msg := cmd(); msg != tea.Quit() {
			t.Fatalf("%s sent %T, want quit", k, msg)
		}
		if *calls != 1 {
			t.Fatalf("save calls after %s = %d, want 1", k, *calls)
		}
	}
}

func TestSelectedRowKeepsHighlightAcrossCells(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	now := time.Now().UTC()
	m := newModel(options{org: "org-c", interval: 30, repo: "repo"})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)
	m.jobs = manyJobs(5)
	for i := 0; i < 3; i++ {
		m.jobs = append(m.jobs, scan.Job{Status: "queued", Owner: "org-c", Repo: "repo", Workflow: "wf", Branch: "main", Name: "queued-" + strconv.Itoa(i), RunID: int64(2000 + i), CreatedAt: now.Add(-45 * time.Minute)})
	}
	m.cancels[1002] = true
	m.refresh()

	for _, focus := range []string{"1", "2"} {
		m = send(m, key(focus))
		for step := 0; step < 3; step++ {
			m = send(m, key("j"))
			tbl := m.focusedTable()
			lines := m.tableLines(m.focus)
			selected := lines[2+tbl.Cursor()]
			if n := strings.Count(selected, "\x1b[0m"); n != 1 || !strings.HasSuffix(selected, "\x1b[0m") {
				t.Fatalf("panel %s cursor %d: selected row has %d resets, want 1 at the end: %q", focus, tbl.Cursor(), n, selected)
			}
			above := lines[1+tbl.Cursor()]
			if !strings.Contains(above, "\x1b[") {
				t.Fatalf("panel %s cursor %d: row above lost its own styling: %q", focus, tbl.Cursor(), above)
			}
		}
	}
}

func TestStyledCellsKeepEscapesWhole(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	now := time.Now().UTC()
	m := layoutModel(t, 213, 41)
	m.jobs = []scan.Job{
		{Status: "in_progress", Owner: "org-c", Repo: "service-alpha-api", Name: "Build and Push Docker Image", Runner: "runner-arm64-long-0-0", RunID: 1, StartedAt: now.Add(-80 * time.Second)},
		{Status: "in_progress", Owner: "org-c", Repo: "infra-portal-core", Name: "check-pinned-versions", Runner: "runner-arm64-long-0-1", RunID: 2, StartedAt: now.Add(-24 * time.Second)},
		{Status: "queued", Owner: "org-c", Repo: "repo-panel", Name: "update_release_draft", Labels: "self-hosted,ARM64", RunID: 3, CreatedAt: now.Add(-127 * time.Minute)},
		{Status: "queued", Owner: "org-c", Repo: "customer-portal-web", Name: "build", Labels: "self-hosted,ARM64", RunID: 4, CreatedAt: now.Add(-109 * time.Minute)},
		{Status: "queued", Owner: "org-c", Repo: "frontend-console", Name: "pr-labeler", Labels: "self-hosted,ARM64", RunID: 5, CreatedAt: now.Add(-34 * time.Minute)},
		{Status: "queued", Owner: "org-c", Repo: "external-gateway-api", Name: "Build & Release", Labels: "self-hosted,ARM64", RunID: 6, CreatedAt: now.Add(-8 * time.Minute)},
	}
	m.refresh()

	sgr := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, focus := range []string{"1", "2"} {
		m = send(m, key(focus))
		for step := 0; step < 4; step++ {
			for i, line := range strings.Split(m.View(), "\n") {
				if whole, all := len(sgr.FindAllString(line, -1)), strings.Count(line, "\x1b["); whole != all {
					t.Fatalf("panel %s step %d line %d: %d of %d escapes cut: %q", focus, step, i, all-whole, all, line)
				}
			}
			m = send(m, key("j"))
		}
	}
}

func layoutJobs() []scan.Job {
	now := time.Now().UTC()
	return []scan.Job{
		{Status: "in_progress", Owner: "org-c", Repo: "api", Workflow: "ci", Branch: "main", Name: "test", Runner: "runner-1", Labels: "self-hosted,Linux,x64", RunID: 1, StartedAt: now.Add(-2 * time.Minute)},
		{Status: "in_progress", Owner: "org-c", Repo: "api", Workflow: "ci", Branch: "main", Name: "lint", Runner: "runner-2", Labels: "self-hosted,macos", RunID: 2, StartedAt: now.Add(-time.Minute)},
		{Status: "in_progress", Owner: "org-c", Repo: "web", Workflow: "build", Branch: "dev", Name: "build", Runner: "runner-3", Labels: "self-hosted, linux", RunID: 3, StartedAt: now.Add(-time.Minute)},
		{Status: "queued", Owner: "org-c", Repo: "web", Workflow: "e2e", Branch: "feat/x", Name: "playwright", Labels: "self-hosted,linux", RunID: 4, CreatedAt: now.Add(-47 * time.Minute), URL: "https://github.com/org-c/web/actions/runs/4"},
		{Status: "queued", Owner: "org-c", Repo: "docs", Workflow: "pages", Branch: "main", Name: "publish", Labels: "self-hosted,windows", RunID: 5, CreatedAt: now.Add(-2 * time.Minute)},
	}
}

func layoutModel(t *testing.T, w, h int) model {
	t.Helper()
	m := newModel(options{org: "org-c", interval: 30})
	m.scanning = false
	m.jobs = layoutJobs()
	m.refresh()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}

func TestRepoCounts(t *testing.T) {
	m := layoutModel(t, 140, 40)
	want := []repoCount{{"api", 2, 0}, {"web", 1, 1}, {"docs", 0, 1}}
	if !reflect.DeepEqual(m.repos, want) {
		t.Fatalf("repos = %+v, want %+v", m.repos, want)
	}
	view := m.View()
	for _, s := range []string{"2 running", "1 queued", "3 active"} {
		if !strings.Contains(view, s) {
			t.Errorf("view lacks %q", s)
		}
	}
}

func TestRepoPanelKeepsItsSizeAndScrolls(t *testing.T) {
	m := layoutModel(t, 140, 40)
	few := m.reposHeight()
	var jobs []scan.Job
	for i := 0; i < maxRepoRows+3; i++ {
		jobs = append(jobs, scan.Job{Status: "queued", Repo: fmt.Sprintf("repo-%02d", i), Name: "j"})
	}
	m.jobs = jobs
	m.refresh()
	if m.reposHeight() != few || few != maxRepoRows+2 {
		t.Fatalf("repos box %d with 3 repos, %d with %d, want %d both", few, m.reposHeight(), len(jobs), maxRepoRows+2)
	}
	lines := m.repoLines()
	if len(lines) != maxRepoRows || strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "more") {
		t.Fatalf("repo lines = %d, want %d rows and no +N more", len(lines), maxRepoRows)
	}
	m = send(m, key("0"))
	m = send(m, key(">"))
	view := ansi.Strip(m.View())
	box := ansi.Strip(strings.Join(m.repoLines(), "\n"))
	if !strings.Contains(box, "repo-08") || strings.Contains(box, "repo-00") || !strings.Contains(view, "9 of 9") {
		t.Fatalf("> on repos did not scroll to the last repo:\n%s", view)
	}
	if !strings.Contains(view, "Queued (1)") || !strings.Contains(view, "╭─Repo─") {
		t.Fatalf("repo detail missing:\n%s", view)
	}
}

func TestStatusPanelKeepsEachFactOnItsLine(t *testing.T) {
	m := newModel(options{targets: []scan.Target{{Owner: "org-a", Repos: []string{"web", "api"}}, {Owner: "org-long-name-b", All: true}, {Owner: "org-long-name-c", All: true}}, interval: 30})
	m.repoCount = 89
	m.lastScan = "09:12 UTC+7"
	lines := m.statusPanelLines(40)
	if len(lines) != 3 {
		t.Fatalf("status lines = %d, want 3", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 40 {
			t.Fatalf("line %d width %d > 40: %q", i, w, ansi.Strip(l))
		}
	}
	if got := ansi.Strip(lines[0]); !strings.HasPrefix(got, " org-a") || !strings.HasSuffix(got, "…") {
		t.Fatalf("owners line = %q, want cut with …", got)
	}
	if got := ansi.Strip(lines[1]); got != " 89 repos · scanned 09:12 UTC+7" {
		t.Fatalf("scan line = %q", got)
	}
}

func TestZoneLabel(t *testing.T) {
	cases := map[int]string{0: "UTC", 7 * 3600: "UTC+7", 5*3600 + 1800: "UTC+5:30", -5 * 3600: "UTC-5", -(3*3600 + 1800): "UTC-3:30", 12*3600 + 2700: "UTC+12:45"}
	for off, want := range cases {
		if got := zoneLabel(off); got != want {
			t.Errorf("zoneLabel(%d) = %q, want %q", off, got, want)
		}
	}
}

func TestClockUsesLocalZone(t *testing.T) {
	prev := time.Local
	time.Local = time.FixedZone("ICT", 7*3600)
	defer func() { time.Local = prev }()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := clock(at, "15:04"); got != "10:04 UTC+7" {
		t.Fatalf("clock = %q, want 10:04 UTC+7", got)
	}
}

func TestBusyRunners(t *testing.T) {
	jobs := layoutJobs()
	running, queued := scan.SplitJobs(jobs)
	cases := []struct {
		name string
		q    scan.Job
		want []string
	}{
		{"subset matches across case and spaces", queued[0], []string{"runner-1", "runner-3"}},
		{"no runner has the labels", queued[1], nil},
		{"no labels", scan.Job{Status: "queued"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range busyRunners(tc.q, running) {
				got = append(got, r.Runner)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("busy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetailPanelFollowsSelection(t *testing.T) {
	m := layoutModel(t, 140, 40)
	view := m.View()
	for _, s := range []string{"api › ci › test", "Runner", "runner-1", "Elapsed", "running"} {
		if !strings.Contains(view, s) {
			t.Errorf("running detail lacks %q", s)
		}
	}
	if strings.Contains(view, "Busy runners") {
		t.Error("running job shows the busy runners section")
	}

	m = send(m, key("2"))
	view = m.View()
	for _, s := range []string{"web › e2e › playwright", "Waiting", "47m", "self-hosted, linux", "#4", "github.com/org-c/web/actions/runs/4", "Busy runners with these labels", "runner-3"} {
		if !strings.Contains(view, s) {
			t.Errorf("queued detail lacks %q", s)
		}
	}

	m = send(m, key("j"))
	if view = m.View(); !strings.Contains(view, "no running job uses all of these labels") {
		t.Error("queued job without matching runners lacks the empty message")
	}
}

func withAccounts(m model, list []scan.Account, err error) model {
	next, _ := m.Update(accountsMsg{list: list, err: err})
	return next.(model)
}

func manyAccounts(n int) []scan.Account {
	list := []scan.Account{{Login: "me-user"}}
	for i := 1; i < n; i++ {
		list = append(list, scan.Account{Login: "org-" + strconv.Itoa(i), Org: true})
	}
	return list
}

func withRepos(m model, names []string, err error) model {
	next, _ := m.Update(reposMsg{seq: m.repoSeq, owner: m.pickOwner, names: names, err: err})
	return next.(model)
}

func popupText(m model) string {
	return ansi.Strip(m.View())
}

func TestPickerOpensOnStartWithoutOwner(t *testing.T) {
	m := newModel(options{interval: 30})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = next.(model)
	if !m.picking || m.scanning {
		t.Fatalf("picking=%v scanning=%v, want the picker open and no scan", m.picking, m.scanning)
	}
	if view := popupText(m); !strings.Contains(view, "Choose what to scan") || !strings.Contains(view, "loading accounts") || !strings.Contains(view, "esc quit") {
		t.Fatal("startup picker missing title, loading line or quit hint")
	}
	m = withAccounts(m, []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}}, nil)
	if view := popupText(m); !strings.Contains(view, " > [ ] me-user  you") || !strings.Contains(view, "[ ] org-a    org") {
		t.Fatal("accounts not listed unchecked with the cursor on the first one")
	}
	if _, cmd := sendCmd(m, key("esc")); cmd == nil {
		t.Fatal("esc with no owner should quit")
	}
}

func TestPickerEscStepsBackThenCloses(t *testing.T) {
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, key("O"))
	m = send(m, key("enter"))
	m = send(m, key("esc"))
	if !m.picking || m.pickStep != 0 {
		t.Fatalf("esc on the repo step: picking=%v step=%d, want back on accounts", m.picking, m.pickStep)
	}
	m = send(m, key("esc"))
	if m.picking || m.opts.org != "org-c" {
		t.Fatalf("esc on accounts: picking=%v org=%q", m.picking, m.opts.org)
	}
}

func TestPickerShowsErrors(t *testing.T) {
	m := send(newTestModel(t), key("O"))
	m = withAccounts(m, nil, errors.New("bad credentials"))
	if !strings.Contains(popupText(m), "cannot list accounts: bad credentials") {
		t.Fatal("account error not shown")
	}
	m = send(m, key("esc"))
	m, cmd := sendCmd(m, key("O"))
	if cmd == nil || m.accountsReady || !strings.Contains(popupText(m), "loading accounts") {
		t.Fatal("reopening after an account error does not retry")
	}
	m = withAccounts(m, []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, key("enter"))
	m = withRepos(m, nil, errors.New("cannot find me-user"))
	if !strings.Contains(popupText(m), "cannot list repositories: cannot find me-user") {
		t.Fatal("repo error not shown")
	}
}

func TestStaleRepoListIgnored(t *testing.T) {
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}}, nil)
	m = send(m, key("O"))
	m = send(m, key("enter"))
	stale := m.repoSeq
	m = send(m, key("esc"))
	m = send(m, key("j"))
	m = send(m, key("enter"))
	next, _ := m.Update(reposMsg{seq: stale, owner: "me-user", names: []string{"wrong"}})
	m = next.(model)
	if m.reposReady || len(m.repoChoices) != 0 {
		t.Fatalf("stale repo list applied: %v", m.repoChoices)
	}
}

func TestLayoutFillsTerminal(t *testing.T) {
	for _, size := range [][2]int{{160, 40}, {140, 40}, {120, 40}, {100, 30}, {90, 30}, {80, 24}} {
		for _, confirm := range []bool{false, true} {
			m := withAccounts(layoutModel(t, size[0], size[1]), manyAccounts(12), nil)
			if !confirm {
				m = send(m, key("O"))
				m = send(m, key("G"))
			}
			if confirm {
				m = send(m, key("c"))
				if !m.confirming {
					t.Fatal("c did not open the cancel prompt")
				}
			}
			lines := strings.Split(m.View(), "\n")
			if len(lines) != size[1] {
				t.Fatalf("%dx%d confirm=%v: %d lines, want %d", size[0], size[1], confirm, len(lines), size[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w != size[0] {
					t.Fatalf("%dx%d confirm=%v: line %d width %d, want %d: %q", size[0], size[1], confirm, i, w, size[0], ansi.Strip(l))
				}
			}
		}
	}
}

func TestNarrowTerminalHidesDetail(t *testing.T) {
	if strings.Contains(layoutModel(t, 80, 24).View(), "╭─Job") {
		t.Error("detail panel shown at 80 columns")
	}
	if !strings.Contains(layoutModel(t, 100, 30).View(), "╭─Job") {
		t.Error("detail panel missing at 100 columns")
	}
}

type rerunCall struct {
	runID      int64
	failedOnly bool
}

func stubRerun(t *testing.T, result error) *[]rerunCall {
	t.Helper()
	var calls []rerunCall
	prev := rerunRun
	rerunRun = func(_ context.Context, owner, repo string, runID int64, failedOnly bool) error {
		calls = append(calls, rerunCall{runID, failedOnly})
		return result
	}
	t.Cleanup(func() { rerunRun = prev })
	return &calls
}

func finishedModel(t *testing.T) model {
	t.Helper()
	m := scrollModel(t, 3)
	now := time.Now().UTC()
	m.jobs = append(m.jobs,
		scan.Job{Owner: "org-a", Repo: "repo", RunID: 2002, Status: "completed", Workflow: "Deploy", Conclusion: "success", FinishedAt: now.Add(-time.Hour)},
		scan.Job{Owner: "org-a", Repo: "repo", RunID: 2001, Status: "completed", Workflow: "CI", Conclusion: "failure", FinishedAt: now.Add(-5 * time.Minute)},
	)
	m.refresh()
	return send(m, key("3"))
}

func TestFinishedPanelListsRunsNewestFirst(t *testing.T) {
	m := finishedModel(t)
	if len(m.running)+len(m.queued) != 3 || len(m.finished) != 2 || m.finished[0].RunID != 2001 {
		t.Fatalf("running %d queued %d finished %+v", len(m.running), len(m.queued), m.finished)
	}
	view := m.View()
	for _, want := range []string{"[3] Finished · 24h (2)", "failure", "success", "Result", "R re-run"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if windowTitle(m.jobs) != windowTitle(m.jobs[:3]) {
		t.Fatal("finished runs counted in the window title")
	}
}

func TestRerunFailedRunOffersFailedAndAll(t *testing.T) {
	for _, tc := range []struct {
		key        string
		failedOnly bool
	}{{"f", true}, {"a", false}} {
		t.Run(tc.key, func(t *testing.T) {
			calls := stubRerun(t, nil)
			m := finishedModel(t)
			m, cmd := sendCmd(m, key("R"))
			if cmd != nil || !m.rerunning {
				t.Fatalf("R: rerunning=%v cmd=%v", m.rerunning, cmd)
			}
			view := m.View()
			if !strings.Contains(view, "Re-run run #2001 of repo › CI?") || !strings.Contains(view, "failed jobs") {
				t.Fatalf("prompt missing:\n%s", view)
			}
			m, cmd = sendCmd(m, key(tc.key))
			if cmd == nil || m.rerunning {
				t.Fatalf("%s: rerunning=%v cmd=%v", tc.key, m.rerunning, cmd)
			}
			msg := cmd()
			if len(*calls) != 1 || (*calls)[0] != (rerunCall{2001, tc.failedOnly}) {
				t.Fatalf("calls = %+v", *calls)
			}
			if !strings.Contains(m.fTable.View(), "re-running") {
				t.Fatal("row not marked re-running")
			}
			if m, _ = sendCmd(m, key("R")); m.rerunning || !strings.Contains(m.notice, "already requested") {
				t.Fatalf("second R while in flight: rerunning=%v notice=%q", m.rerunning, m.notice)
			}
			next, _ := m.Update(msg)
			m = next.(model)
			if !strings.Contains(m.notice, "requested") || m.noticeErr {
				t.Fatalf("notice = %q", m.notice)
			}
		})
	}
}

func TestRerunSucceededRunHasNoFailedOption(t *testing.T) {
	calls := stubRerun(t, nil)
	m := send(finishedModel(t), key("j"))
	m = send(m, key("R"))
	if strings.Contains(m.View(), "failed jobs") {
		t.Fatal("failed jobs offered for a successful run")
	}
	m, cmd := sendCmd(m, key("f"))
	if cmd != nil || m.rerunning || len(*calls) != 0 {
		t.Fatalf("f on a successful run: cmd=%v rerunning=%v calls=%v", cmd, m.rerunning, *calls)
	}
}

func TestRerunOtherKeysSendNothing(t *testing.T) {
	calls := stubRerun(t, nil)
	for _, k := range []string{"n", "y", "esc", "enter", "r"} {
		m := send(finishedModel(t), key("R"))
		m, cmd := sendCmd(m, key(k))
		if cmd != nil || m.rerunning || len(m.reruns) != 0 {
			t.Fatalf("%s: cmd=%v rerunning=%v reruns=%v", k, cmd, m.rerunning, m.reruns)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
}

func TestRerunAndCancelOnlyOnTheirPanels(t *testing.T) {
	stubRerun(t, nil)
	stubCancel(t, nil)
	m := send(scrollModel(t, 3), key("R"))
	if m.rerunning || !strings.Contains(m.notice, "finished runs") {
		t.Fatalf("R on a running job: rerunning=%v notice=%q", m.rerunning, m.notice)
	}
	m = send(finishedModel(t), key("c"))
	if m.confirming || !strings.Contains(m.notice, "already finished") {
		t.Fatalf("c on a finished run: confirming=%v notice=%q", m.confirming, m.notice)
	}
}

func TestRerunOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantNotice string
		wantMarked bool
	}{
		{"accepted", nil, "re-run of failed jobs requested", true},
		{"no permission", scan.ErrNoPermission, "no permission to re-run", false},
		{"rate limited", scan.ErrRateLimited, "rate limited", false},
		{"github refuses", errors.New("created over a month ago"), "re-run failed · created over a month ago", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := send(finishedModel(t), key("R"))
			job := m.rerunJob
			m = send(m, key("f"))
			next, _ := m.Update(rerunDoneMsg{job: job, failedOnly: true, err: tc.err})
			m = next.(model)
			if !strings.Contains(m.notice, tc.wantNotice) {
				t.Fatalf("notice = %q, want %q", m.notice, tc.wantNotice)
			}
			if m.reruns[job.RunID] != tc.wantMarked {
				t.Fatalf("marked = %v, want %v", m.reruns[job.RunID], tc.wantMarked)
			}
		})
	}
}

func TestRerunWakesRepoAndClearsWhenRunIsBack(t *testing.T) {
	stubRerun(t, nil)
	var woken []string
	prev := wakeRepo
	wakeRepo = func(_ *scan.Rounds, owner, repo string) { woken = append(woken, owner+"/"+repo) }
	t.Cleanup(func() { wakeRepo = prev })
	m := send(finishedModel(t), key("R"))
	job := m.rerunJob
	m = send(m, key("a"))
	next, _ := m.Update(rerunDoneMsg{job: job})
	m = next.(model)
	if len(woken) != 1 || woken[0] != "org-a/repo" {
		t.Fatalf("woken = %v, want org-a/repo for the next round", woken)
	}
	next, _ = m.Update(scanDoneMsg{id: m.scanID, jobs: m.jobs})
	m = next.(model)
	if !m.reruns[job.RunID] {
		t.Fatal("re-run mark dropped while the run still shows as finished")
	}
	back := append([]scan.Job{}, m.jobs[:3]...)
	back = append(back, scan.Job{Owner: "org-a", Repo: "repo", RunID: job.RunID, Status: "queued", Name: "build"})
	next, _ = m.Update(scanDoneMsg{id: m.scanID, jobs: back})
	m = next.(model)
	if m.reruns[job.RunID] {
		t.Fatal("re-run mark kept after the run is queued again")
	}
}

func TestRerunPopupFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{160, 40}, {120, 40}, {90, 30}, {80, 24}} {
		m := finishedModel(t)
		next, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = send(next.(model), key("R"))
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Fatalf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w != size[0] {
				t.Fatalf("%dx%d: line %d width %d: %q", size[0], size[1], i, w, ansi.Strip(l))
			}
		}
	}
}

func TestFilterFloatsAndAppliesWhileTyping(t *testing.T) {
	m := newTestModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = send(next.(model), key("/"))
	m = typeRunes(m, "jobA")
	if m.filter != "jobA" || len(m.runTable.Rows()) != 1 || len(m.qTable.Rows()) != 0 {
		t.Fatalf("live filter = %q, running %d queued %d", m.filter, len(m.runTable.Rows()), len(m.qTable.Rows()))
	}
	lines := strings.Split(m.View(), "\n")
	if len(lines) != 40 {
		t.Fatalf("%d lines, want 40", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 120 {
			t.Fatalf("line %d width %d: %q", i, w, ansi.Strip(l))
		}
	}
	top := ansi.Strip(lines[0] + "\n" + lines[1] + "\n" + lines[2])
	if !strings.HasPrefix(top, "╭─Filter") || !strings.Contains(top, "jobA") || !strings.Contains(top, "1 match") {
		t.Fatalf("prompt not floating at the top:\n%s", top)
	}
	bar := ansi.Strip(lines[39])
	if !strings.Contains(bar, "enter: keep filter") || strings.Contains(bar, "filter: jobA") {
		t.Fatalf("key bar while typing: %q", bar)
	}
	m = send(m, key("enter"))
	view := ansi.Strip(m.View())
	if strings.Contains(view, "╭─Filter") || !strings.Contains(view, "filter: jobA") || m.filter != "jobA" {
		t.Fatalf("after enter: filter %q view:\n%s", m.filter, view)
	}
	m = send(m, key("/"))
	m = send(m, key("esc"))
	if m.filter != "" || len(m.runTable.Rows()) != 2 || strings.Contains(ansi.Strip(m.View()), "╭─Filter") {
		t.Fatalf("esc did not clear: filter %q running %d", m.filter, len(m.runTable.Rows()))
	}
}

func TestAngleKeysJumpOnEveryPanel(t *testing.T) {
	m := finishedModel(t)
	m = send(m, key(">"))
	if m.fTable.Cursor() != len(m.finished)-1 {
		t.Fatalf("> on finished: cursor %d", m.fTable.Cursor())
	}
	m = send(m, key("<"))
	if m.fTable.Cursor() != 0 {
		t.Fatalf("< on finished: cursor %d", m.fTable.Cursor())
	}
}
