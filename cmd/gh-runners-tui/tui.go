package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"gh-runners/internal/config"
	"gh-runners/internal/scan"
)

var (
	colorAccent = lipgloss.Color("6")
	colorGreen  = lipgloss.Color("2")
	colorYellow = lipgloss.Color("3")
	colorRed    = lipgloss.Color("1")
	colorDim    = lipgloss.Color("8")
	colorCyan   = lipgloss.Color("14")
)

var (
	dimStyle    = lipgloss.NewStyle().Foreground(colorDim)
	accentStyle = lipgloss.NewStyle().Foreground(colorAccent)
	boldStyle   = lipgloss.NewStyle().Bold(true)
	greenStyle  = lipgloss.NewStyle().Foreground(colorGreen)
	yellowStyle = lipgloss.NewStyle().Foreground(colorYellow)
	redStyle    = lipgloss.NewStyle().Foreground(colorRed)
	hlStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)
)

var keyHints = [][2]string{
	{"enter", "open"},
	{"c", "cancel run"},
	{"/", "filter"},
	{"h/l", "panel"},
	{"r", "rescan"},
	{"O", "choose"},
	{"u", "usage"},
	{"q", "quit"},
}

var runningCols = []string{"Runner", "Repo", "Workflow", "Branch", "Job", "Elapsed"}
var queuedCols = []string{"Waiting", "Repo", "Workflow", "Branch", "Job", "Labels"}

var runningPanelCols = []string{"Runner", "Repo", "Job", "Elapsed"}

var queuedPanelCols = []string{"Wait", "Repo", "Job", "Labels"}

type repoCount struct {
	name    string
	running int
	queued  int
}

type progressMsg struct {
	done  int
	total int
}

type scanDoneMsg struct {
	id       int
	jobs     []scan.Job
	err      error
	requests int64
	free     int64
}

type scanCost struct {
	requests int64
	free     int64
}

type cancelDoneMsg struct {
	job scan.Job
	err error
}

type tickMsg time.Time

type options struct {
	org      string
	targets  []scan.Target
	label    string
	days     int
	interval int
	repo     string
}

type pick struct {
	targets []scan.Target
}

type choice struct {
	all   bool
	repos map[string]bool
}

type pickRow struct {
	login string
	kind  string
}

type model struct {
	opts options

	jobs    []scan.Job
	running []scan.Job
	queued  []scan.Job
	repos   []repoCount

	runTable  table.Model
	qTable    table.Model
	runStyles [][]lipgloss.Style
	qStyles   [][]lipgloss.Style
	focus     int

	filterInput  textinput.Model
	filterActive bool
	filter       string

	accounts      []scan.Account
	accountCursor int
	accountsErr   error
	accountsReady bool

	picking     bool
	pickStep    int
	pickOwner   string
	repoChoices []string
	repoCursor  int
	reposReady  bool
	reposErr    error
	repoSeq     int
	pending     *pick
	sel         map[string]*choice
	selNote     string

	spin      spinner.Model
	scanning  bool
	scanID    int
	stopScan  context.CancelFunc
	progress  progressMsg
	repoCount int
	lastScan  string
	nextIn    int
	scanErr   error
	saveErr   error

	usageActive bool
	lastCost    *scanCost

	cancels    map[int64]bool
	confirming bool
	confirmJob scan.Job
	pendingG   bool
	notice     string
	noticeErr  bool

	width  int
	height int
}

func newModel(opts options) model {
	if len(opts.targets) == 0 && opts.org != "" {
		opts.targets = []scan.Target{{Owner: opts.org, All: true}}
		if opts.repo != "" {
			if owner, name := scan.SplitRepo(opts.org, opts.repo); name != "" {
				opts.targets = []scan.Target{{Owner: owner, Repos: []string{name}}}
			}
		}
	}
	m := model{
		opts:      opts,
		nextIn:    opts.interval,
		width:     120,
		height:    40,
		scanning:  len(opts.targets) > 0,
		progress:  progressMsg{0, scan.TotalUnknown},
		repoCount: scan.TotalUnknown,
		cancels:   map[int64]bool{},
	}
	if len(opts.targets) == 0 {
		m.openPicker()
	}

	m.runTable = newTable(runningPanelCols)
	m.qTable = newTable(queuedPanelCols)

	in := textinput.New()
	in.Placeholder = "filter"
	in.Prompt = "/ "
	m.filterInput = in

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = accentStyle
	m.spin = s

	return m
}

func newTable(cols []string) table.Model {
	columns := make([]table.Column, len(cols))
	for i, c := range cols {
		columns[i] = table.Column{Title: c, Width: 10}
	}
	t := table.New(table.WithColumns(columns), table.WithFocused(true))
	t.SetStyles(tableStyles())
	return t
}

func tableStyles() table.Styles {
	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).Bold(true).Foreground(colorDim)
	st.Selected = st.Selected.Bold(true).Foreground(colorAccent).Background(lipgloss.Color("0"))
	return st
}

func (m model) hasTargets() bool {
	return len(m.opts.targets) > 0
}

func (m model) Init() tea.Cmd {
	if !m.hasTargets() {
		return tea.Batch(tickCmd(), m.spin.Tick, accountsCmd)
	}
	return tea.Batch(scanCmd(context.Background(), m.opts, 0), tickCmd(), m.spin.Tick, accountsCmd)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) startScan() tea.Cmd {
	if m.scanning {
		return nil
	}
	if m.pending != nil {
		m.applyPending()
	}
	if !m.hasTargets() {
		return nil
	}
	return m.beginScan()
}

func (m *model) applyPending() {
	p := m.pending
	m.pending = nil
	m.opts.targets = p.targets
	m.opts.org = p.targets[0].Owner
	m.opts.repo = ""
	m.jobs = nil
	m.scanErr = nil
	m.lastScan = ""
	m.lastCost = nil
	m.repoCount = scan.TotalUnknown
	m.refresh()
}

func (m *model) beginScan() tea.Cmd {
	if m.stopScan != nil {
		m.stopScan()
	}
	ctx, stop := context.WithCancel(context.Background())
	m.stopScan = stop
	m.scanID++
	m.scanning = true
	m.progress = progressMsg{0, scan.TotalUnknown}
	return scanCmd(ctx, m.opts, m.scanID)
}

var cancelRun = scan.CancelRun

var usage = scan.Usage

var saveCache = scan.SaveCache

var listAccounts = scan.Accounts

var listRepoNames = scan.Repos

type reposMsg struct {
	seq   int
	owner string
	names []string
	err   error
}

func reposCmd(seq int, owner string, days int) tea.Cmd {
	return func() tea.Msg {
		names, err := listRepoNames(context.Background(), owner, days)
		return reposMsg{seq: seq, owner: owner, names: names, err: err}
	}
}

type accountsMsg struct {
	list []scan.Account
	err  error
}

func accountsCmd() tea.Msg {
	list, err := listAccounts(context.Background())
	return accountsMsg{list: list, err: err}
}

func saveCacheCmd() tea.Msg {
	saveCache()
	return nil
}

func quitCmd() tea.Cmd {
	return func() tea.Msg {
		saveCache()
		return tea.QuitMsg{}
	}
}

func cancelCmd(j scan.Job) tea.Cmd {
	return func() tea.Msg {
		return cancelDoneMsg{job: j, err: cancelRun(context.Background(), j.Owner, j.Repo, j.RunID)}
	}
}

func scanCmd(ctx context.Context, opts options, id int) tea.Cmd {
	return func() tea.Msg {
		before := usage()
		jobs, scanErr := scanLabeled(ctx, opts, func(done, total int) {
			if program != nil {
				program.Send(progressMsg{done, total})
			}
		})
		after := usage()
		return scanDoneMsg{
			id:       id,
			jobs:     jobs,
			err:      scanErr,
			requests: after.Requests - before.Requests,
			free:     after.NotModified - before.NotModified,
		}
	}
}

var program *tea.Program

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case accountsMsg:
		m.accounts = msg.list
		m.accountsErr = msg.err
		m.accountsReady = true
		m.accountCursor = m.currentAccount()
		return m, nil

	case reposMsg:
		if msg.seq != m.repoSeq {
			return m, nil
		}
		m.repoChoices = msg.names
		if c := m.sel[strings.ToLower(msg.owner)]; c != nil {
			var extra []string
			for name := range c.repos {
				if !slices.Contains(msg.names, name) {
					extra = append(extra, name)
				}
			}
			sort.Strings(extra)
			m.repoChoices = append(append([]string{}, msg.names...), extra...)
		}
		m.reposErr = msg.err
		m.reposReady = true
		return m, nil

	case progressMsg:
		m.progress = msg
		m.repoCount = msg.total
		return m, nil

	case scanDoneMsg:
		if msg.id != m.scanID {
			return m, nil
		}
		m.scanning = false
		m.jobs = msg.jobs
		m.scanErr = msg.err
		m.lastCost = &scanCost{requests: msg.requests, free: msg.free}
		m.lastScan = clock(time.Now(), "15:04:05")
		m.nextIn = m.opts.interval
		m.refresh()
		return m, saveCacheCmd

	case cancelDoneMsg:
		label := msg.job.Workflow + " (" + msg.job.Repo + ")"
		m.noticeErr = true
		switch {
		case msg.err == nil:
			m.noticeErr = false
			m.notice = "cancel accepted · " + label
		case errors.Is(msg.err, scan.ErrRunFinished):
			m.notice = "run already finished · " + label
		case errors.Is(msg.err, scan.ErrNoCancelPermission):
			delete(m.cancels, msg.job.RunID)
			m.notice = "no permission to cancel · token needs actions:write"
		case errors.Is(msg.err, scan.ErrRateLimited):
			delete(m.cancels, msg.job.RunID)
			m.notice = "rate limited · cancel not sent"
		default:
			delete(m.cancels, msg.job.RunID)
			m.notice = "cancel failed · " + msg.err.Error()
		}
		m.refresh()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tickMsg:
		if !m.scanning {
			m.nextIn--
			if m.nextIn <= 0 {
				m.nextIn = m.opts.interval
				return m, tea.Batch(m.startScan(), tickCmd())
			}
		}
		m.refresh()
		return m, tickCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picking {
		return m.pickerKey(msg)
	}
	if m.filterActive {
		switch msg.String() {
		case "esc":
			m.filterActive = false
			m.filter = ""
			m.filterInput.SetValue("")
			m.filterInput.Blur()
			m.refresh()
			return m, nil
		case "enter":
			m.filter = strings.TrimSpace(m.filterInput.Value())
			m.filterActive = false
			m.filterInput.Blur()
			m.refresh()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	if m.usageActive {
		switch msg.String() {
		case "ctrl+c":
			return m, quitCmd()
		case "esc", "u", "q":
			m.usageActive = false
		}
		return m, nil
	}

	m.notice = ""

	if m.confirming {
		m.confirming = false
		switch msg.String() {
		case "ctrl+c":
			return m, quitCmd()
		case "y":
			job := m.confirmJob
			if m.cancels[job.RunID] {
				return m, nil
			}
			m.cancels[job.RunID] = true
			m.refresh()
			return m, cancelCmd(job)
		}
		return m, nil
	}

	armed := m.pendingG
	m.pendingG = false

	switch msg.String() {
	case "q", "ctrl+c":
		return m, quitCmd()
	case "c":
		job, ok := m.selected()
		if !ok || job.RunID == 0 {
			return m, nil
		}
		if m.cancels[job.RunID] {
			m.notice = "cancel already requested · " + job.Workflow + " (" + job.Repo + ")"
			m.noticeErr = true
			return m, nil
		}
		m.confirming = true
		m.confirmJob = job
		return m, nil
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.filterInput.SetValue("")
			m.refresh()
		}
		return m, nil
	case "/":
		m.filterActive = true
		m.filterInput.Focus()
		m.layout()
		return m, textinput.Blink
	case "u":
		m.usageActive = true
		return m, nil
	case "r":
		m.nextIn = m.opts.interval
		return m, m.startScan()
	case "O":
		m.openPicker()
		if m.accountsErr != nil {
			m.accountsReady = false
			m.accountsErr = nil
			return m, accountsCmd
		}
		return m, nil
	case "h", "l":
		m.focus = 1 - m.focus
		return m, nil
	case "1":
		m.focus = 0
		return m, nil
	case "2":
		m.focus = 1
		return m, nil
	case "enter", "o":
		m.open()
		return m, nil
	case "j", "down":
		m.moveCursor(1)
		return m, nil
	case "k", "up":
		m.moveCursor(-1)
		return m, nil
	case "g":
		if armed {
			m.gotoEdge(false)
			return m, nil
		}
		m.pendingG = true
		return m, nil
	case "home":
		m.gotoEdge(false)
		return m, nil
	case "G", "end":
		m.gotoEdge(true)
		return m, nil
	case "pgup", "ctrl+b":
		m.moveCursor(-m.page())
		return m, nil
	case "pgdown", "ctrl+f":
		m.moveCursor(m.page())
		return m, nil
	case "ctrl+u":
		m.moveCursor(-m.halfPage())
		return m, nil
	case "ctrl+d":
		m.moveCursor(m.halfPage())
		return m, nil
	case "H":
		m.moveToVisible(0)
		return m, nil
	case "M":
		m.moveToVisible(1)
		return m, nil
	case "L":
		m.moveToVisible(2)
		return m, nil
	}

	return m, nil
}

func (m *model) openPicker() {
	m.picking = true
	m.pickStep = 0
	m.selNote = ""
	m.sel = map[string]*choice{}
	for _, t := range m.opts.targets {
		c := &choice{all: t.All, repos: map[string]bool{}}
		for _, r := range t.Repos {
			c.repos[r] = true
		}
		m.sel[strings.ToLower(t.Owner)] = c
	}
	m.accountCursor = m.currentAccount()
}

func (m model) pickAccounts() []pickRow {
	rows := make([]pickRow, 0, len(m.accounts)+len(m.opts.targets))
	seen := map[string]bool{}
	for _, a := range m.accounts {
		kind := "you"
		if a.Org {
			kind = "org"
		}
		rows = append(rows, pickRow{a.Login, kind})
		seen[strings.ToLower(a.Login)] = true
	}
	for _, t := range m.opts.targets {
		if !seen[strings.ToLower(t.Owner)] {
			rows = append(rows, pickRow{t.Owner, "saved"})
			seen[strings.ToLower(t.Owner)] = true
		}
	}
	return rows
}

func (m model) currentAccount() int {
	if len(m.opts.targets) == 0 {
		return 0
	}
	for i, r := range m.pickAccounts() {
		if strings.EqualFold(r.login, m.opts.targets[0].Owner) {
			return i
		}
	}
	return 0
}

func (m model) pickerRows() int {
	if m.pickStep == 0 {
		return len(m.pickAccounts())
	}
	return len(m.repoChoices) + 1
}

func (m *model) movePick(delta int) {
	if m.pickStep == 0 {
		m.accountCursor = clamp(m.accountCursor+delta, len(m.pickAccounts()))
		return
	}
	m.repoCursor = clamp(m.repoCursor+delta, len(m.repoChoices)+1)
}

func (m *model) toggleAccount() {
	rows := m.pickAccounts()
	if m.accountCursor < 0 || m.accountCursor >= len(rows) {
		return
	}
	key := strings.ToLower(rows[m.accountCursor].login)
	if m.sel[key] != nil {
		delete(m.sel, key)
		return
	}
	m.sel[key] = &choice{all: true, repos: map[string]bool{}}
}

func (m *model) toggleRepo() {
	key := strings.ToLower(m.pickOwner)
	c := m.sel[key]
	if c == nil {
		c = &choice{repos: map[string]bool{}}
		m.sel[key] = c
	}
	if m.repoCursor == 0 {
		if c.all {
			delete(m.sel, key)
			return
		}
		c.all = true
		c.repos = map[string]bool{}
		return
	}
	if m.repoCursor > len(m.repoChoices) {
		return
	}
	name := m.repoChoices[m.repoCursor-1]
	switch {
	case c.all:
		c.all = false
		c.repos = map[string]bool{}
		for _, r := range m.repoChoices {
			if r != name {
				c.repos[r] = true
			}
		}
	case c.repos[name]:
		delete(c.repos, name)
	default:
		c.repos[name] = true
	}
	if !c.all && len(c.repos) == 0 {
		delete(m.sel, key)
	}
}

func (m model) pickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.selNote = ""
	switch msg.String() {
	case "ctrl+c":
		return m, quitCmd()
	case "esc":
		if m.pickStep == 1 {
			m.pickStep = 0
			return m, nil
		}
		if !m.hasTargets() {
			return m, quitCmd()
		}
		m.picking = false
		return m, nil
	case "j", "down":
		m.movePick(1)
	case "k", "up":
		m.movePick(-1)
	case "g", "home":
		m.movePick(-m.pickerRows())
	case "G", "end":
		m.movePick(m.pickerRows())
	case " ":
		if m.pickStep == 0 {
			m.toggleAccount()
		} else {
			m.toggleRepo()
		}
	case "s":
		if m.pickStep == 0 {
			return m.saveSelection()
		}
	case "enter":
		if m.pickStep == 1 {
			m.pickStep = 0
			return m, nil
		}
		rows := m.pickAccounts()
		if m.accountCursor < 0 || m.accountCursor >= len(rows) {
			return m, nil
		}
		m.pickOwner = rows[m.accountCursor].login
		m.pickStep = 1
		m.repoChoices = nil
		m.reposReady = false
		m.reposErr = nil
		m.repoCursor = 0
		m.repoSeq++
		return m, reposCmd(m.repoSeq, m.pickOwner, m.opts.days)
	}
	return m, nil
}

func (m model) selection() []scan.Target {
	var targets []scan.Target
	for _, r := range m.pickAccounts() {
		c := m.sel[strings.ToLower(r.login)]
		if c == nil {
			continue
		}
		t := scan.Target{Owner: r.login, All: c.all}
		if !c.all {
			for name := range c.repos {
				t.Repos = append(t.Repos, name)
			}
			sort.Strings(t.Repos)
		}
		targets = append(targets, t)
	}
	return targets
}

func (m model) saveSelection() (tea.Model, tea.Cmd) {
	targets := m.selection()
	if len(targets) == 0 {
		m.selNote = "select at least one account or repo"
		return m, nil
	}
	m.picking = false
	m.pickStep = 0
	m.saveErr = config.SetScan(scan.FormatTargets(targets))
	if strings.Join(scan.FormatTargets(targets), ",") == strings.Join(scan.FormatTargets(m.opts.targets), ",") {
		m.pending = nil
		return m, nil
	}
	m.pending = &pick{targets: targets}
	if !m.hasTargets() {
		m.nextIn = m.opts.interval
		return m, m.startScan()
	}
	return m, nil
}

func (m model) repoName(j scan.Job) string {
	if scan.MultiOwner(m.opts.targets) && j.Owner != "" {
		return j.Owner + "/" + j.Repo
	}
	return j.Repo
}

func (m *model) focusedTable() *table.Model {
	if m.focus == 1 {
		return &m.qTable
	}
	return &m.runTable
}

func (m *model) page() int {
	if h := m.focusedTable().Height(); h > 1 {
		return h
	}
	return 1
}

func (m *model) halfPage() int {
	if h := m.page() / 2; h > 1 {
		return h
	}
	return 1
}

func visibleRows(t table.Model) (int, int) {
	rows := make([]table.Row, len(t.Rows()))
	for i := range rows {
		rows[i] = table.Row{strconv.Itoa(i)}
	}
	t.SetRows(rows)
	t.SetColumns([]table.Column{{Title: "i", Width: 8}})
	t.SetStyles(table.Styles{})

	first, last := -1, -1
	for _, line := range strings.Split(t.View(), "\n")[1:] {
		i, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	return first, last
}

func (m *model) moveToVisible(pos int) {
	t := m.focusedTable()
	if len(t.Rows()) == 0 {
		return
	}
	first, last := visibleRows(*t)
	if first < 0 {
		return
	}
	m.moveCursor(first + (last-first)*pos/2 - t.Cursor())
}

func (m *model) moveCursor(delta int) {
	t := m.focusedTable()
	if len(t.Rows()) == 0 || delta == 0 {
		return
	}
	if delta < 0 {
		t.MoveUp(-delta)
	} else {
		t.MoveDown(delta)
	}
	m.refresh()
}

func (m *model) gotoEdge(bottom bool) {
	t := m.focusedTable()
	if len(t.Rows()) == 0 {
		return
	}
	if bottom {
		t.GotoBottom()
	} else {
		t.GotoTop()
	}
	m.refresh()
}

func (m *model) selected() (scan.Job, bool) {
	jobs := m.running
	cursor := m.runTable.Cursor()
	if m.focus == 1 {
		jobs = m.queued
		cursor = m.qTable.Cursor()
	}
	if cursor < 0 || cursor >= len(jobs) {
		return scan.Job{}, false
	}
	return jobs[cursor], true
}

func (m *model) open() {
	jobs := m.running
	cursor := m.runTable.Cursor()
	if m.focus == 1 {
		jobs = m.queued
		cursor = m.qTable.Cursor()
	}
	if cursor < 0 || cursor >= len(jobs) {
		return
	}
	if url := jobs[cursor].URL; url != "" {
		exec.Command("open", url).Start()
	}
}

func (m *model) refresh() {
	runCursor := m.runTable.Cursor()
	qCursor := m.qTable.Cursor()

	m.running, m.queued = scan.SplitJobs(scan.FilterJobs(m.jobs, m.filter))
	m.repos = repoCounts(m.running, m.queued, m.repoName)

	now := time.Now().UTC()
	cols := m.runTable.Columns()
	runRows := make([]table.Row, 0, len(m.running))
	runStyles := make([][]lipgloss.Style, 0, len(m.running))
	runSel := clamp(runCursor, len(m.running))
	for i, j := range m.running {
		elapsed := "-"
		if !j.StartedAt.IsZero() {
			elapsed = fmtElapsed(now.Sub(j.StartedAt))
		}
		runnerStyle := greenStyle
		rest := m.rowStyle(j.Repo)
		elapsedStyle := rest
		if m.cancelling(j) {
			elapsed = "cancelling"
			elapsedStyle = yellowStyle
		}
		if i == runSel {
			runnerStyle, rest, elapsedStyle = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()
		}
		runRows = append(runRows, table.Row{
			cell(j.Runner, cols, 0),
			cell(m.repoName(j), cols, 1),
			cell(j.Name, cols, 2),
			cell(elapsed, cols, 3),
		})
		runStyles = append(runStyles, []lipgloss.Style{runnerStyle, rest, rest, elapsedStyle})
	}

	qcols := m.qTable.Columns()
	qRows := make([]table.Row, 0, len(m.queued))
	qStyles := make([][]lipgloss.Style, 0, len(m.queued))
	qSel := clamp(qCursor, len(m.queued))
	for i, j := range m.queued {
		wait := "-"
		waitStyle := m.rowStyle(j.Repo)
		if !j.CreatedAt.IsZero() {
			mins := int(now.Sub(j.CreatedAt).Minutes())
			wait = fmt.Sprintf("%dm", mins)
			waitStyle = waitColor(mins, waitStyle)
		}
		if m.cancelling(j) {
			wait = "cancelling"
			waitStyle = yellowStyle
		}
		rest := m.rowStyle(j.Repo)
		if i == qSel {
			waitStyle, rest = lipgloss.NewStyle(), lipgloss.NewStyle()
		}
		qRows = append(qRows, table.Row{
			cell(wait, qcols, 0),
			cell(m.repoName(j), qcols, 1),
			cell(j.Name, qcols, 2),
			cell(j.Labels, qcols, 3),
		})
		qStyles = append(qStyles, []lipgloss.Style{waitStyle, rest, rest, rest})
	}

	m.runStyles, m.qStyles = runStyles, qStyles
	setRows(&m.runTable, runRows, runCursor)
	setRows(&m.qTable, qRows, qCursor)
	m.layout()
}

func waitColor(mins int, base lipgloss.Style) lipgloss.Style {
	if mins >= 60 {
		return redStyle
	}
	if mins >= 30 {
		return yellowStyle
	}
	return base
}

func repoCounts(running, queued []scan.Job, name func(scan.Job) string) []repoCount {
	idx := map[string]int{}
	var out []repoCount
	add := func(repo string, isRunning bool) {
		i, ok := idx[repo]
		if !ok {
			i = len(out)
			idx[repo] = i
			out = append(out, repoCount{name: repo})
		}
		if isRunning {
			out[i].running++
		} else {
			out[i].queued++
		}
	}
	for _, j := range running {
		add(name(j), true)
	}
	for _, j := range queued {
		add(name(j), false)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].running != out[b].running {
			return out[a].running > out[b].running
		}
		if out[a].queued != out[b].queued {
			return out[a].queued > out[b].queued
		}
		return out[a].name < out[b].name
	})
	return out
}

func busyRunners(q scan.Job, running []scan.Job) []scan.Job {
	want := labelSet(q.Labels)
	if len(want) == 0 {
		return nil
	}
	var out []scan.Job
	for _, r := range running {
		have := labelSet(r.Labels)
		match := true
		for l := range want {
			if !have[l] {
				match = false
				break
			}
		}
		if match {
			out = append(out, r)
		}
	}
	return out
}

func labelSet(labels string) map[string]bool {
	set := map[string]bool{}
	for _, l := range strings.Split(labels, ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			set[l] = true
		}
	}
	return set
}

func setRows(t *table.Model, rows []table.Row, cursor int) {
	t.SetRows(rows)
	t.SetCursor(clamp(cursor, len(rows)))
	if cursor >= len(rows) {
		reseat(t)
	}
}

func reseat(t *table.Model) {
	if len(t.Rows()) == 0 {
		return
	}
	cursor := t.Cursor()
	t.GotoTop()
	t.MoveDown(cursor)
}

func (m model) cancelling(j scan.Job) bool {
	return j.RunID != 0 && m.cancels[j.RunID]
}

func (m model) rowStyle(repo string) lipgloss.Style {
	if m.opts.repo == "" {
		return lipgloss.NewStyle()
	}
	if _, name := scan.SplitRepo(m.opts.org, m.opts.repo); repo == name {
		return hlStyle
	}
	return lipgloss.NewStyle()
}

func cell(text string, cols []table.Column, i int) string {
	if i < len(cols) {
		text = truncate(text, cols[i].Width)
	}
	return text
}

func (m model) tableLines(focus int) []string {
	if focus == 1 {
		return tableView(m.qTable, m.qStyles)
	}
	return tableView(m.runTable, m.runStyles)
}

func tableView(t table.Model, styles [][]lipgloss.Style) []string {
	lines := strings.Split(t.View(), "\n")
	first, last := visibleRows(t)
	head := len(lines) - t.Height()
	if first < 0 || head < 0 {
		return lines
	}
	st := tableStyles()
	cols := t.Columns()
	rows := t.Rows()
	for r := first; r <= last && head+r-first < len(lines); r++ {
		cells := make([]string, 0, len(cols))
		for i, c := range cols {
			if c.Width <= 0 || i >= len(rows[r]) {
				continue
			}
			text := truncate(rows[r][i], c.Width)
			if r < len(styles) && i < len(styles[r]) {
				text = styles[r][i].Render(text)
			}
			cells = append(cells, st.Cell.Render(lipgloss.NewStyle().Width(c.Width).MaxWidth(c.Width).Inline(true).Render(text)))
		}
		row := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
		if r == t.Cursor() {
			row = st.Selected.Render(row)
		}
		lines[head+r-first] = row
	}
	return lines
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

func clamp(cursor, n int) int {
	if n == 0 {
		return 0
	}
	if cursor < 0 {
		return 0
	}
	if cursor > n-1 {
		return n - 1
	}
	return cursor
}

func fmtElapsed(d time.Duration) string {
	total := int(d.Seconds())
	if total < 0 {
		total = 0
	}
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func (m *model) layout() {
	left, _ := m.columns()
	inner := left - 2

	m.runTable.SetColumns(columnWidths(runningPanelCols, inner))
	m.qTable.SetColumns(columnWidths(queuedPanelCols, inner))

	runBox, qBox := m.listHeights()
	setHeight(&m.runTable, tableHeight(runBox))
	setHeight(&m.qTable, tableHeight(qBox))
	m.runTable.SetWidth(inner)
	m.qTable.SetWidth(inner)
}

func (m model) columns() (int, int) {
	left := m.width * 40 / 100
	if left < minListWidth {
		left = minListWidth
	}
	right := m.width - left
	if right < minDetailWidth {
		return m.width, 0
	}
	return left, right
}

func (m model) bodyHeight() int {
	h := m.height - keybarHeight
	if h < minBodyHeight {
		h = minBodyHeight
	}
	return h
}

func (m model) reposHeight() int {
	n := len(m.repos)
	if n < 1 {
		n = 1
	}
	if n > maxRepoRows {
		n = maxRepoRows
	}
	return n + 2
}

func (m model) listHeights() (int, int) {
	rest := m.bodyHeight() - statusPanelHeight - m.reposHeight()
	run := rest / 2
	queued := rest - run
	if run < minListBox {
		run = minListBox
	}
	if queued < minListBox {
		queued = minListBox
	}
	return run, queued
}

func setHeight(t *table.Model, h int) {
	before := t.Height()
	t.SetHeight(h)
	if t.Height() != before {
		reseat(t)
	}
}

func tableHeight(box int) int {
	h := box - 2
	if h < 2 {
		h = 2
	}
	return h
}

func columnWidths(cols []string, inner int) []table.Column {
	avail := inner - 2 - (len(cols)-1)*2
	if avail < len(cols)*3 {
		avail = len(cols) * 3
	}
	units := 0
	for _, c := range cols {
		units += colUnits(c)
	}
	widths := make([]table.Column, len(cols))
	used := 0
	for i, c := range cols {
		w := avail * colUnits(c) / units
		if w < colMin(c) {
			w = colMin(c)
		}
		if i == len(cols)-1 {
			w = avail - used
			if w < colMin(c) {
				w = colMin(c)
			}
		}
		used += w
		widths[i] = table.Column{Title: c, Width: w}
	}
	return widths
}

func colMin(name string) int {
	if name == "Elapsed" || name == "Wait" {
		return len("cancelling")
	}
	return 5
}

func colUnits(name string) int {
	switch name {
	case "Branch", "Job", "Labels":
		return 3
	case "Runner", "Repo", "Elapsed":
		return 2
	}
	return 1
}

const keybarHeight = 1

const statusPanelHeight = 5

const maxPickRows = 10

const maxRepoRows = 6

const minListWidth = 50

const minDetailWidth = 40

const minBodyHeight = 16

const minListBox = 5

func (m model) View() string {
	return m.bodyView() + "\n" + m.statusView()
}

func (m model) bodyView() string {
	if m.usageActive {
		return m.usageOverlayView()
	}

	left, right := m.columns()
	runH, qH := m.listHeights()
	col := lipgloss.JoinVertical(lipgloss.Left,
		panel("Status", "", false, left, statusPanelHeight, m.statusPanelLines(left-2)),
		panel("Repos", m.reposFooter(), false, left, m.reposHeight(), m.repoLines(left-2)),
		panel(fmt.Sprintf("[1] Running (%d)", len(m.running)), countFooter(m.runTable), m.focus == 0, left, runH, m.tableLines(0)),
		panel(fmt.Sprintf("[2] Queued (%d)", len(m.queued)), countFooter(m.qTable), m.focus == 1, left, qH, m.tableLines(1)),
	)
	body := col
	if right > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, col, m.detailPanel(right, m.bodyHeight()))
	}
	if m.picking {
		body = overlay(body, m.pickerPopup(), m.width)
	}
	if m.confirming {
		body = overlay(body, m.cancelPopup(), m.width)
	}
	return body
}

func panel(title, footer string, focused bool, w, h int, body []string) string {
	b := lipgloss.RoundedBorder()
	bs, ts := dimStyle, boldStyle
	if focused {
		bs, ts = greenStyle, greenStyle.Bold(true)
	}
	inner := w - 2

	label := ansi.Truncate(title, inner-1, "…")
	fill := inner - 1 - ansi.StringWidth(label)
	if fill < 0 {
		fill = 0
	}
	rows := []string{bs.Render(b.TopLeft+b.Top) + ts.Render(label) + bs.Render(strings.Repeat(b.Top, fill)+b.TopRight)}

	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = ansi.Truncate(body[i], inner, "")
		}
		pad := inner - ansi.StringWidth(line)
		if pad < 0 {
			pad = 0
		}
		rows = append(rows, bs.Render(b.Left)+line+"\x1b[0m"+strings.Repeat(" ", pad)+bs.Render(b.Right))
	}

	foot := ""
	if footer != "" && ansi.StringWidth(footer)+4 <= inner {
		foot = " " + footer + " "
	}
	rows = append(rows, bs.Render(b.BottomLeft+strings.Repeat(b.Bottom, inner-1-ansi.StringWidth(foot))+foot+b.Bottom+b.BottomRight))
	return strings.Join(rows, "\n")
}

func countFooter(t table.Model) string {
	if len(t.Rows()) == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d", t.Cursor()+1, len(t.Rows()))
}

func (m model) statusPanelLines(width int) []string {
	owners := " " + hlStyle.Render(ansi.Truncate(m.orgLabel(), width-2, "…"))
	first := " " + dimStyle.Render(m.repoCountLabel()+" · scanned "+m.lastScanLabel())
	second := " " + dimStyle.Render("no scan yet")
	switch {
	case !m.hasTargets():
		second = " " + dimStyle.Render("press O to choose what to scan")
	case m.scanning && m.progress.total < 0:
		second = " " + m.spin.View() + dimStyle.Render(" listing repos…")
	case m.scanning:
		second = " " + m.spin.View() + dimStyle.Render(fmt.Sprintf(" scanning %d/%d repos", m.progress.done, m.progress.total))
	case m.lastCost != nil:
		second = " " + dimStyle.Render(fmt.Sprintf("last scan %d req, %d free (304)", m.lastCost.requests, m.lastCost.free))
	}
	return []string{owners, first, second}
}

func (m model) pickerPopup() string {
	w := m.width - 4
	if w > 64 {
		w = 64
	}
	limit := m.bodyHeight() - 12
	if limit > maxPickRows {
		limit = maxPickRows
	}
	if limit < 1 {
		limit = 1
	}
	title := "Choose what to scan"
	hints := []string{"space all/none · enter pick repos", "s save · esc cancel"}
	if !m.hasTargets() {
		hints[1] = "s save · esc quit"
	}
	var rows []string
	cursor := m.accountCursor
	switch {
	case m.pickStep == 1:
		title = "Repos · " + m.pickOwner
		hints = []string{"space toggle · enter/esc back"}
		cursor = m.repoCursor
		c := m.sel[strings.ToLower(m.pickOwner)]
		all := c != nil && c.all
		rows = append(rows, checkbox(all, false)+" all repositories")
		for _, name := range m.repoChoices {
			rows = append(rows, checkbox(all || (c != nil && c.repos[name]), false)+" "+name)
		}
	case !m.accountsReady:
		rows = nil
	default:
		accounts := m.pickAccounts()
		nameW := 0
		for _, a := range accounts {
			if lw := ansi.StringWidth(a.login); lw > nameW {
				nameW = lw
			}
		}
		for _, a := range accounts {
			c := m.sel[strings.ToLower(a.login)]
			some := c != nil && !c.all && len(c.repos) > 0
			row := checkbox(c != nil && c.all, some) + " " + a.login + strings.Repeat(" ", nameW-ansi.StringWidth(a.login)) + "  " + fmt.Sprintf("%-5s", a.kind)
			switch {
			case c != nil && c.all:
				row += " all repos"
			case some:
				row += fmt.Sprintf(" %d %s", len(c.repos), plural(len(c.repos), "repo", "repos"))
			}
			rows = append(rows, strings.TrimRight(row, " "))
		}
	}

	body := []string{""}
	start := 0
	if cursor >= limit {
		start = cursor - limit + 1
	}
	end := start + limit
	if end > len(rows) {
		end = len(rows)
	}
	for i := start; i < end; i++ {
		if i == cursor {
			body = append(body, accentStyle.Render(" > "+rows[i]))
			continue
		}
		body = append(body, "   "+rows[i])
	}
	switch {
	case m.pickStep == 0 && !m.accountsReady:
		body = append(body, " "+m.spin.View()+dimStyle.Render(" loading accounts…"))
	case m.pickStep == 0 && m.accountsErr != nil:
		body = append(body, "  "+redStyle.Render("cannot list accounts: "+m.accountsErr.Error()))
	case m.pickStep == 1 && !m.reposReady:
		body = append(body, " "+m.spin.View()+dimStyle.Render(" loading repositories…"))
	case m.pickStep == 1 && m.reposErr != nil:
		body = append(body, "  "+redStyle.Render("cannot list repositories: "+m.reposErr.Error()))
	case m.pickStep == 1 && m.opts.days > 0:
		body = append(body, "  "+dimStyle.Render(fmt.Sprintf("repos pushed in the last %d days", m.opts.days)))
	}
	if m.selNote != "" {
		body = append(body, "  "+yellowStyle.Render(m.selNote))
	}
	body = append(body, "")
	for _, h := range hints {
		body = append(body, "  "+dimStyle.Render(h))
	}
	footer := ""
	if n := len(rows); n > 0 {
		footer = fmt.Sprintf("%d of %d", cursor+1, n)
	}
	return panel(title, footer, true, w, len(body)+2, body)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func checkbox(on, some bool) string {
	switch {
	case on:
		return "[x]"
	case some:
		return "[~]"
	}
	return "[ ]"
}

func (m model) reposFooter() string {
	if len(m.repos) == 0 {
		return ""
	}
	return fmt.Sprintf("%d active", len(m.repos))
}

func (m model) repoLines(inner int) []string {
	if len(m.repos) == 0 {
		return []string{" " + dimStyle.Render("no active repos")}
	}
	repos := m.repos
	more := 0
	if len(repos) > maxRepoRows {
		more = len(repos) - maxRepoRows + 1
		repos = repos[:maxRepoRows-1]
	}
	nameW := 0
	for _, r := range repos {
		if w := ansi.StringWidth(r.name); w > nameW {
			nameW = w
		}
	}
	if limit := inner - 24; nameW > limit {
		nameW = limit
	}
	if nameW < 4 {
		nameW = 4
	}

	var lines []string
	for _, r := range repos {
		name := ansi.Truncate(r.name, nameW, "…")
		runStyle, qStyle := dimStyle, dimStyle
		if r.running > 0 {
			runStyle = greenStyle
		}
		if r.queued > 0 {
			qStyle = yellowStyle
		}
		lines = append(lines, " "+hlStyle.Render(name)+strings.Repeat(" ", nameW-ansi.StringWidth(name)+3)+
			runStyle.Render(fmt.Sprintf("%d running", r.running))+"   "+qStyle.Render(fmt.Sprintf("%d queued", r.queued)))
	}
	if more > 0 {
		lines = append(lines, " "+dimStyle.Render(fmt.Sprintf("+%d more", more)))
	}
	return lines
}

func (m model) detailPanel(w, h int) string {
	j, ok := m.selected()
	if !ok {
		return panel("Job", "", false, w, h, []string{"", " " + dimStyle.Render("no job selected")})
	}

	now := time.Now().UTC()
	isRunning := j.Status == "in_progress"
	status, statusStyle := "queued", yellowStyle.Bold(true)
	if isRunning {
		status, statusStyle = "running", greenStyle.Bold(true)
	}
	if m.cancelling(j) {
		status, statusStyle = "cancelling", yellowStyle.Bold(true)
	}

	kv := func(k, v string) string {
		return " " + dimStyle.Render(fmt.Sprintf("%-10s", k)) + " " + v
	}
	labels := strings.ReplaceAll(j.Labels, ",", ", ")
	if labels == "" {
		labels = "-"
	}

	lines := []string{
		"",
		" " + hlStyle.Render(m.repoName(j)) + dimStyle.Render(" › "+j.Workflow+" › ") + boldStyle.Render(j.Name),
		"",
		kv("Status", statusStyle.Render(status)),
	}
	if isRunning {
		lines = append(lines, kv("Runner", greenStyle.Render(j.Runner)))
		if !j.StartedAt.IsZero() {
			lines = append(lines, kv("Elapsed", fmtElapsed(now.Sub(j.StartedAt))+dimStyle.Render("  (since "+clock(j.StartedAt, "15:04")+")")))
		}
	} else if !j.CreatedAt.IsZero() {
		mins := int(now.Sub(j.CreatedAt).Minutes())
		lines = append(lines, kv("Waiting", waitColor(mins, lipgloss.NewStyle()).Render(fmt.Sprintf("%dm", mins))+dimStyle.Render("  (since "+clock(j.CreatedAt, "15:04")+")")))
	}
	lines = append(lines,
		kv("Branch", j.Branch),
		kv("Labels", labels),
		kv("Run", "#"+strconv.FormatInt(j.RunID, 10)),
		kv("URL", accentStyle.Render(strings.TrimPrefix(j.URL, "https://"))),
	)

	if !isRunning {
		inner := w - 4
		rule := inner
		if rule > 44 {
			rule = 44
		}
		lines = append(lines, "", " "+dimStyle.Bold(true).Render("Busy runners with these labels"), " "+dimStyle.Render(strings.Repeat("─", rule)))
		busy := busyRunners(j, m.running)
		if len(busy) == 0 {
			lines = append(lines, " "+dimStyle.Render("no running job uses all of these labels"))
		}
		for _, r := range busy {
			elapsed := "-"
			if !r.StartedAt.IsZero() {
				elapsed = fmtElapsed(now.Sub(r.StartedAt))
			}
			runner := fmt.Sprintf("%-10s", r.Runner)
			what := ansi.Truncate(m.repoName(r)+" › "+r.Name, inner-ansi.StringWidth(runner)-1-len(elapsed)-2, "…")
			gap := inner - ansi.StringWidth(runner) - 1 - ansi.StringWidth(what) - len(elapsed)
			if gap < 1 {
				gap = 1
			}
			lines = append(lines, " "+greenStyle.Render(runner)+" "+what+strings.Repeat(" ", gap)+dimStyle.Render(elapsed))
		}
	}

	content := h - 2
	if len(lines) > content-1 {
		lines = lines[:content-1]
	}
	for len(lines) < content-1 {
		lines = append(lines, "")
	}
	lines = append(lines, " "+hlStyle.Render("enter")+dimStyle.Render(" open in browser    ")+hlStyle.Render("c")+dimStyle.Render(" cancel whole run"))
	return panel("Job", status, false, w, h, lines)
}

func (m model) cancelPopup() string {
	j := m.confirmJob
	body := []string{
		"",
		"  Cancel run #" + strconv.FormatInt(j.RunID, 10) + " of " + hlStyle.Render(m.repoName(j)+" › "+j.Workflow) + "?",
		"  " + dimStyle.Render("GitHub cancels the whole run, not one job."),
		"",
		"  " + hlStyle.Render("y") + dimStyle.Render(" cancel run      ") + hlStyle.Render("any other key") + dimStyle.Render(" keep it"),
		"",
	}
	w := 0
	for _, l := range body {
		if lw := ansi.StringWidth(l); lw > w {
			w = lw
		}
	}
	w += 4
	if w > m.width {
		w = m.width
	}
	return panel("Cancel run", "", true, w, len(body)+2, body)
}

func overlay(bg, fg string, width int) string {
	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")
	fw := ansi.StringWidth(fgLines[0])
	x := (width - fw) / 2
	if x < 0 {
		x = 0
	}
	y := (len(bgLines) - len(fgLines)) / 2
	if y < 0 {
		y = 0
	}
	for i, l := range fgLines {
		row := y + i
		if row >= len(bgLines) {
			break
		}
		under := bgLines[row]
		bgLines[row] = ansi.Truncate(under, x, "") + "\x1b[0m" + l + "\x1b[0m" + ansi.TruncateLeft(under, x+fw, "")
	}
	return strings.Join(bgLines, "\n")
}

func (m model) usageOverlayView() string {
	u := usage()
	label := func(k, v string) string {
		return dimStyle.Render(fmt.Sprintf("%-18s", k)) + v
	}

	var lines []string
	if u.HasData {
		resource := u.Resource
		if resource == "" {
			resource = "-"
		}
		reset := "-"
		if !u.Reset.IsZero() {
			reset = clock(u.Reset, "15:04:05") + " (" + fmtCountdown(time.Until(u.Reset)) + ")"
		}
		lines = append(lines,
			label("resource", resource),
			label("limit", strconv.Itoa(u.Limit)),
			label("used", strconv.Itoa(u.Used)),
			label("remaining", usageStyle(u).Render(strconv.Itoa(u.Remaining))),
			label("resets", reset),
		)
	} else {
		lines = append(lines, label("rate limit", dimStyle.Render("no data yet")))
	}

	lines = append(lines,
		"",
		label("requests", strconv.FormatInt(u.Requests, 10)),
		label("not modified (304)", strconv.FormatInt(u.NotModified, 10)),
		label("rate limited", strconv.FormatInt(u.RateLimited, 10)),
	)

	last := "no scan yet"
	if m.lastCost != nil {
		last = fmt.Sprintf("%d requests, %d free (304)", m.lastCost.requests, m.lastCost.free)
	}
	lines = append(lines,
		"",
		label("last scan", last),
		label("interval", fmt.Sprintf("%ds", m.opts.interval)),
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Render(strings.Join(lines, "\n") + "\n\n" + dimStyle.Render("esc/u/q close"))

	area := m.bodyHeight()
	if area < lipgloss.Height(box) {
		area = lipgloss.Height(box)
	}
	return lipgloss.Place(m.width, area, lipgloss.Center, lipgloss.Center, box)
}

func fmtCountdown(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	total := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("in %dm%02ds", total/60, total%60)
}

func usageStyle(u scan.UsageSnapshot) lipgloss.Style {
	if u.Limit <= 0 {
		return dimStyle
	}
	pct := u.Remaining * 100 / u.Limit
	if pct < 5 {
		return redStyle
	}
	if pct < 20 {
		return yellowStyle
	}
	return dimStyle
}

func (m model) usageIndicator() string {
	u := usage()
	if !u.HasData {
		return dimStyle.Render("api –")
	}
	text := fmt.Sprintf("api %d/%d", u.Remaining, u.Limit)
	if !u.Reset.IsZero() {
		text += " · resets " + clock(u.Reset, "15:04")
	}
	return usageStyle(u).Render(text)
}

func (m model) orgLabel() string {
	if !m.hasTargets() {
		return "—"
	}
	return scan.TargetLabel(m.opts.targets)
}

func (m model) repoCountLabel() string {
	if m.repoCount < 0 {
		return "—"
	}
	return fmt.Sprintf("%d repos", m.repoCount)
}

func clock(t time.Time, layout string) string {
	t = t.Local()
	_, off := t.Zone()
	return t.Format(layout) + " " + zoneLabel(off)
}

func zoneLabel(off int) string {
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	label := fmt.Sprintf("UTC%s%d", sign, off/3600)
	if m := off % 3600 / 60; m != 0 {
		label += fmt.Sprintf(":%02d", m)
	}
	return label
}

func (m model) lastScanLabel() string {
	if m.lastScan == "" {
		return "-"
	}
	return m.lastScan
}

func (m model) statusView() string {
	if m.filterActive {
		return ansi.Truncate(m.filterInput.View(), m.width, "")
	}

	var parts []string
	if m.notice != "" {
		style := greenStyle
		if m.noticeErr {
			style = yellowStyle
		}
		parts = append(parts, style.Render(m.notice))
	}
	if m.saveErr != nil {
		parts = append(parts, yellowStyle.Render("account not saved"))
	}
	if !m.scanning && m.scanErr != nil {
		parts = append(parts, redStyle.Render("rate limited · incomplete"))
	}
	if m.filter != "" {
		parts = append(parts, dimStyle.Render("filter: "+m.filter))
	}
	if m.pending != nil {
		parts = append(parts, yellowStyle.Render("next scan: "+scan.TargetLabel(m.pending.targets)))
	}
	parts = append(parts, hlStyle.Render(m.orgLabel())+dimStyle.Render(fmt.Sprintf(" · next %ds", m.nextIn)), m.usageIndicator())
	right := strings.Join(parts, dimStyle.Render(" · ")) + " "

	budget := m.width - ansi.StringWidth(right) - 1
	left := " "
	for i, k := range keyHints {
		part := hlStyle.Render(k[0]) + dimStyle.Render(": "+k[1])
		if i > 0 {
			part = dimStyle.Render(" | ") + part
		}
		if ansi.StringWidth(left+part) > budget {
			break
		}
		left += part
	}

	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		gap = 1
	}
	return ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, "")
}

func Run(opts options) error {
	m := newModel(opts)
	program = tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}
