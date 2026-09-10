// Package tui renders the controller's live state in the terminal.
//
// It runs in one of two modes, and the difference is deliberately invisible to
// the renderer. If this process owns the control loop, snapshots arrive from
// the loop itself; if another process already owns it, they are rebuilt by
// scraping that process's /metrics. Everything below sees the same
// state.Snapshot either way, so there is one layout to get right rather than
// two that drift apart.
//
// Both modes poll rather than being pushed to. A control loop that has to wait
// on a renderer to accept a snapshot is a control loop that can be stalled by a
// slow terminal, and that is not a trade worth making for a display.
package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/state"
)

const (
	// refresh is how often the model re-reads the shared slot. It is unrelated
	// to the control interval; it only needs to be fast enough that the "N ago"
	// fields do not visibly lag.
	refresh = 500 * time.Millisecond
	// scrapeEvery paces VIEWING mode. The daemon cycles far less often, but its
	// counters and timestamps move in between, and a scrape is cheap.
	scrapeEvery = 2 * time.Second
	// noticeFor is how long a keypress acknowledgement stays on screen.
	noticeFor = 6 * time.Second
	barWidth  = 20
	// histLen caps the trend series. At the default 30s interval this is two
	// hours, which is far more than any sparkline will draw; the surplus is so
	// the window stays full after a spell of slower cycles.
	histLen = 240
	// twoColumnAt is the terminal width from which the trends pane earns its
	// place beside the tables rather than squeezing them.
	twoColumnAt = 104
	// trendCells caps how much history a chart shows, however wide the
	// terminal is. Given every spare column of a 250 column window it drew a
	// window over an hour long, nearly all of it empty, which reads as a line
	// with a smudge on the end rather than as a trend. Sixty cells is half an
	// hour at the default interval.
	trendCells = 60
	// trendMin is the narrowest trends pane worth drawing: axis, legend and
	// enough cells for the shape to mean anything.
	trendMin = 34
	// trendAxisW is the column the axis numbers sit in, wide enough for three
	// digits and a sign. Only the top and bottom rows carry one.
	trendAxisW = 4
	// trendMinSpanC is the narrowest temperature range the chart will draw. A
	// quiet machine must look like a quiet machine: fitted exactly, a set of
	// sensors all sitting within a tenth of a degree fills the chart with noise
	// magnified to full height, which is the last thing a fan controller should
	// encourage anyone to react to.
	trendMinSpanC = 5.0
	// brailleRows is the dot grid inside one braille cell, four rows of two.
	// It is what makes a line chart possible in a character grid at all: the
	// block characters can only fill a cell from the bottom, so a flat series
	// has nowhere to sit but as a solid slab.
	brailleRows = 4
)

// Ctl is the slice of the controller the UI can drive.
//
// It is nil in VIEWING mode. That is the enforcement, not a convention: when
// another process owns the loop there is no object here to call, so no keypress
// can reach a BMC that something else is already writing to.
type Ctl interface {
	Reload() error
	SetDryRun(bool)
	DryRun() bool
	Trigger()
}

// App holds the state shared between the producer (loop or scraper) and the
// bubbletea model.
type App struct {
	cfg     *config.Config
	sink    *LogSink
	ctl     Ctl
	scraper *Scraper

	mu        sync.Mutex
	snap      state.Snapshot
	haveSnap  bool
	scrapeErr string

	// hist is one temperature series per sensor group, oldest first, for the
	// trend sparklines. It is appended per control cycle rather than per
	// refresh: in VIEWING mode the scraper runs every couple of seconds
	// against a daemon that may only cycle every thirty, and sampling the same
	// value fifteen times would draw a flat line through real movement.
	hist map[string][]float64
	// lastRecorded is the cycle already in hist, so a repeated read of it is
	// not counted twice.
	lastRecorded time.Time
}

// NewApp wires the UI. Pass a Ctl to drive the loop, or a Scraper to watch
// someone else's; exactly one of them is expected.
func NewApp(cfg *config.Config, sink *LogSink, ctl Ctl, scraper *Scraper) *App {
	return &App{cfg: cfg, sink: sink, ctl: ctl, scraper: scraper, hist: map[string][]float64{}}
}

// Publish is the controller's Observer. It is called on the loop goroutine and
// only ever takes an uncontended mutex.
func (a *App) Publish(s state.Snapshot) {
	a.mu.Lock()
	a.snap, a.haveSnap = s, true
	a.recordLocked(s)
	a.mu.Unlock()
}

// recordLocked appends this cycle's group temperatures to the trend series.
//
// Identity is the cycle's own timestamp, not the moment of observation, so the
// two producers behave the same: the loop publishes once per cycle, while the
// scraper re-reads the same cycle several times before the next one lands.
func (a *App) recordLocked(s state.Snapshot) {
	if s.LastCycle.IsZero() || s.LastCycle.Equal(a.lastRecorded) {
		return
	}
	a.lastRecorded = s.LastCycle
	for _, g := range s.Groups {
		series := append(a.hist[g.Name], g.Value)
		if len(series) > histLen {
			series = series[len(series)-histLen:]
		}
		a.hist[g.Name] = series
	}
}

func (a *App) read() (state.Snapshot, bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snap, a.haveSnap, a.scrapeErr
}

// history copies the series for one group. Copied rather than shared: the
// model renders on a different goroutine from the one appending.
func (a *App) history(group string) []float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	src := a.hist[group]
	if len(src) == 0 {
		return nil
	}
	out := make([]float64, len(src))
	copy(out, src)
	return out
}

// Run takes over the terminal until the user quits or ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if a.scraper != nil {
		go a.scrapeLoop(ctx)
	}

	p := tea.NewProgram(newModel(a), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	// A cancelled context and a Ctrl-C are both ordinary ways to leave.
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func (a *App) scrapeLoop(ctx context.Context) {
	t := time.NewTicker(scrapeEvery)
	defer t.Stop()
	for {
		snap, err := a.scraper.Scrape(ctx)
		a.mu.Lock()
		if err != nil {
			a.scrapeErr = err.Error()
		} else {
			a.snap, a.haveSnap, a.scrapeErr = snap, true, ""
			a.recordLocked(snap)
		}
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type tickMsg time.Time

type noticeMsg struct {
	text string
	bad  bool
}

type model struct {
	app *App

	w, h      int
	snap      state.Snapshot
	haveSnap  bool
	scrapeErr string

	notice      string
	noticeAt    time.Time
	noticeIsBad bool
	// started is when this view opened, for the first-cycle progress line.
	started time.Time

	// expanded lists every individual reading under its group instead of the
	// one-line summary. It is view state and lives on the model rather than on
	// App, because two attached views should not be able to fold each other's
	// panes.
	expanded bool
}

func newModel(a *App) model { return model{app: a, started: time.Now()} }

// skeleton is the shape of a snapshot with nothing measured in it yet, built
// from the config alone.
//
// The first cycle is not on a timer: the control loop runs one immediately, so
// there is no delay here to remove. What takes the time is the cycle itself,
// which opens an SSH session to a BMC that only speaks SHA-1 era key exchange,
// reads every sensor, then writes each fan floor a few hundred milliseconds
// apart because the iLO shell cannot be driven faster. Ten to twenty seconds on
// a six-fan, six-drive chassis.
//
// So the fix is not to start sooner, it is to stop showing one line of text
// while it happens. Drawing the tables from the config with every value unknown
// settles the layout on the first frame and shows what is about to be filled
// in, and the event log underneath narrates the connection as it goes.
func skeleton(cfg *config.Config) state.Snapshot {
	s := state.Snapshot{Interval: cfg.Interval.Duration}
	for _, f := range cfg.Fans {
		s.Fans = append(s.Fans, state.Fan{
			Index: f.Index, Label: f.Label,
			Floor: state.Unknown(), Observed: state.Unknown(),
		})
	}
	names := make([]string, 0, len(cfg.Sensors))
	for name := range cfg.Sensors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s.Groups = append(s.Groups, state.Group{
			Name: name, Kind: cfg.Sensors[name].Kind, Value: state.Unknown(),
		})
	}
	for _, c := range cfg.Curves {
		s.Curves = append(s.Curves, state.Curve{
			Name: c.Name, Sensor: c.Sensor,
			Temp: state.Unknown(), Demand: state.Unknown(),
		})
	}
	return s
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.snap, m.haveSnap, m.scrapeErr = m.app.read()
		if !m.noticeAt.IsZero() && time.Since(m.noticeAt) > noticeFor {
			m.notice, m.noticeAt = "", time.Time{}
		}
		return m, tick()

	case noticeMsg:
		m.notice, m.noticeIsBad, m.noticeAt = msg.text, msg.bad, time.Now()
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "e":
		// Purely a display toggle, so it works in VIEWING mode too. Nothing
		// here reaches the BMC.
		m.expanded = !m.expanded
		return m, nil
	}
	if m.app.ctl == nil {
		// Read-only attachment: say why rather than silently ignoring the key.
		switch msg.String() {
		case "r", "d", " ":
			return m, notify("another process owns the control loop; this view is read only", true)
		}
		return m, nil
	}

	ctl := m.app.ctl
	switch msg.String() {
	case "r":
		// Reload can wind down fans dropped from the config, which talks to the
		// BMC, so it runs as a command rather than on the UI goroutine.
		return m, func() tea.Msg {
			if err := ctl.Reload(); err != nil {
				return noticeMsg{"reload rejected, previous config still running", true}
			}
			return noticeMsg{"config reloaded", false}
		}
	case "d":
		on := !ctl.DryRun()
		ctl.SetDryRun(on)
		if on {
			return m, notify("dry run on: floors already set stay set, no new ones are written", true)
		}
		return m, notify("dry run off: writing floors again", false)
	case " ":
		ctl.Trigger()
		return m, notify("cycle requested", false)
	}
	return m, nil
}

func notify(text string, bad bool) tea.Cmd {
	return func() tea.Msg { return noticeMsg{text, bad} }
}

// Styles. AdaptiveColor so the thing is readable on a light terminal too.
var (
	cOK    = lipgloss.AdaptiveColor{Light: "22", Dark: "42"}
	cWarn  = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	cBad   = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	cDim   = lipgloss.AdaptiveColor{Light: "245", Dark: "241"}
	cLabel = lipgloss.AdaptiveColor{Light: "25", Dark: "111"}
	// The footer bar. Brighter than cDim on purpose: it sits on a background,
	// and the whole point is that it stops reading as another log line.
	cBarFG = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
	cBarBG = lipgloss.AdaptiveColor{Light: "253", Dark: "238"}

	sOK      = lipgloss.NewStyle().Foreground(cOK)
	sWarn    = lipgloss.NewStyle().Foreground(cWarn)
	sBad     = lipgloss.NewStyle().Foreground(cBad)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sHead    = lipgloss.NewStyle().Foreground(cLabel).Bold(true)
	sSection = lipgloss.NewStyle().Foreground(cLabel).Bold(true)
	sBar     = lipgloss.NewStyle().Foreground(cBarFG).Background(cBarBG)
	sKey     = lipgloss.NewStyle().Foreground(cLabel).Background(cBarBG).Bold(true)

	// cSeries identifies the lines in the trend chart, which overlays every
	// group on one axis. A terminal cell has a single foreground colour, so
	// colour is the only thing telling one line from another, and the legend
	// is keyed on it.
	//
	// Warn and bad are deliberately absent. Orange and red mean something
	// specific everywhere else on this screen, and a group must not appear to
	// be in trouble because of the order it was dealt a colour in.
	cSeries = []lipgloss.AdaptiveColor{
		{Light: "22", Dark: "42"},   // green
		{Light: "31", Dark: "44"},   // cyan
		{Light: "92", Dark: "141"},  // violet
		{Light: "26", Dark: "75"},   // blue
		{Light: "162", Dark: "212"}, // pink
		{Light: "23", Dark: "80"},   // teal
	}
	sSeries = func() []lipgloss.Style {
		out := make([]lipgloss.Style, len(cSeries))
		for i, c := range cSeries {
			out[i] = lipgloss.NewStyle().Foreground(c)
		}
		return out
	}()
)

// seriesStyle is the colour of the nth line, wrapping when there are more
// groups than colours. Two groups sharing a colour is worse than the palette
// being short, but the alternative is a group with no line at all.
func seriesStyle(i int) lipgloss.Style { return sSeries[i%len(sSeries)] }

func (m model) View() string {
	width := m.w
	if width < 40 {
		width = 80
	}
	height := m.h
	if height < 1 {
		height = 24
	}

	var b strings.Builder
	b.WriteString(m.header(width) + "\n")
	for _, a := range m.alerts(width) {
		b.WriteString(a + "\n")
	}
	b.WriteString("\n")

	if !m.haveSnap {
		b.WriteString(sDim.Render(fmt.Sprintf(
			"  opening the BMC session and taking the first readings, %s so far",
			uptime(m.started))) + "\n\n")
		skel := m
		skel.snap = skeleton(m.app.cfg)
		b.WriteString(skel.body(width) + "\n")
	} else {
		b.WriteString(m.body(width) + "\n")
	}

	// Everything written so far ends in exactly one newline, which Height
	// counts as a line of its own. Below this go the events heading and the
	// footer, one line each.
	used := lipgloss.Height(b.String()) - 1

	// The events pane takes every remaining row and pads itself out, so the
	// frame is always exactly as tall as the terminal and the footer always
	// sits on the last line.
	//
	// This is what fixes the two footers on the DL380, and the cause was not
	// the frame being too tall: it was the frame CHANGING height. Anything
	// above that comes and goes, an alert clearing or a wrapped line, made the
	// next frame shorter, and the tail of the taller one stayed on the screen
	// underneath it. A constant height cannot leave a tail. The clamp below is
	// the other half, for a terminal too short to hold the tables at all.
	room := height - used - 2
	if room < 1 {
		room = 1
	}
	b.WriteString(m.eventPane(room, width) + "\n")
	b.WriteString(m.footer(width))
	return clampLines(b.String(), height)
}

func clampLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// truncate cuts a string to n cells, rune-safe. Slicing a string by byte
// splits a multi-byte rune and prints a replacement character; every string
// here can carry a degree sign or an ellipsis from an earlier truncation.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// body lays out the three tables, putting the trend sparklines beside them
// when the terminal is wide enough to carry both.
//
// The trends are anchored just past the tables at their COLLAPSED width, and
// take every column after that.
//
// Two rules fighting each other produced this. Anchoring on whatever the tables
// currently measure closes the gap but slides the pane sideways and rescales
// every chart the moment e is pressed, and a chart whose horizontal scale moves
// on its own is worse than no chart. Giving the pane a fixed share of the
// window keeps it still but leaves a dead gap, because these tables are around
// sixty columns wide and a wide terminal is not. Measuring the collapsed tables
// gets both: the anchor does not depend on the toggle, and expanding packs the
// reading grid into the same column instead of pushing anything.
func (m model) body(width int) string {
	if width < twoColumnAt {
		return m.tables(width)
	}
	collapsed := m
	collapsed.expanded = false
	// Measured at the full width so nothing is truncated: this is the width
	// the tables want, not the width they would be squeezed into.
	at := lipgloss.Width(collapsed.tables(width)) + 2
	if limit := width - trendMin; at > limit {
		at = limit
	}
	trends := m.trends(width - at)
	if trends == "" {
		return m.tables(width)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(at).Render(m.tables(at-2)), trends)
}

func (m model) tables(width int) string {
	return m.fans() + "\n" + m.sensors(width) + "\n" + m.curves()
}

// brailleDots maps a sub column (0 left, 1 right) and a dot row (0 topmost) to
// its bit in the braille block at U+2800.
//
// The order is not the obvious one. Dots 1 to 6 were the original two by three
// cell and dots 7 and 8 were added underneath them later, so the bottom row of
// the grid is at the top of the byte.
var brailleDots = [2][brailleRows]rune{
	{0x01, 0x02, 0x04, 0x40},
	{0x08, 0x10, 0x20, 0x80},
}

// trends draws every sensor group as a line on one shared temperature axis.
//
// One axis rather than one per group, so height means the same thing wherever
// you look at it: the inlet really is at the bottom of the chart and the HBA
// really is at the top, and a group that has not moved is a flat line at its
// own temperature rather than a shape scaled up out of nothing. The previous
// design gave each group its own range, which drew a tenth of a degree of
// noise identically to a twenty degree ramp.
//
// What that costs is fine detail: a group moving one degree on a forty degree
// axis moves one step. The numbers beside each line carry the precision, the
// chart carries the shape, and SENSORS carries the readings behind it.
func (m model) trends(width int) string {
	groups := m.snap.Groups
	if len(groups) == 0 {
		return ""
	}
	nameW := 5
	for _, g := range groups {
		if len(g.Name) > nameW {
			nameW = len(g.Name)
		}
	}
	// The axis number, the axis, a gap, then the legend: name, space, value.
	cells := width - (trendAxisW + 1 + 2 + nameW + 7)
	if cells > trendCells {
		cells = trendCells
	}
	if cells < 8 {
		return ""
	}
	// One row per group, which is the height the pane already had, and the
	// least that lets every line have a row of its own when they converge.
	// Never fewer than two, because the axis needs a top and a bottom.
	rows := max(len(groups), 2)

	series := make([][]float64, len(groups))
	for i, g := range groups {
		series[i] = alignRight(m.app.history(g.Name), cells)
	}
	p := newPlot(series, cells, rows)

	var b strings.Builder
	b.WriteString(sSection.Render("TRENDS") + "\n")
	for r := 0; r < rows; r++ {
		axis := strings.Repeat(" ", trendAxisW)
		switch {
		case !p.ok:
		case r == 0:
			axis = fmt.Sprintf("%*.0f", trendAxisW, p.hi)
		case r == rows-1:
			axis = fmt.Sprintf("%*.0f", trendAxisW, p.lo)
		}
		b.WriteString(sDim.Render(axis+"┤") + p.renderRow(r))
		for i, g := range groups {
			if p.row[i] != r {
				continue
			}
			// The legend sits on the row its line is in at the right hand
			// edge, which is where the eye leaves the chart, so nothing has to
			// be matched up by colour alone.
			val := sDim.Render(fmt.Sprintf("%6s", "-"))
			if state.Known(g.Value) {
				st := sDim
				if g.Blind {
					// The line has already stopped: a blind group's readings
					// are gaps. This says the number beside it is old.
					st = sWarn
				}
				val = st.Render(fmt.Sprintf("%5.1fC", g.Value))
			}
			b.WriteString("  " + seriesStyle(i).Render(fmt.Sprintf("%-*s", nameW, g.Name)) + " " + val)
			break
		}
		b.WriteString("\n")
	}
	b.WriteString(sDim.Render(fmt.Sprintf("%*s %s per cell, oldest left",
		trendAxisW, "", m.snap.Interval)) + "\n")
	return b.String()
}

// trendCell is one character of the chart: what to draw and which series drew
// it, since that decides the colour.
type trendCell struct {
	glyph rune
	owner int // index into the series slice, -1 for nobody
}

// plot is every series rendered into a character grid, ready to colour.
type plot struct {
	grid [][]trendCell
	// row is where each series' legend belongs: the row its line occupies at
	// the right hand edge. A series with nothing to draw still gets one, so
	// every group is listed.
	row    []int
	lo, hi float64
	// ok is false when no group has been read yet, so there is no axis.
	ok bool
}

// newPlot lays every series out on one axis.
func newPlot(series [][]float64, cells, rows int) plot {
	p := plot{row: make([]int, len(series)), grid: make([][]trendCell, rows)}
	for i := range p.row {
		p.row[i] = -1
	}
	for r := range p.grid {
		p.grid[r] = make([]trendCell, cells)
		for c := range p.grid[r] {
			p.grid[r][c] = trendCell{glyph: ' ', owner: -1}
		}
	}
	if cells < 1 || rows < 1 {
		return p
	}

	for _, s := range series {
		for _, v := range s {
			if !state.Known(v) {
				continue
			}
			if !p.ok {
				p.lo, p.hi, p.ok = v, v, true
				continue
			}
			p.lo, p.hi = math.Min(p.lo, v), math.Max(p.hi, v)
		}
	}
	if !p.ok {
		p.spreadLegend(nil, cells, rows)
		return p
	}
	if span := p.hi - p.lo; span < trendMinSpanC {
		mid := (p.lo + p.hi) / 2
		p.lo, p.hi = mid-trendMinSpanC/2, mid+trendMinSpanC/2
	}

	levels := rows * brailleRows
	level := make([][]int, len(series))
	for i, s := range series {
		level[i] = make([]int, cells)
		first := -1
		for c, v := range s {
			level[i][c] = -1
			if !state.Known(v) {
				// A gap is drawn as a gap. Interpolating across an unreadable
				// sensor would invent the very reading the collector refused
				// to give.
				continue
			}
			if first < 0 {
				first = c
			}
			level[i][c] = clampInt(int(math.Round(
				(v-p.lo)/(p.hi-p.lo)*float64(levels-1))), 0, levels-1)
		}
		// The oldest reading is held flat out to the left edge, so the chart
		// has its full width from the first frame and pans left as it fills
		// rather than growing out of a stub.
		for c := 0; c < first; c++ {
			level[i][c] = level[i][first]
		}
	}

	rowOf := assignRows(level, cells, rows)
	for i := range series {
		prevRow, prevSub := -1, -1
		for c := 0; c < cells; c++ {
			if level[i][c] < 0 {
				prevRow, prevSub = -1, -1
				continue
			}
			r := rowOf[i][c]
			// The height within the row is the one the value asks for, not one
			// measured from the row the series was pushed into, so a nudged
			// line is the same shape translated bodily downwards. Measuring it
			// from the new row instead pinned it against the row's edge and
			// flattened it: three sensors within a few degrees all became
			// straight lines, which is the whole complaint this chart exists
			// to answer.
			sub := (levels - 1 - level[i][c]) % brailleRows
			left := sub
			if r == prevRow {
				// Within one row a step is drawn as a slope, so a rise reads
				// as a rise and not as two disconnected heights.
				left = prevSub
			}
			p.grid[r][c] = trendCell{
				glyph: 0x2800 + brailleDots[0][left] + brailleDots[1][sub],
				owner: i,
			}
			prevRow, prevSub = r, sub
		}
	}
	p.spreadLegend(rowOf, cells, rows)
	return p
}

// assignRows decides which character row each series occupies in each column.
//
// It has to, rather than the level alone deciding, because a terminal cell has
// a single foreground colour: two lines in one cell cannot both keep the
// colour they are identified by. Where they would collide the colder one is
// pushed down into the next free row, so their order stays true even though
// the gap between them is drawn wider than it is. A series colliding with
// nothing sits exactly where its value puts it, which is the usual case.
func assignRows(level [][]int, cells, rows int) [][]int {
	out := make([][]int, len(level))
	for i := range out {
		out[i] = make([]int, cells)
	}
	order := make([]int, 0, len(level))
	for c := 0; c < cells; c++ {
		order = order[:0]
		for i := range level {
			out[i][c] = -1
			if level[i][c] >= 0 {
				order = append(order, i)
			}
		}
		// Hottest first, ties broken by the group's own order so the picture
		// does not reshuffle between frames.
		sort.SliceStable(order, func(a, b int) bool {
			return level[order[a]][c] > level[order[b]][c]
		})
		prev := -1
		for _, i := range order {
			r := (rows*brailleRows - 1 - level[i][c]) / brailleRows
			if r <= prev {
				r = prev + 1
			}
			out[i][c] = r
			prev = r
		}
		// Pushing down can run off the bottom, so the same pass runs back up
		// to take up the slack. There is always room: one row per group.
		next := rows
		for k := len(order) - 1; k >= 0; k-- {
			i := order[k]
			if out[i][c] >= next {
				out[i][c] = next - 1
			}
			out[i][c] = clampInt(out[i][c], 0, rows-1)
			next = out[i][c]
		}
	}
	return out
}

// spreadLegend gives every series a row for its label, claiming the row its
// line is in at the rightmost column it appears in. A series that has never
// been read, or whose row is already spoken for, takes the first free one.
func (p *plot) spreadLegend(rowOf [][]int, cells, rows int) {
	used := make([]bool, rows)
	for c := cells - 1; c >= 0 && rowOf != nil; c-- {
		for i := range p.row {
			if p.row[i] >= 0 || rowOf[i][c] < 0 || used[rowOf[i][c]] {
				continue
			}
			p.row[i], used[rowOf[i][c]] = rowOf[i][c], true
		}
	}
	for i := range p.row {
		if p.row[i] >= 0 {
			continue
		}
		for r := 0; r < rows; r++ {
			if !used[r] {
				p.row[i], used[r] = r, true
				break
			}
		}
	}
}

// renderRow colours one row of the chart, one escape sequence per run of cells
// belonging to the same series rather than one per cell.
func (p plot) renderRow(r int) string {
	var b strings.Builder
	row := p.grid[r]
	for i := 0; i < len(row); {
		j := i
		for j < len(row) && row[j].owner == row[i].owner {
			j++
		}
		run := make([]rune, 0, j-i)
		for _, c := range row[i:j] {
			run = append(run, c.glyph)
		}
		if row[i].owner < 0 {
			b.WriteString(string(run))
		} else {
			b.WriteString(seriesStyle(row[i].owner).Render(string(run)))
		}
		i = j
	}
	return b.String()
}

// alignRight puts a series into exactly cells samples, newest against the
// right edge and the left padded with unknowns, so "now" stays at a fixed
// column whatever the terminal is doing and however much history there is.
func alignRight(series []float64, cells int) []float64 {
	out := make([]float64, cells)
	for i := range out {
		out[i] = state.Unknown()
	}
	if n := len(series); n > 0 {
		if n > cells {
			series, n = series[n-cells:], cells
		}
		copy(out[cells-n:], series)
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mode is what this process is doing right now, which is not always what the
// snapshot says. The snapshot carries the mode of the cycle that produced it
// and is up to one interval old, so toggling the dry run from the keyboard
// left the badge and the alert contradicting the acknowledgement for half a
// minute: "DRY RUN" and "dry run off: writing floors again" on screen
// together. When this process owns the control loop, the controller is the
// authority on its own mode; only an attached read-only view has to believe
// the snapshot.
func (m model) mode() state.Mode {
	if m.app.ctl == nil {
		if m.snap.Mode != "" {
			return m.snap.Mode
		}
		return state.ModeViewing
	}
	if m.app.ctl.DryRun() {
		return state.ModeDryRun
	}
	return state.ModeControlling
}

func (m model) header(width int) string {
	s := m.snap
	mode := m.mode()
	badge := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	switch mode {
	case state.ModeControlling:
		badge = badge.Foreground(lipgloss.Color("232")).Background(cOK)
	case state.ModeDryRun:
		badge = badge.Foreground(lipgloss.Color("232")).Background(cWarn)
	default:
		badge = badge.Foreground(lipgloss.Color("232")).Background(cLabel)
	}

	host := m.app.cfg.ILO.Host
	if s.BMCHost != "" {
		host = s.BMCHost
	}
	// Three states, not two. A handshake in flight is not a fault, and iLO 4
	// takes long enough over one that reporting "no session" while it runs
	// puts a red word on screen for the first several seconds of every
	// startup. Red is reserved for a session that failed or was never tried.
	link := sBad.Render("no session")
	switch {
	case s.Connected:
		link = sOK.Render("connected")
	case s.Connecting, !m.haveSnap && m.app.ctl != nil:
		link = sWarn.Render("connecting")
	}

	right := "cycle " + ago(s.LastCycle) + "  up " + uptime(s.StartedAt)
	left := fmt.Sprintf("%s %s  %s %s",
		sHead.Render("ilo-fanctl"), badge.Render(string(mode)),
		sDim.Render(host), link)

	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + sDim.Render(right)
}

// alerts surfaces the states that are worth interrupting the reader for, worst
// first. Every one of them is a reason the machine is not being cooled the way
// the config says it should be.
//
// Each is wrapped to the terminal rather than left to the terminal's own
// wrapping, so that the line count the caller measures is the line count the
// screen shows. The mismatch alert names every offending fan, so on a six-fan
// chassis it is routinely two or three lines long.
func (m model) alerts(width int) []string {
	s := m.snap
	var out []string
	add := func(st lipgloss.Style, text string) {
		out = append(out, st.Width(width).Render("  "+text))
	}
	if m.scrapeErr != "" {
		add(sBad, "cannot reach the running daemon: "+m.scrapeErr)
	}
	if s.Critical {
		add(sBad, "CRITICAL  a threshold is exceeded, every fan is at the maximum floor")
	}
	if bad := mismatched(s.Fans); len(bad) > 0 {
		add(sBad, fmt.Sprintf(
			"the BMC is running %s below the commanded floor; an HPE firmware upgrade reverts the ilo4_unlock patch",
			strings.Join(bad, ", ")))
	}
	if s.Err != "" {
		add(sBad, "last cycle failed: "+s.Err)
	}
	if s.BlindGroups > 0 {
		add(sWarn, fmt.Sprintf(
			"%d sensor group(s) unreadable, running on held or fallback values", s.BlindGroups))
	}
	if m.mode() == state.ModeViewing && s.ConfigChecksum != "" &&
		m.app.cfg.Checksum != "" && s.ConfigChecksum != m.app.cfg.Checksum {
		add(sWarn, "the config on disk is not the one the daemon is running; labels below may be stale")
	}
	if m.mode() == state.ModeDryRun {
		add(sWarn, "dry run: floors are computed and logged but never written")
	}
	if m.notice != "" {
		st := sOK
		if m.noticeIsBad {
			st = sWarn
		}
		add(st, m.notice)
	}
	return out
}

func mismatched(fans []state.Fan) []string {
	var out []string
	for _, f := range fans {
		if f.Mismatch {
			out = append(out, fanName(f))
		}
	}
	return out
}

func fanName(f state.Fan) string {
	if f.Label == "" {
		return fmt.Sprintf("fan %d", f.Index)
	}
	return fmt.Sprintf("fan %d (%s)", f.Index, f.Label)
}

func (m model) fans() string {
	var b strings.Builder
	b.WriteString(sSection.Render("FANS") + "\n")
	b.WriteString(sDim.Render(fmt.Sprintf("  %-3s %-*s %8s %9s",
		"idx", labelWidth(m.snap.Fans), "label", "floor", "observed")) + "\n")
	for _, f := range m.snap.Fans {
		row := fmt.Sprintf("  %-3d %-*s %8s %9s  ",
			f.Index, labelWidth(m.snap.Fans), f.Label, pct(f.Floor), pct(f.Observed))
		st := sOK
		if f.Mismatch {
			st = sBad
		} else if !state.Known(f.Observed) {
			st = sDim
		}
		b.WriteString(row + st.Render(bar(f.Floor, f.Observed)) + "\n")
	}
	return b.String()
}

func labelWidth(fans []state.Fan) int {
	w := 5
	for _, f := range fans {
		if len(f.Label) > w {
			w = len(f.Label)
		}
	}
	if w > 24 {
		w = 24
	}
	return w
}

// bar draws the observed speed, with the commanded floor marked. Seeing the two
// separately is the point: the BMC's own curve routinely runs a fan above the
// floor, and that is correct, while a fan below its marker is the failure this
// program exists to detect.
func bar(floor, observed float64) string {
	cells := make([]rune, barWidth)
	fill := scale(observed)
	for i := range cells {
		switch {
		case i < fill:
			cells[i] = '█'
		default:
			cells[i] = '·'
		}
	}
	if state.Known(floor) {
		if at := scale(floor); at > 0 && at <= barWidth {
			cells[at-1] = '|'
		}
	}
	return string(cells)
}

func scale(v float64) int {
	if !state.Known(v) || v <= 0 {
		return 0
	}
	n := int(math.Round(v / 100 * barWidth))
	if n > barWidth {
		n = barWidth
	}
	return n
}

func (m model) sensors(width int) string {
	var b strings.Builder
	b.WriteString(sSection.Render("SENSORS") + "\n")
	gw := 5
	for _, g := range m.snap.Groups {
		if len(g.Name) > gw {
			gw = len(g.Name)
		}
	}
	b.WriteString(sDim.Render(fmt.Sprintf("  %-*s %-6s %8s  %s", gw, "group", "kind", "value", "detail")) + "\n")
	for _, g := range m.snap.Groups {
		st := sOK
		switch {
		case g.Blind && !state.Known(g.Value):
			st = sBad
		case g.Blind, g.Held:
			st = sWarn
		}
		detail := groupDetail(g)
		row := fmt.Sprintf("  %-*s %-6s %8s  ", gw, g.Name, g.Kind, celsius(g.Value))
		if room := width - lipgloss.Width(row); room > 4 {
			detail = truncate(detail, room)
		}
		b.WriteString(row + st.Render(detail) + "\n")
		if m.expanded {
			b.WriteString(readingGrid(g, width))
		}
	}
	return b.String()
}

// readingGrid lists a group's individual sensors under it, packed into as many
// columns as the width allows.
//
// The aggregate above is a max over these, and a max hides its own inputs: six
// drives at 40C and five at 40C with one at 54C produce the same "hottest"
// line only if you already know which drive it was. This is the view that
// answers "which one", and it is off by default because six extra rows per
// group is most of a screen.
func readingGrid(g state.Group, width int) string {
	if len(g.Readings) == 0 {
		return ""
	}
	nameW := 0
	for _, r := range g.Readings {
		if len(r.Name) > nameW {
			nameW = len(r.Name)
		}
	}
	const indent = "      "
	// name, space, "nnn.nC", two of gutter.
	cell := nameW + 1 + 6 + 2
	cols := (width - len(indent)) / cell
	if cols < 1 {
		cols = 1
	}

	// The hottest is what the aggregate took, so it is coloured; the rest are
	// dim, which is what makes the outlier findable without reading numbers.
	hot := 0
	for i, r := range g.Readings {
		if state.Known(r.Celsius) && (!state.Known(g.Readings[hot].Celsius) || r.Celsius > g.Readings[hot].Celsius) {
			hot = i
		}
	}

	var b strings.Builder
	for i, r := range g.Readings {
		if i%cols == 0 {
			b.WriteString(indent)
		}
		st := sDim
		switch {
		case !state.Known(r.Celsius):
			st = sBad
		case i == hot:
			st = sWarn
		}
		b.WriteString(st.Render(fmt.Sprintf("%-*s %6s", nameW, r.Name, celsius(r.Celsius))))
		if i%cols == cols-1 || i == len(g.Readings)-1 {
			b.WriteString("\n")
		} else {
			b.WriteString("  ")
		}
	}
	return b.String()
}

// groupDetail says where the group's number came from, which matters more than
// the individual readings: an aggregate of six drives is only as trustworthy as
// the count behind it.
func groupDetail(g state.Group) string {
	// The error text is not a metric, so a scraped view knows a group is blind
	// but not why. Appending an empty reason would leave a dangling colon.
	because := ""
	if g.Err != "" {
		because = ": " + g.Err
	}
	switch {
	case g.Blind && g.Held:
		return "unreadable, holding the last good value" + because
	case g.Blind && state.Known(g.Value):
		return "partial read" + because
	case g.Blind:
		return "unreadable, on the fixed fallback" + because
	}
	switch len(g.Readings) {
	case 0:
		return ""
	case 1:
		return g.Readings[0].Name
	}
	hot := g.Readings[0]
	for _, r := range g.Readings[1:] {
		if r.Celsius > hot.Celsius {
			hot = r
		}
	}
	return fmt.Sprintf("%d readings, hottest %s %s", len(g.Readings), hot.Name, celsius(hot.Celsius))
}

func (m model) curves() string {
	var b strings.Builder
	b.WriteString(sSection.Render("CURVES") + "\n")
	nw, sw := 5, 6
	for _, c := range m.snap.Curves {
		if len(c.Name) > nw {
			nw = len(c.Name)
		}
		if len(c.Sensor) > sw {
			sw = len(c.Sensor)
		}
	}
	b.WriteString(sDim.Render(fmt.Sprintf("  %-*s %-*s %8s %8s  %s",
		nw, "curve", sw, "sensor", "temp", "demand", "")) + "\n")
	for _, c := range m.snap.Curves {
		note := ""
		st := sDim
		if c.Driving {
			note, st = "driving", sOK
		}
		if c.Fallback {
			note, st = "sensor lost, using the fixed fallback", sWarn
		}
		b.WriteString(fmt.Sprintf("  %-*s %-*s %8s %8s  %s\n",
			nw, c.Name, sw, c.Sensor, celsius(c.Temp), pct(c.Demand), st.Render(note)))
	}
	return b.String()
}

// eventPane always returns exactly rows lines under its heading, padding with
// blanks when the daemon has not logged that much yet. The caller depends on
// the count to keep the frame a constant height.
func (m model) eventPane(rows, width int) string {
	// Deliberately not make([]string, 0, rows): slicing to rows would then
	// reach into the spare capacity and pad with empty strings on its own,
	// which silently does the padding loop's job and leaves the loop looking
	// removable when it is the only thing keeping the frame a constant height.
	var lines []string
	events := m.app.sink.Events(rows)
	if len(events) == 0 {
		lines = append(lines, sDim.Render("  (nothing logged yet)"))
	}
	for _, e := range events {
		st := sDim
		switch {
		case e.Level >= slog.LevelError:
			st = sBad
		case e.Level >= slog.LevelWarn:
			st = sWarn
		}
		lines = append(lines, st.Render(truncate(fmt.Sprintf("  %s %-5s %s %s",
			e.At.Format("15:04:05"), e.Level.String(), e.Msg, e.Attrs), width)))
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return sSection.Render("EVENTS") + "\n" + strings.Join(lines, "\n")
}

// footer is a full-width bar rather than another dim line. It sat directly
// under the event log in the same style, so on a real terminal it read as one
// more log entry rather than as the key legend.
func (m model) footer(width int) string {
	keys := [][2]string{{"q", "quit"}}
	if m.expanded {
		keys = append(keys, [2]string{"e", "collapse sensors"})
	} else {
		keys = append(keys, [2]string{"e", "expand sensors"})
	}
	if m.app.ctl != nil {
		dry := "off"
		if m.app.ctl.DryRun() {
			dry = "on"
		}
		keys = append(keys,
			[2]string{"r", "reload config"},
			[2]string{"d", "dry run (" + dry + ")"},
			[2]string{"space", "cycle now"})
	}

	var b strings.Builder
	b.WriteString(sBar.Render("  "))
	for i, k := range keys {
		if i > 0 {
			b.WriteString(sBar.Render("   "))
		}
		b.WriteString(sKey.Render(k[0]) + sBar.Render(" "+k[1]))
	}
	if m.app.ctl == nil {
		b.WriteString(sBar.Render("   read only: another process owns the control loop"))
	}
	if pad := width - lipgloss.Width(b.String()); pad > 0 {
		b.WriteString(sBar.Render(strings.Repeat(" ", pad)))
	}
	return b.String()
}

func pct(v float64) string {
	if !state.Known(v) {
		return "   -"
	}
	return fmt.Sprintf("%.0f%%", math.Round(v))
}

func celsius(v float64) string {
	if !state.Known(v) {
		return "   -"
	}
	return fmt.Sprintf("%.1fC", v)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return uptime(t) + " ago"
}

func uptime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
