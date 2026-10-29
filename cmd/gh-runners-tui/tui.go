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
	{"R", "re-run"},
	{"/", "filter"},
	{"h/l", "panel"},
	{"r", "rescan"},
	{"O", "choose"},
	{"u", "usage"},
	{"q", "quit"},
}

var helpGroups = []struct {
	title string
	keys  [][2]string
}{
	{"Navigate", [][2]string{
		{"j/k ↓/↑", "move"},
		{"gg/G < >", "top / bottom"},
		{"ctrl+d/u", "half page down / up"},
		{"ctrl+f/b", "page down / up"},
		{"H/M/L", "top / middle / bottom of view"},
		{"h/l", "previous / next panel"},
		{"0 1 2 3", "Repos / Running / Queued / Finished"},
	}},
	{"Actions", [][2]string{
		{"enter/o", "open in browser"},
		{"c", "cancel run"},
		{"R", "re-run finished run"},
		{"r", "rescan now"},
	}},
	{"View", [][2]string{
		{"/", "filter · enter keep · esc clear"},
		{"esc", "clear filter"},
		{"O", "choose accounts and repos"},
		{"u", "API usage"},
		{"?", "this help"},
		{"q", "quit"},
	}},
}

var filterHints = [][2]string{
	{"enter", "keep filter"},
	{"esc", "clear"},
}

var runningCols = []string{"Runner", "Repo", "Workflow", "Branch", "Job", "Elapsed"}
var queuedCols = []string{"Waiting", "Repo", "Workflow", "Branch", "Job", "Labels"}

var runningPanelCols = []string{"Runner", "Repo", "Job", "Step", "Elapsed"}

var queuedPanelCols = []string{"Wait", "Repo", "Job", "Labels"}

var finishedPanelCols = []string{"Done", "Repo", "Workflow", "Result"}

var repoPanelCols = []string{"Repo", "Running", "Queued"}

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

type rerunDoneMsg struct {
	job        scan.Job
	failedOnly bool
	err        error
}

type tickMsg time.Time

type options struct {
	org      string
	targets  []scan.Target
	label    string
	days     int
	interval int
	repo     string
	hints    string
	rounds   *scan.Rounds
	onRepo   func(owner, repo string, jobs []scan.Job)
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

	jobs     []scan.Job
	running  []scan.Job
	queued   []scan.Job
	finished []scan.Job
	repos    []repoCount

	runTable  table.Model
	qTable    table.Model
	fTable    table.Model
	rTable    table.Model
	runStyles [][]lipgloss.Style
	qStyles   [][]lipgloss.Style
	fStyles   [][]lipgloss.Style
	rStyles   [][]lipgloss.Style
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
	stale     bool
	nextIn    int
	scanErr   error
	saveErr   error

	usageActive bool
	helpActive  bool
	lastCost    *scanCost

	cancels    map[int64]bool
	confirming bool
	confirmJob scan.Job
	reruns     map[int64]bool
	rerunning  bool
	rerunJob   scan.Job
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
	if opts.rounds == nil {
		opts.rounds = scan.NewRounds()
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
		reruns:    map[int64]bool{},
	}
	if len(opts.targets) == 0 {
		m.openPicker()
	}

	m.runTable = newTable(runningPanelCols)
	m.qTable = newTable(queuedPanelCols)
	m.fTable = newTable(finishedPanelCols)
	m.rTable = newTable(repoPanelCols)

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
	m.stale = false
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

var rerunRun = scan.RerunRun

var wakeRepo = func(r *scan.Rounds, owner, repo string) { r.Wake(owner, repo) }

var usage = scan.Usage

var saveCache = scan.SaveCache

var saveLastJobs = scan.SaveLastJobs

var loadLastJobs = scan.LoadLastJobs

var loadRounds = scan.LoadRounds

var saveRounds = func(r *scan.Rounds) error { return r.Save() }

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

func (m model) viewKey() string {
	return strings.Join(scan.FormatTargets(m.opts.targets), ",") + "|" + strconv.Itoa(m.opts.days) + "|" + m.opts.label
}

func (m *model) restoreLast() {
	if !m.hasTargets() {
		return
	}
	jobs, at, ok := loadLastJobs(m.viewKey())
	if !ok {
		return
	}
	m.jobs = jobs
	for _, j := range jobs {
		m.opts.rounds.Hint(j.Owner, j.Repo)
	}
	m.lastScan = clock(at, "15:04:05")
	m.stale = true
	m.refresh()
}

func saveLastCmd(key string, jobs []scan.Job) tea.Cmd {
	return func() tea.Msg {
		saveLastJobs(key, jobs)
		return nil
	}
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

func rerunCmd(j scan.Job, failedOnly bool) tea.Cmd {
	return func() tea.Msg {
		return rerunDoneMsg{job: j, failedOnly: failedOnly, err: rerunRun(context.Background(), j.Owner, j.Repo, j.RunID, failedOnly)}
	}
}

func scanCmd(ctx context.Context, opts options, id int) tea.Cmd {
	return func() tea.Msg {
		before := usage()
		opts.onRepo = func(owner, repo string, jobs []scan.Job) {
			if program != nil {
				program.Send(repoJobsMsg{id: id, owner: owner, repo: repo, jobs: jobs})
			}
		}
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

type repoJobsMsg struct {
	id    int
	owner string
	repo  string
	jobs  []scan.Job
}

func replaceRepoJobs(jobs []scan.Job, owner, repo string, fresh []scan.Job) []scan.Job {
	out := make([]scan.Job, 0, len(jobs)+len(fresh))
	for _, j := range jobs {
		if !strings.EqualFold(j.Owner, owner) || !strings.EqualFold(j.Repo, repo) {
			out = append(out, j)
		}
	}
	return append(out, fresh...)
}

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

	case repoJobsMsg:
		if msg.id != m.scanID {
			return m, nil
		}
		m.jobs = replaceRepoJobs(m.jobs, msg.owner, msg.repo, msg.jobs)
		m.refresh()
		return m, nil

	case progressMsg:
		m.progress = msg
		if msg.total >= 0 {
			m.repoCount = msg.total
		}
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
		m.stale = false
		m.dropReruns()
		m.refresh()
		rounds := m.opts.rounds
		cmds := []tea.Cmd{saveCacheCmd, tea.SetWindowTitle(windowTitle(m.jobs)), func() tea.Msg {
			saveRounds(rounds)
			return nil
		}}
		if msg.err == nil {
			cmds = append(cmds, saveLastCmd(m.viewKey(), m.jobs))
		}
		return m, tea.Batch(cmds...)

	case cancelDoneMsg:
		label := msg.job.Workflow + " (" + msg.job.Repo + ")"
		m.noticeErr = true
		switch {
		case msg.err == nil:
			m.noticeErr = false
			m.notice = "cancel accepted · " + label
		case errors.Is(msg.err, scan.ErrRunFinished):
			m.notice = "run already finished · " + label
		case errors.Is(msg.err, scan.ErrNoPermission):
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

	case rerunDoneMsg:
		label := msg.job.Workflow + " (" + msg.job.Repo + ")"
		m.noticeErr = true
		switch {
		case msg.err == nil:
			m.noticeErr = false
			m.notice = "re-run requested · " + label
			if msg.failedOnly {
				m.notice = "re-run of failed jobs requested · " + label
			}
			wakeRepo(m.opts.rounds, msg.job.Owner, msg.job.Repo)
		case errors.Is(msg.err, scan.ErrNoPermission):
			delete(m.reruns, msg.job.RunID)
			m.notice = "no permission to re-run · token needs actions:write"
		case errors.Is(msg.err, scan.ErrRateLimited):
			delete(m.reruns, msg.job.RunID)
			m.notice = "rate limited · re-run not sent"
		default:
			delete(m.reruns, msg.job.RunID)
			m.notice = "re-run failed · " + msg.err.Error()
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
			m.filterActive = false
			m.filterInput.Blur()
			m.refresh()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.filter = strings.TrimSpace(m.filterInput.Value())
		m.refresh()
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

	if m.helpActive {
		switch msg.String() {
		case "ctrl+c":
			return m, quitCmd()
		case "esc", "?", "q":
			m.helpActive = false
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

	if m.rerunning {
		m.rerunning = false
		key := msg.String()
		job := m.rerunJob
		switch {
		case key == "ctrl+c":
			return m, quitCmd()
		case key == "f" && hasFailed(job), key == "a":
			if m.reruns[job.RunID] {
				return m, nil
			}
			m.reruns[job.RunID] = true
			m.refresh()
			return m, rerunCmd(job, key == "f")
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
		if job.Status == "completed" {
			m.notice = "run already finished · " + job.Workflow + " (" + job.Repo + ")"
			m.noticeErr = true
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
	case "R":
		job, ok := m.selected()
		if !ok || job.RunID == 0 {
			return m, nil
		}
		if job.Status != "completed" {
			m.notice = "re-run works on finished runs · press 3"
			m.noticeErr = true
			return m, nil
		}
		if m.reruns[job.RunID] {
			m.notice = "re-run already requested · " + job.Workflow + " (" + job.Repo + ")"
			m.noticeErr = true
			return m, nil
		}
		m.rerunning = true
		m.rerunJob = job
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
		m.filterInput.Width = m.width - 10
		m.filterInput.Focus()
		m.layout()
		return m, textinput.Blink
	case "u":
		m.usageActive = true
		return m, nil
	case "?":
		m.helpActive = true
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
	case "h":
		m.focus = focusOrder[(slices.Index(focusOrder, m.focus)+len(focusOrder)-1)%len(focusOrder)]
		return m, nil
	case "l":
		m.focus = focusOrder[(slices.Index(focusOrder, m.focus)+1)%len(focusOrder)]
		return m, nil
	case "0":
		m.focus = focusRepos
		return m, nil
	case "1":
		m.focus = 0
		return m, nil
	case "2":
		m.focus = 1
		return m, nil
	case "3":
		m.focus = 2
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
	case "home", "<":
		m.gotoEdge(false)
		return m, nil
	case "G", "end", ">":
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
	switch m.focus {
	case 1:
		return &m.qTable
	case 2:
		return &m.fTable
	case focusRepos:
		return &m.rTable
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
	switch m.focus {
	case 1:
		jobs = m.queued
		cursor = m.qTable.Cursor()
	case 2:
		jobs = m.finished
		cursor = m.fTable.Cursor()
	case focusRepos:
		return scan.Job{}, false
	}
	if cursor < 0 || cursor >= len(jobs) {
		return scan.Job{}, false
	}
	return jobs[cursor], true
}

func (m *model) open() {
	j, ok := m.selected()
	if !ok {
		return
	}
	if j.URL != "" {
		exec.Command("open", j.URL).Start()
	}
}

func (m *model) dropReruns() {
	finished := map[int64]bool{}
	for _, j := range m.jobs {
		if j.Status == "completed" {
			finished[j.RunID] = true
		}
	}
	for id := range m.reruns {
		if !finished[id] {
			delete(m.reruns, id)
		}
	}
}

func hasFailed(j scan.Job) bool {
	switch j.Conclusion {
	case "failure", "cancelled", "timed_out", "startup_failure":
		return true
	}
	return false
}

func conclusionStyle(conclusion string) lipgloss.Style {
	switch conclusion {
	case "success":
		return greenStyle
	case "cancelled", "skipped", "neutral":
		return yellowStyle
	case "failure", "timed_out", "startup_failure", "action_required":
		return redStyle
	}
	return dimStyle
}

func resultGlyph(conclusion string) string {
	switch conclusion {
	case "success":
		return "✓"
	case "failure", "timed_out", "startup_failure", "action_required":
		return "✗"
	case "cancelled":
		return "⊘"
	case "skipped", "neutral":
		return "–"
	}
	return "·"
}

func fmtAgo(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func (m *model) refresh() {
	runCursor := m.runTable.Cursor()
	qCursor := m.qTable.Cursor()
	fCursor := m.fTable.Cursor()
	rCursor := m.rTable.Cursor()

	visible := scan.FilterJobs(m.jobs, m.filter)
	m.running, m.queued = scan.SplitJobs(visible)
	m.finished = scan.FinishedJobs(visible)
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
			cell("● "+j.Runner, cols, 0),
			cell(m.repoName(j), cols, 1),
			cell(j.Name, cols, 2),
			cell(j.Step.Fraction(), cols, 3),
			cell(elapsed, cols, 4),
		})
		runStyles = append(runStyles, []lipgloss.Style{runnerStyle, rest, rest, rest, elapsedStyle})
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
			cell("○ "+wait, qcols, 0),
			cell(m.repoName(j), qcols, 1),
			cell(j.Name, qcols, 2),
			cell(j.Labels, qcols, 3),
		})
		qStyles = append(qStyles, []lipgloss.Style{waitStyle, rest, rest, rest})
	}

	fcols := m.fTable.Columns()
	fRows := make([]table.Row, 0, len(m.finished))
	fStyles := make([][]lipgloss.Style, 0, len(m.finished))
	fSel := clamp(fCursor, len(m.finished))
	for i, j := range m.finished {
		result := resultGlyph(j.Conclusion) + " " + j.Conclusion
		resultStyle := conclusionStyle(j.Conclusion)
		if m.reruns[j.RunID] {
			result = "↻ re-running"
			resultStyle = yellowStyle
		}
		rest := m.rowStyle(j.Repo)
		doneStyle := dimStyle
		if i == fSel {
			doneStyle, rest, resultStyle = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()
		}
		fRows = append(fRows, table.Row{
			cell(fmtAgo(now.Sub(j.FinishedAt)), fcols, 0),
			cell(m.repoName(j), fcols, 1),
			cell(j.Workflow, fcols, 2),
			cell(result, fcols, 3),
		})
		fStyles = append(fStyles, []lipgloss.Style{doneStyle, rest, rest, resultStyle})
	}

	m.runStyles, m.qStyles, m.fStyles = runStyles, qStyles, fStyles
	setRows(&m.runTable, runRows, runCursor)
	setRows(&m.qTable, qRows, qCursor)
	setRows(&m.fTable, fRows, fCursor)

	rRows := make([]table.Row, 0, len(m.repos))
	rStyles := make([][]lipgloss.Style, 0, len(m.repos))
	rSel := clamp(rCursor, len(m.repos))
	rcols := m.rTable.Columns()
	for i, r := range m.repos {
		runStyle, qStyle := dimStyle, dimStyle
		if r.running > 0 {
			runStyle = greenStyle
		}
		if r.queued > 0 {
			qStyle = yellowStyle
		}
		nameStyle := hlStyle
		if i == rSel {
			nameStyle, runStyle, qStyle = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()
		}
		rRows = append(rRows, table.Row{
			cell(r.name, rcols, 0),
			cell(fmt.Sprintf("%d running", r.running), rcols, 1),
			cell(fmt.Sprintf("%d queued", r.queued), rcols, 2),
		})
		rStyles = append(rStyles, []lipgloss.Style{nameStyle, runStyle, qStyle})
	}
	m.rStyles = rStyles
	setRows(&m.rTable, rRows, rCursor)
	m.layout()
}

func windowTitle(jobs []scan.Job) string {
	_, queued := scan.SplitJobs(jobs)
	if len(queued) == 0 {
		return "gh-runner"
	}
	return fmt.Sprintf("(%d queued) gh-runner", len(queued))
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
	switch focus {
	case 1:
		return tableView(m.qTable, m.qStyles)
	case 2:
		return tableView(m.fTable, m.fStyles)
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
	m.fTable.SetColumns(columnWidths(finishedPanelCols, inner))
	m.rTable.SetColumns(repoColumns(inner))

	runBox, qBox, fBox := m.listHeights()
	setHeight(&m.runTable, tableHeight(runBox))
	setHeight(&m.qTable, tableHeight(qBox))
	setHeight(&m.fTable, tableHeight(fBox))
	setHeight(&m.rTable, m.reposHeight())
	m.runTable.SetWidth(inner)
	m.qTable.SetWidth(inner)
	m.fTable.SetWidth(inner)
	m.rTable.SetWidth(inner)
}

func repoColumns(inner int) []table.Column {
	run, queued := len("99 running"), len("99 queued")
	name := inner - 2 - run - queued - 2*2
	return []table.Column{{Title: "Repo", Width: max(name, 4)}, {Title: "Running", Width: run}, {Title: "Queued", Width: queued}}
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
	room := m.bodyHeight() - statusPanelHeight - 2*minListBox - collapsedBox
	return min(maxRepoRows+2, max(room, 3))
}

func (m model) listHeights() (int, int, int) {
	rest := m.bodyHeight() - statusPanelHeight - m.reposHeight()
	finished := rest / panelCount
	if finished < minListBox {
		finished = collapsedBox
	}
	run := (rest - finished) / 2
	queued := rest - finished - run
	return max(run, minListBox), max(queued, minListBox), finished
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
	for ; used > avail; used-- {
		widest := -1
		for i, c := range widths {
			if c.Width > colMin(c.Title) && (widest < 0 || c.Width-colMin(c.Title) > widths[widest].Width-colMin(widths[widest].Title)) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest].Width--
	}
	return widths
}

func colMin(name string) int {
	if name == "Elapsed" {
		return len("cancelling")
	}
	if name == "Wait" || name == "Result" {
		return len("○ cancelling")
	}
	return 5
}

func colUnits(name string) int {
	switch name {
	case "Branch", "Job", "Labels", "Workflow":
		return 3
	case "Runner", "Repo", "Elapsed":
		return 2
	}
	return 1
}

const keybarHeight = 1

const statusPanelHeight = 5

const panelBorders = 2

const popupPadding = 4

const maxPickRows = 10

const maxRepoRows = 6

const minListWidth = 50

const minDetailWidth = 40

const minBodyHeight = 16

const minListBox = 5

const panelCount = 3

const focusRepos = 3

var focusOrder = []int{focusRepos, 0, 1, 2}

const collapsedBox = 2

func (m model) View() string {
	return m.bodyView() + "\n" + m.statusView()
}

func (m model) bodyView() string {
	if m.usageActive {
		return m.usageOverlayView()
	}

	left, right := m.columns()
	runH, qH, fH := m.listHeights()
	col := lipgloss.JoinVertical(lipgloss.Left,
		panel("Status", "", false, left, statusPanelHeight, m.statusPanelLines(left-2)),
		panel("≡ [0] Repos", m.reposFooter(), m.focus == focusRepos, left, m.reposHeight(), m.repoLines()),
		panel(fmt.Sprintf("▶ [1] Running (%d)", len(m.running)), countFooter(m.runTable), m.focus == 0, left, runH, m.tableLines(0)),
		panel(fmt.Sprintf("⏸ [2] Queued (%d)", len(m.queued)), countFooter(m.qTable), m.focus == 1, left, qH, m.tableLines(1)),
		panel(fmt.Sprintf("✓ [3] Finished · 24h (%d)", len(m.finished)), countFooter(m.fTable), m.focus == 2, left, fH, m.tableLines(2)),
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
	if m.rerunning {
		body = overlay(body, m.rerunPopup(), m.width)
	}
	if m.helpActive {
		body = overlay(body, m.helpPopup(), m.width)
	}
	if m.filterActive {
		body = overlayAt(body, m.filterPrompt(), 0, 0)
	}
	return body
}

func (m model) filterPrompt() string {
	n := len(m.running) + len(m.queued) + len(m.finished)
	footer := plural(n, "1 match", fmt.Sprintf("%d matches", n))
	if m.filter == "" {
		footer = "type to filter"
	}
	return panel("Filter", footer, true, m.width, 3, []string{" " + m.filterInput.View()})
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
	if m.stale {
		first = " " + yellowStyle.Render("last results from "+m.lastScanLabel())
	}
	second := " " + dimStyle.Render("no scan yet")
	switch {
	case !m.hasTargets():
		second = " " + dimStyle.Render("press O to choose what to scan")
	case m.scanning && m.progress.total < 0:
		second = " " + m.spin.View() + dimStyle.Render(" listing repos…")
	case m.scanning:
		second = " " + m.spin.View() + dimStyle.Render(fmt.Sprintf(" scanning %d/%d repos", m.progress.done, m.progress.total))
	case m.opts.rounds.Unseen() > 0:
		second = " " + yellowStyle.Render(fmt.Sprintf("first pass · %d %s not checked yet", m.opts.rounds.Unseen(), plural(m.opts.rounds.Unseen(), "repo", "repos")))
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
	if m.focus == focusRepos {
		return countFooter(m.rTable)
	}
	return fmt.Sprintf("%d active", len(m.repos))
}

func (m model) repoLines() []string {
	if len(m.repos) == 0 {
		return []string{" " + dimStyle.Render("no active repos")}
	}
	lines := tableView(m.rTable, m.rStyles)
	return lines[len(lines)-m.rTable.Height():]
}

func (m model) repoDetail(w, h int) string {
	if len(m.repos) == 0 {
		return panel("Repo", "", false, w, h, []string{"", " " + dimStyle.Render("no active repos")})
	}
	r := m.repos[clamp(m.rTable.Cursor(), len(m.repos))]
	lines := []string{"", " " + hlStyle.Render(r.name), ""}
	section := func(title string, jobs []scan.Job, style lipgloss.Style, text func(scan.Job) string) {
		var rows []string
		for _, j := range jobs {
			if m.repoName(j) == r.name {
				rows = append(rows, "   "+text(j))
			}
		}
		lines = append(lines, " "+style.Render(fmt.Sprintf("%s (%d)", title, len(rows))))
		lines = append(lines, rows...)
		lines = append(lines, "")
	}
	section("Running", m.running, greenStyle.Bold(true), func(j scan.Job) string {
		return j.Name + dimStyle.Render("  "+j.Workflow+" · "+j.Runner)
	})
	section("Queued", m.queued, yellowStyle.Bold(true), func(j scan.Job) string {
		return j.Name + dimStyle.Render("  "+j.Workflow+" · "+j.Labels)
	})
	section("Finished · 24h", m.finished, boldStyle, func(j scan.Job) string {
		return conclusionStyle(j.Conclusion).Render(fmt.Sprintf("%-10s", j.Conclusion)) + " " + j.Workflow
	})
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	return panel("Repo", fmt.Sprintf("%d running · %d queued", r.running, r.queued), false, w, h, lines)
}

func (m model) detailPanel(w, h int) string {
	if m.focus == focusRepos {
		return m.repoDetail(w, h)
	}
	j, ok := m.selected()
	if !ok {
		return panel("Job", "", false, w, h, []string{"", " " + dimStyle.Render("no job selected")})
	}
	if j.Status == "completed" {
		return m.runPanel(j, w, h)
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
		if j.Step.Total > 0 {
			lines = append(lines, kv("Step", j.Step.Fraction()+dimStyle.Render(" · ")+j.Step.Name))
		}
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

func (m model) runPanel(j scan.Job, w, h int) string {
	kv := func(k, v string) string {
		return " " + dimStyle.Render(fmt.Sprintf("%-10s", k)) + " " + v
	}
	result, style := j.Conclusion, conclusionStyle(j.Conclusion).Bold(true)
	if m.reruns[j.RunID] {
		result, style = "re-running", yellowStyle.Bold(true)
	}
	lines := []string{
		"",
		" " + hlStyle.Render(m.repoName(j)) + dimStyle.Render(" › ") + boldStyle.Render(j.Workflow),
		"",
		kv("Result", style.Render(result)),
		kv("Finished", fmtAgo(time.Since(j.FinishedAt))+" ago"+dimStyle.Render("  ("+clock(j.FinishedAt, "15:04")+")")),
		kv("Branch", j.Branch),
		kv("Run", "#"+strconv.FormatInt(j.RunID, 10)),
		kv("URL", accentStyle.Render(strings.TrimPrefix(j.URL, "https://"))),
	}
	content := h - 2
	if len(lines) > content-1 {
		lines = lines[:content-1]
	}
	for len(lines) < content-1 {
		lines = append(lines, "")
	}
	lines = append(lines, " "+hlStyle.Render("enter")+dimStyle.Render(" open in browser    ")+hlStyle.Render("R")+dimStyle.Render(" re-run"))
	return panel("Run", result, false, w, h, lines)
}

func (m model) rerunPopup() string {
	j := m.rerunJob
	choices := hlStyle.Render("a") + dimStyle.Render(" all jobs      ")
	if hasFailed(j) {
		choices = hlStyle.Render("f") + dimStyle.Render(" failed jobs   ") + choices
	}
	body := []string{
		"",
		"  Re-run run #" + strconv.FormatInt(j.RunID, 10) + " of " + hlStyle.Render(m.repoName(j)+" › "+j.Workflow) + "?",
		"  " + dimStyle.Render("Result: ") + conclusionStyle(j.Conclusion).Render(j.Conclusion),
		"",
		"  " + choices + hlStyle.Render("any other key") + dimStyle.Render(" keep it"),
		"",
	}
	return m.popup("Re-run", "", body)
}

func (m model) helpPopup() string {
	keyW := 0
	for _, g := range helpGroups {
		for _, k := range g.keys {
			keyW = max(keyW, ansi.StringWidth(k[0]))
		}
	}
	tip := "hints = full in the config keeps every key in the bar"
	if m.opts.hints == "full" {
		tip = "hints = short in the config shows only ?: help in the bar"
	}
	var full, compact []string
	for _, g := range helpGroups {
		title := "  " + boldStyle.Render(g.title)
		full = append(full, "", title)
		compact = append(compact, title)
		for _, k := range g.keys {
			line := "    " + hlStyle.Render(k[0]+strings.Repeat(" ", keyW-ansi.StringWidth(k[0]))) + "  " + dimStyle.Render(k[1])
			full = append(full, line)
			compact = append(compact, line)
		}
	}
	full = append(full, "", "  "+dimStyle.Render(tip), "")
	body := full
	if len(full)+panelBorders > m.bodyHeight() {
		body = compact
	}
	return m.popup("? Keys", "esc close", body)
}

func (m model) popup(title, footer string, body []string) string {
	w := 0
	for _, l := range body {
		w = max(w, ansi.StringWidth(l))
	}
	return panel(title, footer, true, min(w+popupPadding, m.width), len(body)+panelBorders, body)
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
	return m.popup("Cancel run", "", body)
}

func overlay(bg, fg string, width int) string {
	fw := ansi.StringWidth(strings.SplitN(fg, "\n", 2)[0])
	y := (strings.Count(bg, "\n") - strings.Count(fg, "\n")) / 2
	return overlayAt(bg, fg, max((width-fw)/2, 0), max(y, 0))
}

func overlayAt(bg, fg string, x, y int) string {
	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")
	fw := ansi.StringWidth(fgLines[0])
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
	if m.filter != "" && !m.filterActive {
		parts = append(parts, dimStyle.Render("filter: "+m.filter))
	}
	if m.pending != nil {
		parts = append(parts, yellowStyle.Render("next scan: "+scan.TargetLabel(m.pending.targets)))
	}
	parts = append(parts, hlStyle.Render(m.orgLabel())+dimStyle.Render(fmt.Sprintf(" · next %ds", m.nextIn)), m.usageIndicator())
	right := strings.Join(parts, dimStyle.Render(" · ")) + " "

	budget := m.width - ansi.StringWidth(right) - 1
	left := " "
	var hints [][2]string
	if m.opts.hints == "full" {
		hints = keyHints
	}
	if m.filterActive {
		hints = filterHints
	} else {
		left += hlStyle.Render("?") + dimStyle.Render(": help")
	}
	for i, k := range hints {
		part := hlStyle.Render(k[0]) + dimStyle.Render(": "+k[1])
		if i > 0 || !m.filterActive {
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
	opts.rounds = loadRounds()
	m := newModel(opts)
	m.restoreLast()
	program = tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}
