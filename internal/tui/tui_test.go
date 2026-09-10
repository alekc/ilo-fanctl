package tui

import (
	"bytes"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/state"
)

func TestLogSinkKeepsTheNewestRecords(t *testing.T) {
	s := NewLogSink(3, slog.LevelInfo)
	log := slog.New(s)
	for i := range 5 {
		log.Info("line", "n", i)
	}

	got := s.Events(0)
	if len(got) != 3 {
		t.Fatalf("kept %d records, want the cap of 3", len(got))
	}
	if got[0].Attrs != "n=2" || got[2].Attrs != "n=4" {
		t.Errorf("kept %q..%q, want n=2..n=4", got[0].Attrs, got[2].Attrs)
	}
	if tail := s.Events(2); len(tail) != 2 || tail[1].Attrs != "n=4" {
		t.Errorf("Events(2) = %v, want the last two", tail)
	}
}

func TestLogSinkFiltersByLevel(t *testing.T) {
	s := NewLogSink(10, slog.LevelWarn)
	log := slog.New(s)
	log.Debug("no")
	log.Info("no")
	log.Warn("yes")
	log.Error("yes")

	if got := s.Events(0); len(got) != 2 {
		t.Fatalf("kept %d records, want only the warn and the error", len(got))
	}
}

// A derived logger has to land in the same pane. Handlers that copy the sink
// would each get their own buffer, and half the daemon's output would silently
// never reach the screen.
func TestDerivedLoggersShareTheBuffer(t *testing.T) {
	s := NewLogSink(10, slog.LevelInfo)
	slog.New(s).With("fan", 0).WithGroup("bmc").Info("wrote", "raw", 51)

	got := s.Events(0)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !strings.Contains(got[0].Attrs, "fan=0") || !strings.Contains(got[0].Attrs, "raw=51") {
		t.Errorf("attrs = %q, want both the derived and the call-site attribute", got[0].Attrs)
	}
}

func TestDetachAlsoWritesToTheWriter(t *testing.T) {
	var buf bytes.Buffer
	s := NewLogSink(10, slog.LevelInfo)
	log := slog.New(s)

	log.Info("before detaching")
	if buf.Len() != 0 {
		t.Fatalf("wrote %q before Detach", buf.String())
	}
	s.Detach(&buf)
	log.Info("winding fans down", "fan", 0)

	if !strings.Contains(buf.String(), "winding fans down") || !strings.Contains(buf.String(), "fan=0") {
		t.Errorf("detached output = %q", buf.String())
	}
	if strings.Contains(buf.String(), "before detaching") {
		t.Error("Detach replayed records written before it was called")
	}
}

func TestLogSinkIsSafeUnderConcurrentWriters(t *testing.T) {
	s := NewLogSink(50, slog.LevelInfo)
	log := slog.New(s)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				log.Info("busy", "g", i, "j", j)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			_ = s.Events(10)
		}
	}()
	wg.Wait()

	if got := len(s.Events(0)); got != 50 {
		t.Errorf("kept %d records, want the cap of 50", got)
	}
}

func TestScraperURLFromListenAddress(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9873": "http://127.0.0.1:9873/metrics",
		":9873":          "http://127.0.0.1:9873/metrics",
		"0.0.0.0:9873":   "http://127.0.0.1:9873/metrics",
		"[::]:9873":      "http://127.0.0.1:9873/metrics",
		"10.0.0.5:9999":  "http://10.0.0.5:9999/metrics",
		"nonsense":       "http://127.0.0.1:9873/metrics",
	}
	for listen, want := range cases {
		if got := NewScraper(listen, &config.Config{}).URL; got != want {
			t.Errorf("NewScraper(%q).URL = %q, want %q", listen, got, want)
		}
	}
}

// The bar shows the observed speed and marks the commanded floor. The two being
// distinguishable is the whole point: a fan above its marker is the BMC's own
// curve doing its job, and a fan below it is the failure this program watches
// for.
func TestBarMarksTheFloorSeparatelyFromTheSpeed(t *testing.T) {
	for _, tc := range []struct {
		floor, observed float64
		wantMarker      bool
	}{
		{20, 20, true},
		{20, 60, true},
		{state.Unknown(), 45, false},
		{state.Unknown(), state.Unknown(), false},
	} {
		got := bar(tc.floor, tc.observed)
		if n := len([]rune(got)); n != barWidth {
			t.Errorf("bar(%v,%v) is %d cells wide, want %d", tc.floor, tc.observed, n, barWidth)
		}
		if strings.ContainsRune(got, '|') != tc.wantMarker {
			t.Errorf("bar(%v,%v) = %q, marker present = %v, want %v",
				tc.floor, tc.observed, got, !tc.wantMarker, tc.wantMarker)
		}
	}
}

type fakeCtl struct {
	mu       sync.Mutex
	dry      bool
	reloads  int
	triggers int
	err      error
}

func (f *fakeCtl) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return f.err
}
func (f *fakeCtl) SetDryRun(v bool) { f.mu.Lock(); f.dry = v; f.mu.Unlock() }
func (f *fakeCtl) DryRun() bool     { f.mu.Lock(); defer f.mu.Unlock(); return f.dry }
func (f *fakeCtl) Trigger()         { f.mu.Lock(); f.triggers++; f.mu.Unlock() }

func keyOf(s string) tea.KeyMsg {
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// The handler dispatches on Key.String(), so a helper that builds the wrong
// shape would let every key test pass against a handler that never fires.
func TestKeyHelperProducesTheKeysItClaims(t *testing.T) {
	for _, k := range []string{"q", "r", "d", " ", "esc", "ctrl+c"} {
		if got := keyOf(k).String(); got != k {
			t.Errorf("keyOf(%q).String() = %q", k, got)
		}
	}
}

func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func render(m model) string { return ansi.ReplaceAllString(m.View(), "") }

func newTestModel(ctl Ctl) model {
	app := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), ctl, nil)
	m := newModel(app)
	m.w, m.h = 100, 40
	return m
}

// A scraped view knows a group is blind but not why, because the error text is
// not a metric. It still has to read as a sentence.
func TestGroupDetailWithoutAnErrorReason(t *testing.T) {
	got := groupDetail(state.Group{Name: "cpu", Blind: true, Value: state.Unknown()})
	if strings.HasSuffix(got, ":") || strings.Contains(got, ": ") {
		t.Errorf("groupDetail with no error = %q, want no dangling colon", got)
	}
	if withErr := groupDetail(state.Group{Blind: true, Value: state.Unknown(), Err: "boom"}); !strings.Contains(withErr, ": boom") {
		t.Errorf("groupDetail with an error = %q, want the reason appended", withErr)
	}
}

// The first cycle takes ten to twenty seconds of real work against the BMC, and
// the loop starts it immediately, so there is nothing to make faster. What the
// view can do is show the shape it is about to fill in, from the config, rather
// than one line of text.
func TestViewBeforeTheFirstCycleDrawsTheTablesFromTheConfig(t *testing.T) {
	cfg := &config.Config{
		Interval: config.Duration{Duration: 30 * time.Second},
		Fans: []config.Fan{
			{Index: 0, Label: "Fan 1"}, {Index: 1, Label: "Fan 2"},
		},
		Sensors: map[string]config.Sensor{
			"drives": {Kind: "smart"},
			"cpu":    {Kind: "ipmi"},
		},
		Curves: []config.Curve{{Name: "drives", Sensor: "drives"}},
	}
	m := newModel(NewApp(cfg, NewLogSink(20, slog.LevelInfo), &fakeCtl{}, nil))
	m.w, m.h = 100, 40
	out := render(m)

	if !strings.Contains(out, "opening the BMC session") {
		t.Errorf("the view does not say what it is waiting on:\n%s", out)
	}
	for _, want := range []string{"FANS", "Fan 1", "Fan 2", "SENSORS", "cpu", "drives", "CURVES"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skeleton is missing %q:\n%s", want, out)
		}
	}
	// Every value in it is unmeasured, and must read that way. A zero here is
	// a stopped fan and a cold drive, which is the one thing this program must
	// never claim without having measured it.
	if strings.Contains(out, "0%") || strings.Contains(out, "0.0C") {
		t.Errorf("the skeleton rendered an unmeasured value as a number:\n%s", out)
	}
}

func TestViewSurfacesTheThingsThatMatter(t *testing.T) {
	m := newTestModel(&fakeCtl{})
	m.haveSnap = true
	m.snap = state.Snapshot{
		Mode: state.ModeControlling, BMCHost: "192.0.2.10", Connected: true,
		LastCycle: time.Now(), StartedAt: time.Now().Add(-time.Hour),
		Critical: true, BlindGroups: 1,
		Fans: []state.Fan{
			{Index: 0, Label: "Fan 1", Floor: 40, Observed: 12, Mismatch: true},
			{Index: 1, Label: "Fan 2", Floor: 40, Observed: 55},
		},
		Groups: []state.Group{
			{Name: "drives", Kind: "smart", Value: 56, Readings: []state.Reading{
				{Name: "/dev/sda", Celsius: 48}, {Name: "/dev/sdb", Celsius: 56},
			}},
			{Name: "cpu", Kind: "ipmi", Value: state.Unknown(), Blind: true, Err: "ipmitool: exit 1"},
		},
		Curves: []state.Curve{{Name: "drives", Sensor: "drives", Temp: 56, Demand: 40, Driving: true}},
	}
	out := render(m)

	for _, want := range []string{
		"CONTROLLING", "192.0.2.10", "connected",
		"CRITICAL",
		"fan 0 (Fan 1)", // named in the mismatch alert
		"ilo4_unlock",   // and told why it might be happening
		"1 sensor group(s) unreadable",
		"Fan 2", "drives", "cpu", "ipmitool: exit 1",
		"hottest /dev/sdb",
		"driving",
		"q quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view is missing %q:\n%s", want, out)
		}
	}
}

// An unmeasured value must never render as a number. Zero percent is a stopped
// fan and zero degrees is a cold one; neither is what "we could not read it"
// means.
func TestUnknownValuesRenderAsUnknown(t *testing.T) {
	if got := pct(state.Unknown()); strings.Contains(got, "0") {
		t.Errorf("pct(unknown) = %q, want no digits", got)
	}
	if got := celsius(state.Unknown()); strings.Contains(got, "0") {
		t.Errorf("celsius(unknown) = %q, want no digits", got)
	}
	if got := ago(time.Time{}); got != "never" {
		t.Errorf("ago(zero) = %q, want never", got)
	}
}

func TestKeysDriveTheController(t *testing.T) {
	ctl := &fakeCtl{}
	m := newTestModel(ctl)

	m2, cmd := m.key(keyOf(" "))
	if ctl.triggers != 1 {
		t.Errorf("space triggered %d cycles, want 1", ctl.triggers)
	}
	runCmd(t, cmd)

	m3, _ := m2.(model).key(keyOf("d"))
	if !ctl.DryRun() {
		t.Error("d did not turn dry run on")
	}
	m3.(model).key(keyOf("d"))
	if ctl.DryRun() {
		t.Error("d did not turn dry run back off")
	}

	_, cmd = m.key(keyOf("r"))
	msg := runCmd(t, cmd)
	if ctl.reloads != 1 {
		t.Errorf("r caused %d reloads, want 1", ctl.reloads)
	}
	if n, ok := msg.(noticeMsg); !ok || n.bad {
		t.Errorf("a successful reload reported %#v", msg)
	}
}

// The read-only attachment is enforced by there being no controller to call,
// not by the key handler remembering to check. This pins that a keypress in
// that mode cannot reach a BMC another process is already writing to.
func TestReadOnlyModeDrivesNothing(t *testing.T) {
	m := newTestModel(nil)
	for _, k := range []string{"r", "d", " "} {
		_, cmd := m.key(keyOf(k))
		msg := runCmd(t, cmd)
		n, ok := msg.(noticeMsg)
		if !ok || !n.bad || !strings.Contains(n.text, "read only") {
			t.Errorf("key %q in read-only mode produced %#v", k, msg)
		}
	}
	if out := render(m); !strings.Contains(out, "read only") {
		t.Errorf("the footer does not say the view is read only:\n%s", out)
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c", "esc"} {
		if _, cmd := newTestModel(&fakeCtl{}).key(keyOf(k)); cmd == nil {
			t.Errorf("key %q did not quit", k)
		}
	}
}

// busySnapshot is a full six-fan, four-group chassis: the DL380 this was
// written against, which is what makes it worth measuring the layout on.
// /dev/sdf is deliberately not the hottest drive, so its name appears only
// when the readings are expanded.
func busySnapshot(at time.Time, hottest float64) state.Snapshot {
	s := state.Snapshot{
		Mode: state.ModeControlling, BMCHost: "192.0.2.10", Connected: true,
		Interval: 30 * time.Second, LastCycle: at, StartedAt: at.Add(-time.Hour),
		Curves: []state.Curve{
			{Name: "drives", Sensor: "drives", Temp: hottest, Demand: 26, Driving: true},
			{Name: "cpu", Sensor: "cpu", Temp: 61, Demand: 12},
			{Name: "hba", Sensor: "hba", Temp: 58, Demand: 12},
		},
	}
	for i := range 6 {
		s.Fans = append(s.Fans, state.Fan{
			Index: i, Label: fmt.Sprintf("Fan %d", i+1), Floor: 26, Observed: 12, Mismatch: true,
		})
	}
	drives := []state.Reading{}
	for i, name := range []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf"} {
		c := hottest - 6 + float64(i)
		if name == "/dev/sdb" {
			c = hottest
		}
		drives = append(drives, state.Reading{Name: name, Celsius: c})
	}
	s.Groups = []state.Group{
		{Name: "cpu", Kind: "ipmi", Value: 61, Readings: []state.Reading{
			{Name: "02-CPU 1", Celsius: 61}, {Name: "03-CPU 2", Celsius: 55},
		}},
		{Name: "drives", Kind: "smart", Value: hottest, Readings: drives},
		{Name: "hba", Kind: "ipmi", Value: 58, Readings: []state.Reading{{Name: "27-HD Controller", Celsius: 58}}},
		{Name: "inlet", Kind: "ipmi", Value: 21, Readings: []state.Reading{{Name: "01-Inlet Ambient", Celsius: 21}}},
	}
	return s
}

func busyModel(w, h int) model {
	m := newTestModel(&fakeCtl{})
	m.w, m.h = w, h
	m.snap, m.haveSnap = busySnapshot(time.Now(), 54), true
	return m
}

// The DL380 screenshot showed the footer drawn twice with three stale event
// lines between the copies. The cause is a frame that CHANGES height: whatever
// the terminal drew for the taller frame stays on screen under the shorter one.
// So the frame is a constant height, whatever the content does.
func TestTheFrameIsExactlyTheHeightOfTheTerminal(t *testing.T) {
	for _, h := range []int{5, 10, 18, 24, 40, 60} {
		for _, expanded := range []bool{false, true} {
			for _, events := range []int{0, 1, 12} {
				m := busyModel(100, h)
				m.expanded = expanded
				for range events {
					slog.New(m.app.sink).Error("commanded floor is not being honoured",
						"fan", 0, "want", 26.0, "got", 12.5)
				}
				if got := len(strings.Split(render(m), "\n")); got != h {
					t.Errorf("h=%d expanded=%v events=%d: frame is %d lines, want exactly %d",
						h, expanded, events, got, h)
				}
			}
		}
	}
}

// An alert appearing or clearing is the most common way the frame above the
// events pane changes size, and it must not move the footer.
func TestTheFooterStaysOnTheLastRowAsAlertsComeAndGo(t *testing.T) {
	m := busyModel(100, 40)
	slog.New(m.app.sink).Info("fan floor set", "fan", 0)

	withAlert := strings.Split(render(m), "\n")
	for i := range m.snap.Fans {
		m.snap.Fans[i].Mismatch = false
	}
	m.snap.Critical, m.snap.BlindGroups = false, 0
	cleared := strings.Split(render(m), "\n")

	if len(withAlert) != len(cleared) {
		t.Fatalf("the frame changed from %d to %d lines when the alert cleared",
			len(withAlert), len(cleared))
	}
	if !strings.Contains(withAlert[len(withAlert)-1], "q quit") ||
		!strings.Contains(cleared[len(cleared)-1], "q quit") {
		t.Error("the footer is not on the last row")
	}
}

// The footer sat directly under the event log in the same dim style, so on a
// real terminal it read as one more log line. It is a full-width bar now.
//
// This asserts the decisions rather than the escape codes: lipgloss resolves
// its colour profile from the attached terminal, and under `go test` there is
// none, so every style renders as plain text and a check for ANSI would pass
// or fail for reasons that have nothing to do with this layout.
func TestTheFooterIsStyledApartFromTheEventLog(t *testing.T) {
	if sBar.GetBackground() == sDim.GetBackground() {
		t.Error("the footer has no background of its own, so it reads as another log line")
	}
	if sKey.GetForeground() == sBar.GetForeground() {
		t.Error("the key letters are not picked out from the text describing them")
	}
	m := busyModel(100, 40)
	if n := lipgloss.Width(m.footer(m.w)); n != m.w {
		t.Errorf("the footer bar is %d cells wide on a %d-cell terminal, so it is not a bar", n, m.w)
	}
}

// The layout sizes the events pane from the number of strings alerts returns,
// so an alert the terminal wraps and this function does not is a line the
// layout never knew about. Six fans in the mismatch alert is the normal case
// on this chassis, not a corner one.
func TestALongAlertIsWrappedRatherThanLeftToTheTerminal(t *testing.T) {
	m := busyModel(60, 40)
	got := m.alerts(m.w)
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want only the mismatch one: %q", len(got), got)
	}
	if lipgloss.Height(got[0]) < 2 {
		t.Errorf("the six-fan mismatch alert did not wrap at width %d: %q", m.w, got[0])
	}
	for _, line := range strings.Split(ansi.ReplaceAllString(got[0], ""), "\n") {
		if n := len([]rune(line)); n > m.w {
			t.Errorf("alert line is %d cells wide on a %d-cell terminal: %q", n, m.w, line)
		}
	}
}

// The group row reports a max, and a max hides its inputs. Expanding is how you
// find out which drive it was.
func TestExpandListsEveryReading(t *testing.T) {
	m := busyModel(120, 60)
	if out := render(m); strings.Contains(out, "/dev/sdf") {
		t.Fatalf("the collapsed view already lists every drive, so the test proves nothing:\n%s", out)
	}

	next, _ := m.key(keyOf("e"))
	out := render(next.(model))
	for _, want := range []string{"/dev/sda", "/dev/sdc", "/dev/sdf", "02-CPU 1", "03-CPU 2", "collapse"} {
		if !strings.Contains(out, want) {
			t.Errorf("the expanded view is missing %q:\n%s", want, out)
		}
	}

	back, _ := next.(model).key(keyOf("e"))
	if strings.Contains(render(back.(model)), "/dev/sdf") {
		t.Error("e did not collapse again")
	}
}

// Expanding is a display toggle, so it has to work where the control keys
// deliberately do not.
func TestExpandWorksInReadOnlyMode(t *testing.T) {
	next, cmd := newTestModel(nil).key(keyOf("e"))
	if msg := runCmd(t, cmd); msg != nil {
		t.Errorf("e produced %#v, want no notice", msg)
	}
	if !next.(model).expanded {
		t.Error("e did not expand while another process owns the loop")
	}
}

// The scraper re-reads the same cycle several times before the next one lands.
// Recording each read would draw fifteen identical cells and flatten real
// movement into a plateau.
func TestHistoryRecordsOncePerCycle(t *testing.T) {
	app := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), &fakeCtl{}, nil)
	at := time.Now().Add(-time.Minute)
	app.Publish(busySnapshot(at, 54))
	app.Publish(busySnapshot(at, 54))
	app.Publish(busySnapshot(at.Add(30*time.Second), 55))

	got := app.history("drives")
	if len(got) != 2 || got[0] != 54 || got[1] != 55 {
		t.Errorf("history = %v, want one entry per distinct cycle", got)
	}
	// A snapshot that has never completed a cycle carries no temperature worth
	// plotting, and a zero timestamp is not a distinct cycle.
	app.Publish(state.Snapshot{})
	if len(app.history("drives")) != 2 {
		t.Error("a snapshot with no completed cycle was recorded")
	}
}

// owners reports which series drew each cell of a row, so a test can talk
// about the picture without knowing which braille glyph came out.
func owners(p plot, row int) []int {
	out := make([]int, len(p.grid[row]))
	for i, c := range p.grid[row] {
		out[i] = c.owner
	}
	return out
}

// drewAnything reports whether a series appears anywhere in the chart.
func drewAnything(p plot, series int) bool {
	for r := range p.grid {
		for _, c := range p.grid[r] {
			if c.owner == series {
				return true
			}
		}
	}
	return false
}

func TestEveryGroupIsDrawnOnOneSharedAxis(t *testing.T) {
	// Nothing read yet: an empty chart with no axis, rather than an axis
	// invented out of no readings.
	empty := newPlot([][]float64{{state.Unknown()}, {state.Unknown()}}, 20, 2)
	if empty.ok {
		t.Error("claimed a temperature axis from nothing but unreadable samples")
	}
	if empty.row[0] == empty.row[1] || empty.row[0] < 0 || empty.row[1] < 0 {
		t.Errorf("legend rows = %v, want every group listed on a row of its own", empty.row)
	}

	// The point of one axis: height means a temperature, so a cold group is
	// below a hot one and stays there. Given each series its own range, as the
	// chart used to, both of these were a flat line in the middle of their own
	// row and the picture said nothing about which was hotter.
	hot, cold := flatSeries(21, 24), flatSeries(61, 24)
	p := newPlot([][]float64{hot, cold}, 24, 4)
	if !p.ok || p.lo > 21 || p.hi < 61 {
		t.Fatalf("axis = %v..%v, want it to cover every group's readings", p.lo, p.hi)
	}
	if p.row[0] <= p.row[1] {
		t.Errorf("the 21C group is drawn on row %d and the 61C group on row %d, want the cold one lower",
			p.row[0], p.row[1])
	}

	// A quiet machine is drawn as a quiet machine. Fitted exactly, four
	// sensors within a tenth of a degree of each other filled the whole chart
	// with noise magnified to full height.
	quiet := newPlot([][]float64{flatSeries(40, 8), flatSeries(40.1, 8)}, 8, 4)
	if quiet.hi-quiet.lo < trendMinSpanC {
		t.Errorf("axis = %v..%v for two sensors 0.1C apart, want at least %vC of span",
			quiet.lo, quiet.hi, trendMinSpanC)
	}

	// One sample is a chart, and it is a full-width one. The oldest reading is
	// held flat out to the left edge, so the pane has its extent from the
	// first cycle and pans left as it fills rather than growing out of a stub.
	one := newPlot([][]float64{alignRight([]float64{40}, 20)}, 20, 2)
	if !one.ok {
		t.Fatal("one reading did not draw a chart")
	}
	for c, o := range owners(one, one.row[0]) {
		if o != 0 {
			t.Fatalf("cell %d of a one-sample chart is empty, want the reading held flat to the left edge", c)
		}
	}

	// A gap is drawn as a gap. Interpolating across an unreadable sensor would
	// invent the very reading the collector refused to give.
	gapped := newPlot([][]float64{{40, state.Unknown(), 44}}, 3, 2)
	drawn := 0
	for r := range gapped.grid {
		for _, c := range gapped.grid[r] {
			if c.owner == 0 {
				drawn++
			}
		}
	}
	if drawn != 2 {
		t.Errorf("a three-sample series with one gap drew %d cells, want 2", drawn)
	}
}

// A terminal cell has one foreground colour, and colour is the only thing
// telling one line from another once they share an axis. Two groups reading
// almost the same temperature must therefore not land in the same cell.
func TestConvergingSeriesArePushedApartRatherThanMerged(t *testing.T) {
	p := newPlot([][]float64{flatSeries(40, 12), flatSeries(40, 12), flatSeries(40, 12)}, 12, 3)
	if !p.ok {
		t.Fatal("three identical series drew nothing")
	}
	for i := range 3 {
		if !drewAnything(p, i) {
			t.Errorf("series %d was not drawn at all, want every group on the chart", i)
		}
	}
	for c := range 12 {
		seen := map[int]bool{}
		for r := range p.grid {
			if o := p.grid[r][c].owner; o >= 0 {
				if seen[o] {
					t.Fatalf("series %d is drawn twice in column %d", o, c)
				}
				seen[o] = true
			}
		}
		if len(seen) != 3 {
			t.Fatalf("column %d holds %d of 3 series, so two of them merged into one cell", c, len(seen))
		}
	}
	// A pushed line keeps its shape. Measured from the row it was pushed into
	// rather than from the value, it flattens against the row's edge, so three
	// sensors a few degrees apart all became straight lines: the complaint the
	// chart was rewritten to answer, reintroduced by the fix for the other one.
	// Two steady sensors at 45C hold the row this one wants, so it is pushed
	// two rows below where its value puts it, and it has to arrive there still
	// wobbling between 44C and 45C.
	wobble := make([]float64, 16)
	for i := range wobble {
		wobble[i] = 44 + float64(i%2)
	}
	pushed := newPlot([][]float64{flatSeries(45, 16), flatSeries(45, 16), wobble}, 16, 3)
	shapes := map[rune]bool{}
	for r := range pushed.grid {
		for _, c := range pushed.grid[r] {
			if c.owner == 2 {
				shapes[c.glyph] = true
			}
		}
	}
	if len(shapes) < 2 {
		t.Errorf("a nudged series drew one height throughout, want the shape it would have had in its own row")
	}

	// And the legend follows the lines, one group per row, so nothing has to
	// be matched up by colour alone.
	rows := map[int]bool{}
	for i, r := range p.row {
		if r < 0 {
			t.Fatalf("series %d has no legend row", i)
		}
		if rows[r] {
			t.Fatalf("two legends share row %d", r)
		}
		rows[r] = true
	}
}

func flatSeries(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// The snapshot carries the mode of the cycle that produced it, so for up to a
// full interval after pressing d the screen showed the DRY RUN badge and the
// dry run alert directly above the acknowledgement saying it had been turned
// off. Seen on the DL380 with a 30s interval.
func TestTogglingTheDryRunShowsBeforeTheNextCycle(t *testing.T) {
	ctl := &fakeCtl{dry: true}
	app := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), ctl, nil)
	stale := busySnapshot(time.Now().Add(-time.Minute), 44)
	stale.Mode = state.ModeDryRun
	app.Publish(stale)

	m := newModel(app)
	m.snap, m.haveSnap, m.scrapeErr = app.read()
	m.w, m.h = 120, 40

	if !strings.Contains(m.header(m.w), string(state.ModeDryRun)) {
		t.Fatal("the badge did not say dry run while the dry run was on")
	}

	next, _ := m.Update(keyOf("d"))
	m = next.(model)
	if ctl.DryRun() {
		t.Fatal("d did not turn the dry run off")
	}
	// The snapshot is deliberately not republished: this is the window the
	// bug lived in.
	if got := m.header(m.w); strings.Contains(got, string(state.ModeDryRun)) {
		t.Errorf("badge still reads dry run after turning it off: %q", got)
	}
	for _, a := range m.alerts(m.w) {
		if strings.Contains(a, "dry run:") {
			t.Errorf("dry run alert survived turning the dry run off: %q", a)
		}
	}
}

func TestTrendsTakeTheSpareWidthAndYieldItWhenThereIsNone(t *testing.T) {
	app := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), &fakeCtl{}, nil)
	base := time.Now().Add(-10 * time.Minute)
	for i := range 12 {
		app.Publish(busySnapshot(base.Add(time.Duration(i)*30*time.Second), 42+float64(i)))
	}
	m := newModel(app)
	m.snap, m.haveSnap, m.scrapeErr = app.read()
	m.w, m.h = 170, 60

	out := render(m)
	if !strings.Contains(out, "TRENDS") {
		t.Fatalf("no trends pane on a 170-cell terminal:\n%s", out)
	}
	side := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "FANS") && strings.Contains(line, "TRENDS") {
			side = true
		}
	}
	if !side {
		t.Errorf("the trends pane is stacked below the tables rather than beside them:\n%s", out)
	}
	// Beside them, not marooned somewhere off to the right. Anchoring the pane
	// on a fixed fraction of the window left a fifty column hole between the
	// tables and the charts on a real terminal.
	tablesW := lipgloss.Width(m.tables(m.w))
	if col := trendsColumn(out); col > tablesW+4 {
		t.Errorf("the trends pane starts at column %d but the tables end at %d:\n%s",
			col, tablesW, out)
	}
	// The axis is printed, top and bottom, because one shared scale is only
	// readable if the reader can see what it spans. These readings run from
	// the 21C inlet to the 61C CPU.
	if !strings.Contains(out, "  61┤") || !strings.Contains(out, "  21┤") {
		t.Errorf("the trends pane does not print the axis it scaled against:\n%s", out)
	}
	// And every group is named against its own line.
	for _, name := range []string{"cpu", "drives", "hba", "inlet"} {
		if !strings.Contains(m.trends(60), name) {
			t.Errorf("group %q has no legend entry:\n%s", name, m.trends(60))
		}
	}

	// A wide terminal is not a wide window. Handed every spare column of a
	// 250 column terminal the chart drew over an hour of history, nearly all
	// of it empty baseline, which reads as a line with a smudge on the end.
	if m.trends(200) != m.trends(400) {
		t.Errorf("the chart grows with the terminal instead of capping its window:\n%s",
			m.trends(400))
	}

	// The caption belongs against the chart it describes, with no blank row
	// pushing it away from the last line of the plot.
	pane := strings.Split(m.trends(60), "\n")
	for i, line := range pane {
		if !strings.Contains(line, "per cell") {
			continue
		}
		if i == 0 || strings.TrimSpace(pane[i-1]) == "" {
			t.Errorf("a blank line sits between the last series and the caption:\n%s",
				m.trends(60))
		}
	}

	// Expanding the readings widens the tables a lot. The pane must not slide
	// across the screen and rescale itself when it does: a chart whose
	// horizontal scale moves on its own is worse than no chart.
	m.expanded = true
	if a, b := trendsColumn(out), trendsColumn(render(m)); a != b {
		t.Errorf("the trends pane moved from column %d to %d when the readings were expanded", a, b)
	}

	m.expanded = false
	m.w = 80
	if out := render(m); strings.Contains(out, "TRENDS") {
		t.Errorf("the trends pane squeezed the tables on an 80-cell terminal:\n%s", out)
	}
}

func trendsColumn(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "TRENDS"); i >= 0 {
			return i
		}
	}
	return -1
}

// A handshake in flight is not a fault. iLO 4 negotiates SHA-1 era key
// exchange and takes several seconds over it, so reporting "no session"
// while it runs put a red word in the header for the first part of every
// startup, describing a problem that was not there.
func TestTheLinkSaysConnectingWhileTheHandshakeIsOpen(t *testing.T) {
	app := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), &fakeCtl{}, nil)
	m := newModel(app)
	m.w, m.h = 120, 40

	// Nothing published yet, and this process owns the loop, so it is opening
	// a session by definition.
	if got := m.header(m.w); !strings.Contains(got, "connecting") {
		t.Errorf("header before the first snapshot = %q, want connecting", got)
	}

	for _, tc := range []struct {
		name                  string
		connected, connecting bool
		want                  string
	}{
		{"handshake in flight", false, true, "connecting"},
		{"session up", true, false, "connected"},
		{"handshake failed", false, false, "no session"},
	} {
		s := busySnapshot(time.Now(), 44)
		s.Connected, s.Connecting = tc.connected, tc.connecting
		app.Publish(s)
		m.snap, m.haveSnap, m.scrapeErr = app.read()
		got := m.header(m.w)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: header = %q, want %q", tc.name, got, tc.want)
		}
		// "connected" is a substring of nothing else here, but "connecting"
		// contains neither of the others, so check the pair cannot both show.
		if tc.want == "no session" && strings.Contains(got, "connect") {
			t.Errorf("%s: header still claims a connection: %q", tc.name, got)
		}
	}

	// A read-only view attached to another daemon's metrics cannot know that a
	// handshake is in flight, so it must not claim one is.
	ro := NewApp(&config.Config{}, NewLogSink(20, slog.LevelInfo), nil, nil)
	rm := newModel(ro)
	rm.w, rm.h = 120, 40
	if got := rm.header(rm.w); strings.Contains(got, "connecting") {
		t.Errorf("read-only header = %q, want no claim about a handshake", got)
	}
}
