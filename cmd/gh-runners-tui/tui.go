package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

var hints = [4][4]string{
	{"<↑↓/jk>", "move", "</>", "filter"},
	{"<tab/1/2>", "panel", "<r>", "rescan"},
	{"<enter/o>", "open", "<c>", "cancel run"},
	{"<O>", "org", "<q>", "quit"},
}

var runningCols = []string{"Runner", "Repo", "Workflow", "Branch", "Job", "Elapsed"}
var queuedCols = []string{"Waiting", "Repo", "Workflow", "Branch", "Job", "Labels"}

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
	label    string
	days     int
	interval int
	repo     string
}

type model struct {
	opts options

	jobs    []scan.Job
	running []scan.Job
	queued  []scan.Job

	runTable table.Model
	qTable   table.Model
	focus    int

	filterInput  textinput.Model
	filterActive bool
	filter       string

	orgInput  textinput.Model
	orgActive bool
	orgs      []string
	orgCursor int

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
	m := model{
		opts:      opts,
		nextIn:    opts.interval,
		width:     120,
		height:    40,
		scanning:  opts.org != "",
		orgActive: opts.org == "",
		progress:  progressMsg{0, scan.TotalUnknown},
		repoCount: scan.TotalUnknown,
		orgs:      config.Load().Orgs,
		orgCursor: -1,
		cancels:   map[int64]bool{},
	}

	m.runTable = newTable(runningCols)
	m.qTable = newTable(queuedCols)

	in := textinput.New()
	in.Placeholder = "filter"
	in.Prompt = "/ "
	m.filterInput = in

	oi := textinput.New()
	oi.Placeholder = "organisation"
	oi.Prompt = "org: "
	oi.Width = orgInputWidth
	if m.orgActive {
		oi.Focus()
	}
	m.orgInput = oi

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
	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).Bold(true).Foreground(colorDim)
	st.Selected = st.Selected.Bold(true).Foreground(colorAccent).Background(lipgloss.Color("0"))
	t.SetStyles(st)
	return t
}

func (m model) Init() tea.Cmd {
	if m.opts.org == "" {
		return tea.Batch(tickCmd(), m.spin.Tick, textinput.Blink)
	}
	return tea.Batch(scanCmd(context.Background(), m.opts, 0), tickCmd(), m.spin.Tick)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) startScan() tea.Cmd {
	if m.scanning || m.opts.org == "" {
		return nil
	}
	return m.beginScan()
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
		m.lastScan = time.Now().UTC().Format("15:04:05")
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
	if m.orgActive {
		switch msg.String() {
		case "ctrl+c":
			return m, quitCmd()
		case "esc":
			if m.opts.org == "" {
				return m, quitCmd()
			}
			m.orgActive = false
			m.orgInput.SetValue("")
			m.orgInput.Blur()
			return m, nil
		case "up":
			m.moveOrgCursor(-1)
			return m, nil
		case "down":
			m.moveOrgCursor(1)
			return m, nil
		case "enter":
			org := strings.TrimSpace(m.orgInput.Value())
			if org == "" {
				if m.opts.org == "" {
					return m, nil
				}
				m.orgActive = false
				m.orgInput.SetValue("")
				m.orgInput.Blur()
				return m, nil
			}
			m.orgActive = false
			m.orgInput.SetValue(org)
			m.orgInput.Blur()
			m.saveErr = config.SetOrg(org)
			m.orgs = config.Load().Orgs
			m.orgCursor = -1
			if org == m.opts.org {
				return m, nil
			}
			m.opts.org = org
			m.jobs = nil
			m.scanErr = nil
			m.lastScan = ""
			m.lastCost = nil
			m.repoCount = scan.TotalUnknown
			m.nextIn = m.opts.interval
			cmd := m.beginScan()
			m.refresh()
			return m, cmd
		case "x":
			if m.orgCursor >= 0 && m.orgCursor < len(m.orgs) {
				m.removeOrgAtCursor()
				return m, nil
			}
		}
		m.orgCursor = -1
		var cmd tea.Cmd
		m.orgInput, cmd = m.orgInput.Update(msg)
		return m, cmd
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
	case "O":
		m.orgActive = true
		m.orgs = config.Load().Orgs
		m.orgCursor = -1
		m.orgInput.SetValue("")
		m.orgInput.Focus()
		return m, textinput.Blink
	case "u":
		m.usageActive = true
		return m, nil
	case "r":
		m.nextIn = m.opts.interval
		return m, m.startScan()
	case "tab":
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

func (m *model) moveOrgCursor(delta int) {
	if len(m.orgs) == 0 {
		return
	}
	next := m.orgCursor + delta
	if m.orgCursor < 0 && delta < 0 {
		next = len(m.orgs) - 1
	}
	if next < 0 {
		m.orgCursor = -1
		m.orgInput.SetValue("")
		return
	}
	if next > len(m.orgs)-1 {
		next = len(m.orgs) - 1
	}
	m.orgCursor = next
	m.orgInput.SetValue(m.orgs[next])
	m.orgInput.CursorEnd()
}

func (m *model) removeOrgAtCursor() {
	org := m.orgs[m.orgCursor]
	m.saveErr = config.RemoveOrg(org)
	kept := make([]string, 0, len(m.orgs))
	for _, o := range m.orgs {
		if o != org {
			kept = append(kept, o)
		}
	}
	m.orgs = kept
	if len(m.orgs) == 0 {
		m.orgCursor = -1
	} else if m.orgCursor > len(m.orgs)-1 {
		m.orgCursor = len(m.orgs) - 1
	}
	if m.orgCursor < 0 {
		m.orgInput.SetValue("")
	} else {
		m.orgInput.SetValue(m.orgs[m.orgCursor])
	}
	m.orgInput.CursorEnd()
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
		return
	}
	t.MoveDown(delta)
}

func (m *model) gotoEdge(bottom bool) {
	t := m.focusedTable()
	if len(t.Rows()) == 0 {
		return
	}
	if bottom {
		t.GotoBottom()
		return
	}
	t.GotoTop()
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

	now := time.Now().UTC()
	cols := m.runTable.Columns()
	runRows := make([]table.Row, 0, len(m.running))
	for _, j := range m.running {
		elapsed := "-"
		if !j.StartedAt.IsZero() {
			elapsed = fmtElapsed(now.Sub(j.StartedAt))
		}
		rest := m.rowStyle(j.Repo)
		elapsedStyle := rest
		if m.cancelling(j) {
			elapsed = "cancelling"
			elapsedStyle = yellowStyle
		}
		runRows = append(runRows, table.Row{
			cell(j.Runner, greenStyle, cols, 0),
			cell(j.Repo, rest, cols, 1),
			cell(j.Workflow, rest, cols, 2),
			cell(j.Branch, rest, cols, 3),
			cell(j.Name, rest, cols, 4),
			cell(elapsed, elapsedStyle, cols, 5),
		})
	}

	qcols := m.qTable.Columns()
	qRows := make([]table.Row, 0, len(m.queued))
	for _, j := range m.queued {
		wait := "-"
		waitStyle := m.rowStyle(j.Repo)
		if !j.CreatedAt.IsZero() {
			mins := int(now.Sub(j.CreatedAt).Minutes())
			wait = fmt.Sprintf("%dm", mins)
			if mins >= 60 {
				waitStyle = redStyle
			} else if mins >= 30 {
				waitStyle = yellowStyle
			}
		}
		if m.cancelling(j) {
			wait = "cancelling"
			waitStyle = yellowStyle
		}
		rest := m.rowStyle(j.Repo)
		qRows = append(qRows, table.Row{
			cell(wait, waitStyle, qcols, 0),
			cell(j.Repo, rest, qcols, 1),
			cell(j.Workflow, rest, qcols, 2),
			cell(j.Branch, rest, qcols, 3),
			cell(j.Name, rest, qcols, 4),
			cell(j.Labels, rest, qcols, 5),
		})
	}

	setRows(&m.runTable, runRows, runCursor)
	setRows(&m.qTable, qRows, qCursor)
	m.layout()
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

func cell(text string, style lipgloss.Style, cols []table.Column, i int) string {
	if i < len(cols) {
		text = truncate(text, cols[i].Width)
	}
	return style.Render(text)
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
	inner := m.width - 2
	if inner < 20 {
		inner = 20
	}

	m.runTable.SetColumns(columnWidths(runningCols, inner))
	m.qTable.SetColumns(columnWidths(queuedCols, inner))

	chrome := headerHeight + 1
	if m.filterActive {
		chrome++
	}
	body := m.height - chrome
	if body < 10 {
		body = 10
	}
	runBox := body / 2
	qBox := body - runBox

	setHeight(&m.runTable, tableHeight(runBox))
	setHeight(&m.qTable, tableHeight(qBox))
	m.runTable.SetWidth(inner)
	m.qTable.SetWidth(inner)
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
		if w < 5 {
			w = 5
		}
		if i == len(cols)-1 {
			w = avail - used
			if w < 5 {
				w = 5
			}
		}
		used += w
		widths[i] = table.Column{Title: c, Width: w}
	}
	return widths
}

func colUnits(name string) int {
	if name == "Branch" || name == "Job" {
		return 3
	}
	return 1
}

const headerHeight = 4

const hintColWidth = 18

const maxOrgRows = 8

const orgInputWidth = 20

func (m model) View() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")

	if m.usageActive {
		b.WriteString(m.usageOverlayView())
		b.WriteString("\n")
		b.WriteString(m.statusView())
		return b.String()
	}

	inner := m.width - 2
	if inner < 20 {
		inner = 20
	}

	if m.orgActive {
		b.WriteString(m.orgOverlayView())
		b.WriteString("\n")
		b.WriteString(m.statusView())
		return b.String()
	}

	b.WriteString(m.panelView(fmt.Sprintf("Running (%d)", len(m.running)), m.focus == 0, inner, m.runTable))
	b.WriteString("\n")
	b.WriteString(m.panelView(fmt.Sprintf("Queued (%d)", len(m.queued)), m.focus == 1, inner, m.qTable))
	b.WriteString("\n")

	if m.filterActive {
		b.WriteString(m.filterInput.View())
		b.WriteString("\n")
	}

	b.WriteString(m.statusView())
	return b.String()
}

func (m model) orgOverlayView() string {
	hint := "↑↓ pick · x delete · enter confirm · esc cancel"
	if m.opts.org == "" {
		hint = "↑↓ pick · x delete · enter confirm · esc quit"
	}

	body := m.orgInput.View()
	if list := m.orgListView(); list != "" {
		body += "\n\n" + list
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Render(body + "\n\n" + dimStyle.Render(hint))

	area := m.height - headerHeight - 2
	if area < lipgloss.Height(box) {
		area = lipgloss.Height(box)
	}
	return lipgloss.Place(m.width, area, lipgloss.Center, lipgloss.Center, box)
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
			reset = u.Reset.Local().Format("15:04:05") + " (" + fmtCountdown(time.Until(u.Reset)) + ")"
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

	area := m.height - headerHeight - 2
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
		text += " · resets " + u.Reset.Local().Format("15:04")
	}
	return usageStyle(u).Render(text)
}

func (m model) orgListView() string {
	orgs := m.orgs
	cursor := m.orgCursor
	if limit := m.orgListLimit(); len(orgs) > limit {
		start := 0
		if cursor >= limit {
			start = cursor - limit + 1
		}
		orgs = orgs[start : start+limit]
		cursor -= start
	}

	var lines []string
	for i, org := range orgs {
		row := "  " + org
		if i == cursor {
			row = accentStyle.Render("> " + org)
		}
		if org == m.opts.org {
			row += dimStyle.Render("  (saved)")
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

func (m model) orgListLimit() int {
	limit := m.height - headerHeight - 10
	if limit < 1 {
		limit = 1
	}
	if limit > maxOrgRows {
		limit = maxOrgRows
	}
	return limit
}

func (m model) headerView() string {
	labels := []string{"Org:", "Scanned:", "Last:", "Next:"}
	values := []string{
		m.orgLabel(),
		m.repoCountLabel(),
		m.lastScanLabel() + " UTC",
		fmt.Sprintf("%ds", m.nextIn),
	}

	leftW := 0
	for i := range labels {
		if w := lipgloss.Width(labels[i] + " " + values[i]); w > leftW {
			leftW = w
		}
	}

	var lines []string
	for i := range labels {
		l := dimStyle.Render(labels[i]) + " " + values[i]
		l = lipgloss.NewStyle().Width(leftW + 2).Render(l)

		h := hints[i]
		first := ""
		if h[0] != "" {
			first = accentStyle.Render(h[0]) + " " + dimStyle.Render(h[1])
		}
		hint := lipgloss.NewStyle().Width(hintColWidth).MaxWidth(hintColWidth).Render(first) +
			accentStyle.Render(h[2]) + " " + dimStyle.Render(h[3])

		gap := m.width - lipgloss.Width(l) - lipgloss.Width(hint)
		if gap < 1 {
			gap = 1
		}
		lines = append(lines, l+strings.Repeat(" ", gap)+hint)
	}
	return strings.Join(lines, "\n")
}

func (m model) orgLabel() string {
	if m.opts.org == "" {
		return "—"
	}
	return m.opts.org
}

func (m model) repoCountLabel() string {
	if m.repoCount < 0 {
		return "—"
	}
	return fmt.Sprintf("%d repos", m.repoCount)
}

func (m model) lastScanLabel() string {
	if m.lastScan == "" {
		return "-"
	}
	return m.lastScan
}

func (m model) panelView(title string, focused bool, inner int, t table.Model) string {
	border := lipgloss.RoundedBorder()
	bs := dimStyle
	ts := dimStyle.Bold(true)
	if focused {
		bs = accentStyle
		ts = accentStyle.Bold(true)
	}

	label := border.Top + " " + title + " "
	fill := inner - lipgloss.Width(label)
	if fill < 0 {
		fill = 0
	}
	top := bs.Render(border.TopLeft) + bs.Render(border.Top+" ") + ts.Render(title) + bs.Render(" ") +
		bs.Render(strings.Repeat(border.Top, fill)) + bs.Render(border.TopRight)

	lines := strings.Split(t.View(), "\n")
	side := bs.Render(border.Left)
	right := bs.Render(border.Right)

	var rows []string
	rows = append(rows, top)
	for _, l := range lines {
		rows = append(rows, side+lipgloss.NewStyle().Width(inner).MaxWidth(inner).Render(l)+right)
	}
	rows = append(rows, bs.Render(border.BottomLeft+strings.Repeat(border.Bottom, inner)+border.BottomRight))
	return strings.Join(rows, "\n")
}

func (m model) statusView() string {
	panel := "running"
	if m.focus == 1 {
		panel = "queued"
	}
	left := boldStyle.Render(m.orgLabel() + " › " + panel)
	if m.filter != "" {
		left += dimStyle.Render("   filter: " + m.filter)
	}

	var right string
	if m.scanning {
		if m.progress.total < 0 {
			right = m.spin.View() + " listing repos…"
		} else {
			right = m.spin.View() + " " + fmt.Sprintf("scanning %d/%d repos", m.progress.done, m.progress.total)
		}
	} else {
		right = fmt.Sprintf("%d running · %d queued", len(m.running), len(m.queued))
	}

	if !m.scanning && m.scanErr != nil {
		right = redStyle.Render("rate limited · incomplete") + dimStyle.Render("   "+right)
	} else {
		right = dimStyle.Render(right)
	}

	if m.saveErr != nil {
		right = yellowStyle.Render("org not saved") + dimStyle.Render("   ") + right
	}

	if m.notice != "" {
		style := greenStyle
		if m.noticeErr {
			style = yellowStyle
		}
		right = style.Render(m.notice) + dimStyle.Render("   ") + right
	}

	right += dimStyle.Render("   ") + m.usageIndicator()

	if m.confirming {
		prompt := "cancel run " + m.confirmJob.Workflow + " (" + m.confirmJob.Repo + ")? stops every job in this run · y/n"
		return yellowStyle.Render(prompt)
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func Run(opts options) error {
	m := newModel(opts)
	program = tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}
