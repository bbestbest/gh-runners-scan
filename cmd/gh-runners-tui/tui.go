package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	{"<enter/o>", "open", "<esc>", "clear filter"},
	{"", "", "<q>", "quit"},
}

var runningCols = []string{"Runner", "Repo", "Workflow", "Branch", "Job", "Elapsed"}
var queuedCols = []string{"Waiting", "Repo", "Workflow", "Branch", "Job", "Labels"}

type progressMsg struct {
	done  int
	total int
}

type scanDoneMsg struct {
	jobs []scan.Job
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

	spin      spinner.Model
	scanning  bool
	progress  progressMsg
	repoCount int
	lastScan  string
	nextIn    int

	width  int
	height int
}

func newModel(opts options) model {
	m := model{
		opts:      opts,
		nextIn:    opts.interval,
		width:     120,
		height:    40,
		scanning:  true,
		progress:  progressMsg{0, scan.TotalUnknown},
		repoCount: scan.TotalUnknown,
	}

	m.runTable = newTable(runningCols)
	m.qTable = newTable(queuedCols)

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
	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).Bold(true).Foreground(colorDim)
	st.Selected = st.Selected.Bold(true).Foreground(colorAccent).Background(lipgloss.Color("0"))
	t.SetStyles(st)
	return t
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanCmd(m.opts), tickCmd(), m.spin.Tick)
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
	m.scanning = true
	m.progress = progressMsg{0, scan.TotalUnknown}
	return scanCmd(m.opts)
}

func scanCmd(opts options) tea.Cmd {
	return func() tea.Msg {
		jobs := scanLabeled(context.Background(), opts, func(done, total int) {
			if program != nil {
				program.Send(progressMsg{done, total})
			}
		})
		return scanDoneMsg{jobs: jobs}
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
		m.scanning = false
		m.jobs = msg.jobs
		m.lastScan = time.Now().UTC().Format("15:04:05")
		m.nextIn = m.opts.interval
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

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
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
	}

	return m, nil
}

func (m *model) moveCursor(delta int) {
	t := &m.runTable
	if m.focus == 1 {
		t = &m.qTable
	}
	if len(t.Rows()) == 0 {
		return
	}
	t.SetCursor(t.Cursor() + delta)
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
		runRows = append(runRows, table.Row{
			cell(j.Runner, greenStyle, cols, 0),
			cell(j.Repo, rest, cols, 1),
			cell(j.Workflow, rest, cols, 2),
			cell(j.Branch, rest, cols, 3),
			cell(j.Name, rest, cols, 4),
			cell(elapsed, rest, cols, 5),
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

	m.runTable.SetRows(runRows)
	m.qTable.SetRows(qRows)
	m.runTable.SetCursor(clamp(runCursor, len(runRows)))
	m.qTable.SetCursor(clamp(qCursor, len(qRows)))
	m.layout()
}

func (m model) rowStyle(repo string) lipgloss.Style {
	if m.opts.repo != "" && repo == m.opts.repo {
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

	m.runTable.SetHeight(tableHeight(runBox))
	m.qTable.SetHeight(tableHeight(qBox))
	m.runTable.SetWidth(inner)
	m.qTable.SetWidth(inner)
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

func (m model) View() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")

	inner := m.width - 2
	if inner < 20 {
		inner = 20
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

func (m model) headerView() string {
	labels := []string{"Org:", "Scanned:", "Last:", "Next:"}
	values := []string{
		m.opts.org,
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
	left := boldStyle.Render(m.opts.org + " › " + panel)
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

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + dimStyle.Render(right)
}

func Run(opts options) error {
	m := newModel(opts)
	program = tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}
