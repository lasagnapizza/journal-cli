package main

// The app shell: entries, day pager, timeline, composer, entry menu, export.
// One screen. The cursor is already blinking next to the current time; write a
// line, press enter, it's logged.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	composerMaxLines = 5
	// header, blank line, the composer clock, help line
	chromeLines = 4
)

var (
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	faintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	labelStyle  = lipgloss.NewStyle().Bold(true)
	timeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	entryStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	clockStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255"))
	selStyle    = lipgloss.NewStyle().Reverse(true)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("108"))
)

type tickMsg time.Time

type clearStatusMsg struct{ gen int }

type appModel struct {
	entries []Entry
	pages   []day
	pageIdx int

	ta textarea.Model
	vp viewport.Model

	width, height int
	now           int64
	use24h        bool

	// -1: no entry selected. Otherwise an index into the shown day's entries.
	sel        int
	confirming bool
	exporting  bool
	status     string
	saveErr    string

	// editing: the composer holds an existing entry's text and Enter saves it
	// back under editingAt. A separate flag, not an At sentinel — 0 is a
	// legal timestamp.
	editing    bool
	editingAt  int64
	stashDraft string

	// statusGen tags each transient status so a stale clear timer from an
	// earlier status can't wipe a newer one.
	statusGen int
}

func newApp(entries []Entry, use24h bool) appModel {
	ta := textarea.New()
	ta.Placeholder = "What just happened? What's next?"
	ta.Prompt = " " // the clock renders on its own line above, like the web ui
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// the textarea sizes itself to its wrapped content — hard newlines and
	// soft wraps both — so a long sentence never scrolls out of view
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = composerMaxLines
	ta.SetHeight(1)
	s := textarea.DefaultStyles(true)
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)
	// real terminal cursor, positioned by View — the virtual one is a styled
	// cell that neither blinks nor matches the terminal's cursor shape
	ta.SetVirtualCursor(false)
	ta.Focus()

	m := appModel{
		entries: entries,
		ta:      ta,
		vp:      viewport.New(),
		now:     time.Now().UnixMilli(),
		use24h:  use24h,
		sel:     -1,
	}
	m.rebuildPages(true)
	return m
}

func (m appModel) Init() tea.Cmd {
	return tickEvery()
}

func tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---- day pages ----------------------------------------------------------

func (m *appModel) rebuildPages(snapToday bool) {
	shown := ""
	if !snapToday && m.pageIdx < len(m.pages) {
		shown = m.pages[m.pageIdx].Key
	}
	m.pages = dayPages(m.entries, m.now)
	// default to today's page by key — a future-dated entry means today is
	// not necessarily the last page
	today := dayKey(m.now)
	m.pageIdx = len(m.pages) - 1
	for i, d := range m.pages {
		if d.Key == today {
			m.pageIdx = i
			break
		}
	}
	if shown != "" {
		for i, d := range m.pages {
			if d.Key == shown {
				m.pageIdx = i
				break
			}
		}
	}
	m.refreshTimeline(true)
}

func (m *appModel) shownDay() day {
	if m.pageIdx < len(m.pages) {
		return m.pages[m.pageIdx]
	}
	return day{Key: dayKey(m.now)}
}

func (m *appModel) page(delta int) {
	next := m.pageIdx + delta
	if next < 0 || next >= len(m.pages) {
		return // rubber-band: nothing that way
	}
	m.pageIdx = next
	m.sel = -1
	m.confirming = false
	m.refreshTimeline(true)
}

// ---- timeline -----------------------------------------------------------

// timelineLines renders the shown day and returns the line index each entry
// starts at, so selection can scroll to it.
func (m *appModel) timelineLines() ([]string, []int) {
	d := m.shownDay()
	if len(d.Entries) == 0 {
		return []string{"", dimStyle.Render("  nothing yet — write a line below")}, nil
	}
	tsWidth := len(fmtTime(0, m.use24h))
	textWidth := max(20, m.width-tsWidth-4)
	wrap := lipgloss.NewStyle().Width(textWidth)
	var lines []string
	starts := make([]int, len(d.Entries))
	for i, e := range d.Entries {
		if i > 0 {
			if g := gapLabel(time.Duration(e.At-d.Entries[i-1].At) * time.Millisecond); g != "" {
				lines = append(lines, "", m.gapDivider(g), "")
			} else {
				lines = append(lines, "")
			}
		}
		starts[i] = len(lines)
		body := wrap.Render(e.Text)
		ts := timeStyle.Render(fmtTime(e.At, m.use24h))
		if i == m.sel {
			ts = selStyle.Render(fmtTime(e.At, m.use24h))
		}
		for j, l := range strings.Split(body, "\n") {
			if j == 0 {
				lines = append(lines, "  "+ts+"  "+entryStyle.Render(l))
			} else {
				lines = append(lines, strings.Repeat(" ", tsWidth+4)+entryStyle.Render(l))
			}
		}
	}
	return lines, starts
}

// gapDivider: a full-width hairline with the gap's duration centered in it,
// the web ui's "where did the time go" marker.
func (m *appModel) gapDivider(label string) string {
	inner := " " + label + " "
	side := (m.width - lipgloss.Width(inner)) / 2
	if side < 1 {
		return dimStyle.Render(label)
	}
	rule := faintStyle.Render(strings.Repeat("─", side))
	right := faintStyle.Render(strings.Repeat("─", m.width-side-lipgloss.Width(inner)))
	return rule + dimStyle.Render(inner) + right
}

// refreshTimeline re-renders the day into the viewport; pin parks it on the
// newest entry, so a day arrives already showing its bottom.
func (m *appModel) refreshTimeline(pin bool) {
	lines, starts := m.timelineLines()
	m.vp.SetContent(strings.Join(lines, "\n"))
	if pin {
		m.vp.GotoBottom()
	} else if m.sel >= 0 && m.sel < len(starts) {
		m.scrollTo(starts[m.sel])
	}
}

func (m *appModel) scrollTo(line int) {
	if line < m.vp.YOffset() {
		m.vp.SetYOffset(line)
	} else if bottom := m.vp.YOffset() + m.vp.Height() - 1; line > bottom {
		m.vp.SetYOffset(line - m.vp.Height() + 1)
	}
}

// ---- composer -----------------------------------------------------------

// composerClock: the line above the input is the live clock — or, mid-edit,
// the edited entry's own frozen time, so it says which moment the text
// belongs to.
func (m appModel) composerClock() string {
	at := m.now
	if m.editing {
		at = m.editingAt
	}
	return clockStyle.Render(fmtTime(at, m.use24h))
}

func (m *appModel) layout() {
	if m.width == 0 {
		return
	}
	// DynamicHeight sizes the textarea to its wrapped content; the timeline
	// gets whatever the composer doesn't take
	m.ta.SetWidth(m.width)
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(max(1, m.height-chromeLines-m.ta.Height()))
}

func (m *appModel) logEntry() {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return // blank input does nothing
	}
	at := newAt(m.entries)
	m.entries = append(m.entries, Entry{Text: text, At: at})
	m.persist()
	m.ta.Reset()
	m.sel = -1
	m.now = at
	m.rebuildPages(true) // logging anything snaps you back to today
}

// startEdit lifts the picked entry into the composer. The in-progress draft
// is stashed and comes back when the edit ends, either way it ends.
func (m *appModel) startEdit() {
	d := m.shownDay()
	if m.sel < 0 || m.sel >= len(d.Entries) {
		return
	}
	e := d.Entries[m.sel]
	m.stashDraft = m.ta.Value()
	m.editing = true
	m.editingAt = e.At
	m.ta.SetValue(e.Text)
	m.sel = -1
	m.layout()
	m.refreshTimeline(false)
}

func (m *appModel) finishEdit(save bool) {
	if save {
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return // blank save does nothing; esc is the way to discard
		}
		for i := range m.entries {
			if m.entries[i].At == m.editingAt {
				m.entries[i].Text = text
				break
			}
		}
		m.persist()
	}
	m.editing = false
	m.editingAt = 0
	m.ta.SetValue(m.stashDraft)
	m.stashDraft = ""
	m.layout()
	// editing never changes which day an entry lives in, so stay on the page
	// and keep the scroll where it was
	y := m.vp.YOffset()
	m.rebuildPages(false)
	m.vp.SetYOffset(y)
}

func (m *appModel) deleteSelected() {
	d := m.shownDay()
	if m.sel < 0 || m.sel >= len(d.Entries) {
		return
	}
	at := d.Entries[m.sel].At
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.At != at {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	m.persist()
	m.sel = -1
	// rebuildPages already falls back to today when the shown day's last
	// entry vanished; when the day survives, keep the scroll where it was
	y := m.vp.YOffset()
	m.rebuildPages(false)
	if m.shownDay().Key == d.Key {
		m.vp.SetYOffset(y)
	}
}

func (m *appModel) persist() {
	if err := saveEntries(m.entries, m.use24h); err != nil {
		m.saveErr = "save failed: " + err.Error()
	} else {
		m.saveErr = ""
	}
}

// ---- export -------------------------------------------------------------

func (m *appModel) exportText(history bool) string {
	if history {
		return historyText(m.entries, m.use24h)
	}
	return dayText(m.shownDay().Entries, m.use24h)
}

func (m *appModel) exportFile(history bool) tea.Cmd {
	name := "journal-" + m.shownDay().Key + ".txt"
	if history {
		name = "journal-history.txt"
	}
	err := os.WriteFile(name, []byte(m.exportText(history)+"\n"), 0o600)
	if err != nil {
		return m.setStatus(warnStyle.Render(err.Error()))
	}
	return m.setStatus(statusStyle.Render("saved " + name))
}

// setStatus shows a transient message and schedules its clear. The clear
// carries the status generation, so an earlier status's timer can't wipe a
// newer message that replaced it.
func (m *appModel) setStatus(s string) tea.Cmd {
	m.status = s
	m.statusGen++
	gen := m.statusGen
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return clearStatusMsg{gen} })
}

// ---- update -------------------------------------------------------------

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refreshTimeline(m.sel < 0)
		return m, nil

	case tickMsg:
		prevDay := dayKey(m.now)
		m.now = time.Time(msg).UnixMilli()
		if dayKey(m.now) != prevDay {
			// midnight rollover: today gets a fresh page. Keep the scroll —
			// someone reading back at 23:59 must not be yanked to the bottom.
			shown := m.shownDay().Key
			y := m.vp.YOffset()
			m.rebuildPages(false)
			if m.shownDay().Key == shown {
				m.vp.SetYOffset(y)
			} else {
				m.sel = -1 // the shown page vanished; a stale pick would point off-screen
			}
		}
		return m, tickEvery()

	case clearStatusMsg:
		if msg.gen == m.statusGen {
			m.status = ""
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m appModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	switch key {
	case "ctrl+c", "ctrl+q":
		return m, tea.Quit
	}

	if m.exporting {
		switch key {
		case "1":
			m.exporting = false
			return m, tea.Batch(tea.SetClipboard(m.exportText(false)), m.setStatus(statusStyle.Render("day copied")))
		case "2":
			m.exporting = false
			return m, tea.Batch(tea.SetClipboard(m.exportText(true)), m.setStatus(statusStyle.Render("history copied")))
		case "3":
			m.exporting = false
			return m, m.exportFile(false)
		case "4":
			m.exporting = false
			return m, m.exportFile(true)
		case "esc":
			m.exporting = false
		}
		return m, nil
	}

	if m.confirming {
		switch key {
		case "y":
			m.confirming = false
			m.deleteSelected()
		case "n", "esc":
			m.confirming = false
		}
		return m, nil
	}

	// mid-edit, an emptied composer must not turn the arrows into paging or
	// selection — the caret stays owned by the edit until enter or esc
	draftEmpty := m.ta.Value() == "" && !m.editing

	if m.sel >= 0 {
		switch key {
		case "up", "k":
			if m.sel > 0 {
				m.sel--
			}
			m.refreshTimeline(false)
			return m, nil
		case "down", "j":
			m.sel++
			if m.sel >= len(m.shownDay().Entries) {
				m.sel = -1 // stepped past the newest → back to the composer
				m.refreshTimeline(true)
			} else {
				m.refreshTimeline(false)
			}
			return m, nil
		case "d", "delete", "backspace":
			m.confirming = true
			return m, nil
		case "e":
			m.startEdit()
			return m, nil
		case "esc":
			m.sel = -1
			m.refreshTimeline(true)
			return m, nil
		case "left", "right":
			// fall through to paging below
		default:
			m.sel = -1
			m.refreshTimeline(true)
			// anything else falls through to the composer
		}
	}

	switch key {
	case "enter":
		if m.editing {
			m.finishEdit(true)
		} else {
			m.logEntry()
		}
		m.layout()
		return m, nil
	case "shift+enter", "alt+enter", "ctrl+j":
		m.ta.InsertString("\n")
		m.layout()
		return m, nil
	case "esc":
		if m.editing {
			m.finishEdit(false)
		}
		return m, nil
	case "ctrl+e":
		m.exporting = true
		return m, nil
	case "ctrl+left":
		m.page(-1)
		return m, nil
	case "ctrl+right":
		m.page(1)
		return m, nil
	case "pgup":
		m.vp.ScrollUp(m.vp.Height() / 2)
		return m, nil
	case "pgdown":
		m.vp.ScrollDown(m.vp.Height() / 2)
		return m, nil
	case "left":
		// a draft owns its caret; paging needs an empty composer
		if draftEmpty {
			m.page(-1)
			return m, nil
		}
	case "right":
		if draftEmpty {
			m.page(1)
			return m, nil
		}
	case "up":
		if draftEmpty {
			if n := len(m.shownDay().Entries); n > 0 {
				m.sel = n - 1
				m.refreshTimeline(false)
			}
			return m, nil
		}
	case "down":
		if draftEmpty {
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.layout()
	return m, cmd
}

// ---- view ---------------------------------------------------------------

func (m appModel) helpLine() string {
	switch {
	case m.saveErr != "":
		return warnStyle.Render(m.saveErr)
	case m.status != "":
		return m.status
	case m.exporting:
		return warnStyle.Render("export: [1] copy day · [2] copy history · [3] save day .txt · [4] save history .txt · esc")
	case m.confirming:
		return warnStyle.Render("delete this entry? y/n")
	case m.editing:
		return warnStyle.Render("editing " + fmtTime(m.editingAt, m.use24h) + " · enter save · esc discard")
	case m.sel >= 0:
		return dimStyle.Render("↑/↓ pick · e edit · d delete · esc back")
	default:
		return dimStyle.Render("enter log · ↑ pick entry · ←/→ days · ctrl+e export · ctrl+c quit")
	}
}

func (m appModel) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	d := m.shownDay()
	label := labelStyle.Render(strings.ToUpper(dayLabel(entryDayTs(d, m.now), m.now)))
	left, right := dimStyle.Render("‹"), dimStyle.Render("›")
	if m.pageIdx == 0 {
		left = faintStyle.Render("‹")
	}
	if m.pageIdx == len(m.pages)-1 {
		right = faintStyle.Render("›")
	}
	head := " " + left + " " + label + " " + right
	date := dimStyle.Render(d.Key)
	pad := m.width - lipgloss.Width(head) - lipgloss.Width(date) - 1
	if pad > 0 {
		head += strings.Repeat(" ", pad) + date
	}

	// header / timeline / blank / big clock / input / help — the web ui's
	// shape: entries above, the clock already waiting at the bottom
	var b strings.Builder
	b.WriteString(head + "\n")
	b.WriteString(m.vp.View() + "\n")
	b.WriteString("\n")
	b.WriteString(" " + m.composerClock() + "\n")
	b.WriteString(m.ta.View() + "\n")
	b.WriteString(" " + m.helpLine())

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "journal"
	if c := m.ta.Cursor(); c != nil && !m.exporting && !m.confirming && m.sel < 0 {
		c.Position.Y += 1 + m.vp.Height() + 2
		v.Cursor = c
	}
	return v
}

// entryDayTs gives a timestamp inside the shown day for labelling; an empty
// (today) page labels off `now`.
func entryDayTs(d day, now int64) int64 {
	if len(d.Entries) > 0 {
		return d.Entries[0].At
	}
	return now
}

// version is stamped by goreleaser at release time.
var version = "dev"

func main() {
	use24h := os.Getenv("JOURNAL_24H") == "1"
	for _, a := range os.Args[1:] {
		switch a {
		case "-24h", "--24h":
			use24h = true
		case "-v", "--version":
			fmt.Println("journal", version)
			return
		case "-h", "--help":
			fmt.Println("journal — interstitial journaling in the terminal")
			fmt.Println()
			fmt.Println("usage: journal [-24h]")
			fmt.Println()
			fmt.Println("  -24h           24-hour clock (or JOURNAL_24H=1)")
			fmt.Println("  -v, --version  print version")
			fmt.Println("  -h, --help     this help")
			return
		}
	}
	if err := lockStore(); err != nil {
		fmt.Fprintln(os.Stderr, "journal:", err)
		os.Exit(1)
	}
	entries, err := loadEntries()
	if err != nil {
		fmt.Fprintln(os.Stderr, "journal:", err)
		os.Exit(1)
	}
	p := tea.NewProgram(newApp(entries, use24h))
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "journal:", err)
		os.Exit(1)
	}
}
