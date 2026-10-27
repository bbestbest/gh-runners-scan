package main

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"reflect"
	"regexp"
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
	for _, k := range []string{"h", "l"} {
		m := newTestModel(t)
		if m.focus != 0 {
			t.Fatalf("initial focus = %d, want 0", m.focus)
		}
		m = send(m, key(k))
		if m.focus != 1 {
			t.Errorf("focus after %s = %d, want 1", k, m.focus)
		}
		m = send(m, key(k))
		if m.focus != 0 {
			t.Errorf("focus after second %s = %d, want 0", k, m.focus)
		}
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
	for _, want := range []string{"[1] Running (2)", "[2] Queued (1)", "q: quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q", want)
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
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 42})
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

func TestPickWhileScanningWaitsForNextScan(t *testing.T) {
	m := newTestModel(t)
	stopped := false
	m.scanning = true
	m.stopScan = func() { stopped = true }
	oldID := m.scanID

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m = withAccounts(m, []scan.Account{{Login: "me-user"}, {Login: "org-b", Org: true}}, nil)
	m = send(m, key("O"))
	m = send(m, key("j"))
	m = send(m, key("enter"))
	m = withRepos(m, []string{"api"}, nil)
	m = send(m, key("G"))
	m = send(m, key("enter"))

	if stopped || m.scanID != oldID || m.opts.org != "org-c" {
		t.Fatalf("pick interrupted the running scan: stopped=%v scanID=%d org=%q", stopped, m.scanID, m.opts.org)
	}
	if m.pending == nil || m.pending.owner != "org-b" || m.pending.repo != "api" {
		t.Fatalf("pending = %+v, want org-b/api", m.pending)
	}
	if !strings.Contains(ansi.Strip(m.statusView()), "next scan: org-b/api") {
		t.Fatal("key bar does not show the queued change")
	}

	next, _ := m.Update(scanDoneMsg{id: oldID, jobs: testJobs()})
	m = next.(model)
	if len(m.jobs) != 3 || m.opts.org != "org-c" {
		t.Fatalf("the running scan's results were dropped: jobs=%d org=%q", len(m.jobs), m.opts.org)
	}

	m.nextIn = 1
	next, cmd := m.Update(tickMsg(time.Now()))
	m = next.(model)
	if cmd == nil || !m.scanning || m.scanID == oldID || m.opts.org != "org-b" || m.opts.repo != "api" || m.pending != nil || len(m.jobs) != 0 {
		t.Fatalf("next scan did not switch: scanning=%v org=%q repo=%q pending=%v jobs=%d", m.scanning, m.opts.org, m.opts.repo, m.pending, len(m.jobs))
	}
}

func TestRescanKeyAppliesQueuedPick(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}, {Login: "org-c", Org: true}}, nil)
	m.scanning = false
	m = send(m, key("O"))
	m = send(m, key("k"))
	m = send(m, key("enter"))
	m = withRepos(m, []string{"dots"}, nil)
	m = send(m, key("enter"))
	if m.scanning || m.pending == nil {
		t.Fatalf("pick started a scan at once: scanning=%v pending=%v", m.scanning, m.pending)
	}
	m = send(m, key("r"))
	if !m.scanning || m.opts.org != "me-user" || m.opts.repo != "" || m.pending != nil {
		t.Fatalf("r: scanning=%v org=%q repo=%q pending=%v", m.scanning, m.opts.org, m.opts.repo, m.pending)
	}
}

func TestFirstPickWithoutOwnerScansAtOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withAccounts(newModel(options{interval: 30}), []scan.Account{{Login: "me-user"}}, nil)
	m = send(m, key("enter"))
	m = withRepos(m, nil, nil)
	m, cmd := sendCmd(m, key("enter"))
	if cmd == nil || !m.scanning || m.opts.org != "me-user" || m.pending != nil {
		t.Fatalf("scanning=%v org=%q pending=%v", m.scanning, m.opts.org, m.pending)
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

func TestScanDoneSavesCache(t *testing.T) {
	calls := stubSaveCache(t)
	m := newTestModel(t)
	_, cmd := m.Update(scanDoneMsg{id: m.scanID, jobs: testJobs()})
	if cmd == nil {
		t.Fatal("scanDoneMsg returned no save command")
	}
	cmd()
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

func TestRepoPanelCapsRows(t *testing.T) {
	var jobs []scan.Job
	for i := 0; i < maxRepoRows+3; i++ {
		jobs = append(jobs, scan.Job{Status: "queued", Repo: "repo-" + strconv.Itoa(i), Name: "j"})
	}
	m := layoutModel(t, 140, 40)
	m.jobs = jobs
	m.refresh()
	lines := m.repoLines(60)
	if len(lines) != maxRepoRows {
		t.Fatalf("repo lines = %d, want %d", len(lines), maxRepoRows)
	}
	if !strings.Contains(lines[len(lines)-1], "+4 more") {
		t.Fatalf("last repo line = %q, want +4 more", lines[len(lines)-1])
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
	if view := popupText(m); !strings.Contains(view, "Pick account") || !strings.Contains(view, "loading accounts") || !strings.Contains(view, "esc quit") {
		t.Fatal("startup picker missing title, loading line or quit hint")
	}
	m = withAccounts(m, []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}}, nil)
	if view := popupText(m); !strings.Contains(view, " > me-user  you") || !strings.Contains(view, "org-a    org") {
		t.Fatal("accounts not listed with the cursor on the first one")
	}
	if _, cmd := sendCmd(m, key("esc")); cmd == nil {
		t.Fatal("esc with no owner should quit")
	}
}

func TestPickerAccountThenRepoScansThatRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var asked []string
	prev := listRepoNames
	listRepoNames = func(_ context.Context, owner string, days int) ([]string, error) {
		asked = append(asked, owner+"/"+strconv.Itoa(days))
		return []string{"api", "web"}, nil
	}
	t.Cleanup(func() { listRepoNames = prev })

	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}, {Login: "org-a", Org: true}, {Login: "org-c", Org: true}}, nil)
	runCursor := m.runTable.Cursor()
	m = send(m, key("O"))
	if !m.picking || m.accountCursor != 2 {
		t.Fatalf("picking=%v cursor=%d, want open on the current owner org-c", m.picking, m.accountCursor)
	}
	m = send(m, key("k"))
	if m.accountCursor != 1 || m.runTable.Cursor() != runCursor {
		t.Fatalf("k moved account cursor %d and table cursor %d", m.accountCursor, m.runTable.Cursor())
	}
	m, cmd := sendCmd(m, key("enter"))
	if m.pickStep != 1 || m.pickOwner != "org-a" || cmd == nil {
		t.Fatalf("enter on an account: step=%d owner=%q cmd=%v", m.pickStep, m.pickOwner, cmd != nil)
	}
	if view := popupText(m); !strings.Contains(view, "Pick repo · org-a") || !strings.Contains(view, "loading repositories") {
		t.Fatal("repo step not shown while loading")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if len(asked) != 1 || asked[0] != "org-a/"+strconv.Itoa(m.opts.days) {
		t.Fatalf("repos requested = %v", asked)
	}
	if view := popupText(m); !strings.Contains(view, " > All repositories") || !strings.Contains(view, "   web") {
		t.Fatal("repo list not shown with All repositories first")
	}
	m = send(m, key("G"))
	m = send(m, key("enter"))
	if m.picking || m.pending == nil || m.pending.owner != "org-a" || m.pending.repo != "web" {
		t.Fatalf("pick: picking=%v pending=%+v", m.picking, m.pending)
	}
	if got := config.Load().Org; got != "org-a" {
		t.Fatalf("saved org = %q, want org-a", got)
	}
}

func TestPickerAllRepositoriesClearsRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withAccounts(newTestModel(t), []scan.Account{{Login: "me-user"}, {Login: "org-c", Org: true}}, nil)
	m = send(m, key("O"))
	m = send(m, key("enter"))
	m = withRepos(m, []string{"repo-a"}, nil)
	m = send(m, key("enter"))
	if m.picking || m.pending == nil || m.pending.owner != "org-c" || m.pending.repo != "" {
		t.Fatalf("pending = %+v, want org-c with all repositories", m.pending)
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
