package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/curve"
	"github.com/alekc/ilo-fanctl/internal/metrics"
	"github.com/alekc/ilo-fanctl/internal/sensors"
	"github.com/alekc/ilo-fanctl/internal/state"
)

// fakeBMC records what was commanded and reports back whatever the test tells
// it to. The BMC's real behaviour, that a write prints nothing whether or not
// it worked, is exactly what makes the read-back load-bearing, so the fake
// keeps commanded and reported speeds as separate state.
type fakeBMC struct {
	mu sync.Mutex

	commanded map[int]int // fan index -> raw value, in call order per fan
	calls     []string
	reported  map[int]float64

	// ignoreWrites models firmware with the ilo4_unlock patch reverted: the
	// commands succeed and nothing moves.
	ignoreWrites bool

	// spinUpLag models a real fan. A write lands, but the speed the BMC
	// reports only catches up by the following read, so a floor commanded in
	// this cycle is not visible in this cycle's read-back.
	//
	// Without this the fake answers its own commands instantly, which is an
	// idealisation no physical fan meets, and it hid a false-positive
	// mismatch on every startup until the first live run on real hardware.
	spinUpLag bool
	pending   map[int]float64

	connectErr error
	speedsErr  error
	closed     bool

	// onConnect, when set, runs inside Connect. It is how a test holds the
	// handshake open to see what the rest of the cycle does meanwhile.
	onConnect func()
}

func newFakeBMC() *fakeBMC {
	return &fakeBMC{
		commanded: map[int]int{},
		reported:  map[int]float64{0: 9, 1: 11, 2: 11, 3: 17, 4: 18, 5: 17},
		pending:   map[int]float64{},
	}
}

func (f *fakeBMC) Connect(context.Context) error {
	if f.onConnect != nil {
		f.onConnect()
	}
	return f.connectErr
}
func (f *fakeBMC) Close()              { f.mu.Lock(); f.closed = true; f.mu.Unlock() }
func (f *fakeBMC) HostKeyPinned() bool { return true }

func (f *fakeBMC) SetFanMin(_ context.Context, index, raw int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commanded[index] = raw
	f.calls = append(f.calls, fmt.Sprintf("fan %d min %d", index, raw))
	if !f.ignoreWrites {
		if f.spinUpLag {
			f.pending[index] = float64(int(curve.RawToPct(raw)))
		} else {
			f.reported[index] = float64(int(curve.RawToPct(raw)))
		}
	}
	return nil
}

func (f *fakeBMC) FanSpeeds(context.Context) (map[int]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.speedsErr != nil {
		return nil, f.speedsErr
	}
	out := make(map[int]float64, len(f.reported))
	for k, v := range f.reported {
		out[k] = v
	}
	// Promote after answering, so this read shows the speeds as they were
	// before the writes that preceded it and the next one shows them settled.
	for k, v := range f.pending {
		f.reported[k] = v
		delete(f.pending, k)
	}
	return out, nil
}

func (f *fakeBMC) commandedFor(idx int) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.commanded[idx]
	return v, ok
}

func (f *fakeBMC) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeCollector returns fixed readings, or an error.
type fakeCollector struct {
	mu   sync.Mutex
	temp map[string]float64 // group-ish key by first match/device name
	err  error
}

func (f *fakeCollector) Collect(_ context.Context, s config.Sensor) ([]sensors.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	names := s.Match
	if len(names) == 0 {
		names = s.Devices
	}
	var out []sensors.Reading
	for _, n := range names {
		if v, ok := f.temp[n]; ok {
			// The kind is the source a real collector reports, and the
			// /metrics round-trip test needs that label to be truthful: it is
			// how a scraped reading is attributed back to its group.
			out = append(out, sensors.Reading{Source: s.Kind, Name: n, Celsius: v})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no readings for %v", names)
	}
	return out, nil
}

func (f *fakeCollector) set(name string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.temp[name] = v
}

func (f *fakeCollector) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

const testYAML = `
ilo:
  host: 192.0.2.10
  user: ilofanuser
  private_key_path: KEYPATH
  connect_timeout: 15s
interval: 1s
tools:
  ipmitool_path: /nonexistent/ipmitool-A
  smartctl_path: /nonexistent/smartctl-A
fans:
  - { index: 0, label: "Fan 1", zones: [chassis] }
  - { index: 1, label: "Fan 2", zones: [chassis] }
sensors:
  drives:
    kind: smart
    devices: [/dev/sda]
  cpu:
    kind: ipmi
    match: ["02-CPU 1"]
curves:
  - name: drives
    sensor: drives
    zones: [chassis]
    points:
      - { temp: 30, pwm_pct: 10 }
      - { temp: 60, pwm_pct: 70 }
  - name: cpu
    sensor: cpu
    zones: [chassis]
    points:
      - { temp: 40, pwm_pct: 10 }
      - { temp: 90, pwm_pct: 60 }
safety:
  max_floor_pct: 70
  min_floor_pct: 10
  hysteresis_pct: 3
  ramp_down_per_cycle_pct: 2
  on_sensor_error:
    action: hold_last
    max_hold: 1m
    fixed_pct: 45
  critical:
    - { sensor: drives, temp: 58 }
    - { sensor: cpu, temp: 95 }
`

type harness struct {
	ctrl *Controller
	bmc  *fakeBMC
	col  *fakeCollector
	m    *metrics.Metrics
	path string
}

func newHarness(t *testing.T, body string) *harness {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(key, []byte("placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "KEYPATH", key)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("test config is not valid: %v", err)
	}

	col := &fakeCollector{temp: map[string]float64{"/dev/sda": 30, "02-CPU 1": 40}}
	bmc := newFakeBMC()
	m := metrics.New("test")
	c := New(path, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	c.newBMC = func(config.ILO) bmcClient { return bmc }
	c.bmc = bmc
	c.sens = sensors.NewRegistryWith(map[string]sensors.Collector{"smart": col, "ipmi": col})
	return &harness{ctrl: c, bmc: bmc, col: col, m: m, path: path}
}

func (h *harness) cycle() { h.ctrl.cycle(context.Background()) }

// The central question the design has to answer: a hot CPU must raise the fans
// even when the drives are cold, because both curves cover the same fans and
// the highest demand wins.
func TestHotCPUWithColdDrivesStillRaisesFans(t *testing.T) {
	h := newHarness(t, testYAML)

	h.col.set("/dev/sda", 30) // coldest point of the drives curve, demands 10
	h.col.set("02-CPU 1", 90) // hottest point of the cpu curve, demands 60
	h.cycle()

	want := curve.PctToRaw(60)
	for _, idx := range []int{0, 1} {
		got, ok := h.bmc.commandedFor(idx)
		if !ok {
			t.Fatalf("fan %d was never commanded", idx)
		}
		if got != want {
			t.Errorf("fan %d commanded raw %d (%.1f pct), want %d (60 pct)",
				idx, got, curve.RawToPct(got), want)
		}
	}
}

// And the reverse: a hot drive must not be held down by a cold CPU.
func TestHotDrivesWithColdCPU(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 45) // halfway up the drives curve: 10 + 0.5*60 = 40
	h.col.set("02-CPU 1", 40)
	h.cycle()

	if got, _ := h.bmc.commandedFor(0); got != curve.PctToRaw(40) {
		t.Errorf("fan 0 commanded %d, want %d", got, curve.PctToRaw(40))
	}
}

func TestCriticalOverridesEverything(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 59) // above the drives critical threshold of 58
	h.col.set("02-CPU 1", 40)
	h.cycle()

	want := curve.PctToRaw(70) // max_floor_pct
	for _, idx := range []int{0, 1} {
		if got, _ := h.bmc.commandedFor(idx); got != want {
			t.Errorf("fan %d commanded %d during a critical, want the max floor %d", idx, got, want)
		}
	}
}

// A critical must bypass the ramp-down limit in both directions of travel:
// getting there is immediate, not two percent per cycle.
func TestCriticalIgnoresRampDown(t *testing.T) {
	h := newHarness(t, testYAML)
	// Settle at 40 percent first, well below the max floor, so that reaching
	// 70 in one cycle can only happen by bypassing the 2 point ramp limit.
	h.col.set("/dev/sda", 45)
	h.cycle()
	if got := mustCommanded(t, h, 0); got != curve.PctToRaw(40) {
		t.Fatalf("setup: fan 0 = %d, want %d", got, curve.PctToRaw(40))
	}

	h.col.set("/dev/sda", 30) // curves now want 10 percent
	h.col.set("02-CPU 1", 96) // but the cpu critical trips
	h.cycle()

	if got, _ := h.bmc.commandedFor(0); got != curve.PctToRaw(70) {
		t.Errorf("fan 0 commanded %d, want the max floor %d in a single cycle",
			got, curve.PctToRaw(70))
	}
}

func TestRampDownIsGradualAndHysteresisSuppressesChatter(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 55) // 10 + (25/30)*60 = 60 percent
	h.cycle()
	start := curve.RawToPct(mustCommanded(t, h, 0))

	// Drop the demand a long way. Each subsequent cycle may fall by at most
	// ramp_down_per_cycle_pct.
	h.col.set("/dev/sda", 30)
	for i := 0; i < 3; i++ {
		before := curve.RawToPct(mustCommanded(t, h, 0))
		h.cycle()
		after := curve.RawToPct(mustCommanded(t, h, 0))
		if drop := before - after; drop > 2.5 {
			t.Fatalf("cycle %d dropped %.2f points, want at most the 2 point ramp limit", i, drop)
		}
	}
	if now := curve.RawToPct(mustCommanded(t, h, 0)); now >= start {
		t.Errorf("floor did not fall at all: started %.1f, now %.1f", start, now)
	}

	// Left alone at a constant temperature the floor must stop moving. It
	// settles a little above the demand rather than exactly on it, because the
	// last step down is smaller than the hysteresis band, which is the band
	// doing its job. Bounded, so a controller that never settles fails here
	// instead of hanging the suite.
	settled := mustCommanded(t, h, 0)
	converged := false
	for i := 0; i < 100; i++ {
		h.cycle()
		now := mustCommanded(t, h, 0)
		if now == settled {
			converged = true
			break
		}
		settled = now
	}
	if !converged {
		t.Fatalf("floor never settled at a constant temperature, still at %.1f pct",
			curve.RawToPct(settled))
	}

	before := h.bmc.callCount()
	h.cycle()
	h.cycle()
	if got := h.bmc.callCount(); got != before {
		t.Errorf("%d further writes at a settled temperature, want none", got-before)
	}
}

// Firmware with the patch reverted accepts every write and moves nothing. The
// read-back is the only thing that can notice.
func TestRevertedPatchIsDetected(t *testing.T) {
	h := newHarness(t, testYAML)
	h.bmc.ignoreWrites = true
	h.col.set("/dev/sda", 55)
	// Two cycles, because a floor is only judged once it has had an interval
	// to take effect. One cycle proves nothing either way by design.
	h.cycle()
	h.cycle()

	if h.bmc.callCount() == 0 {
		t.Fatal("no write was attempted, so the test proves nothing")
	}
	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got != 0 {
		t.Errorf("control_effective = %v, want 0 when the BMC ignores the floors", got)
	}
	if n := mismatchTotal(t, h); n == 0 {
		t.Error("control_effective was 0 but no fan was counted as mismatched")
	}
}

func TestEffectiveWhenHonoured(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 55)
	// Two cycles: the first floor has to have had an interval to take effect
	// before the read-back is entitled to an opinion about it.
	h.cycle()
	h.cycle()
	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got != 1 {
		t.Errorf("control_effective = %v, want 1", got)
	}
}

// A fan cannot be at a floor it was given moments ago, so the cycle that
// raises a floor must not report the fan as ignoring it.
//
// This is the defect the first live run on the reference machine exposed:
// every startup logged all six fans as unhonoured and drove
// ilo_fanctl_control_effective to 0, which is the single signal meaning the
// ilo4_unlock patch has been reverted and the drives are unprotected. The
// whole suite missed it because the fake BMC answered its own writes with no
// delay, so the one thing never modelled was a fan taking time to spin up.
func TestFloorRaisedThisCycleIsNotYetAMismatch(t *testing.T) {
	h := newHarness(t, testYAML)
	h.bmc.spinUpLag = true

	h.col.set("/dev/sda", 40)
	h.cycle()
	h.col.set("/dev/sda", 55) // demands a materially higher floor
	h.cycle()

	if n := mismatchTotal(t, h); n != 0 {
		t.Errorf("readback_mismatch_total = %v, want 0 while the fans are still spinning up", n)
	}
	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got == 0 {
		t.Error("control_effective = 0 while the fans were merely still spinning up")
	}

	// Once the speeds catch up the verdict must still be clean, otherwise the
	// test above would pass simply by never judging anything.
	h.cycle()
	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got != 1 {
		t.Errorf("control_effective = %v, want 1 once the fans have caught up", got)
	}
}

// The other half of the pair: a genuinely reverted patch must still be caught
// through the lag, one cycle later than before rather than never.
func TestRevertedPatchIsStillCaughtThroughTheLag(t *testing.T) {
	h := newHarness(t, testYAML)
	h.bmc.spinUpLag = true
	h.col.set("/dev/sda", 55)
	h.cycle()

	// From here the BMC accepts every command and moves nothing.
	h.bmc.mu.Lock()
	h.bmc.ignoreWrites = true
	h.bmc.reported[0] = 9
	h.bmc.reported[1] = 11
	h.bmc.mu.Unlock()

	h.cycle()
	h.cycle()

	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got != 0 {
		t.Errorf("control_effective = %v, want 0 when the BMC ignores the floors", got)
	}
	if n := mismatchTotal(t, h); n == 0 {
		t.Error("no mismatch was counted, so the reverted patch would go unnoticed")
	}
}

// Before any floor has had an interval to settle there is no evidence either
// way, and the gauge must be absent rather than optimistic.
func TestEffectivenessIsUnknownBeforeTheFirstSettledFloor(t *testing.T) {
	h := newHarness(t, testYAML)
	h.bmc.spinUpLag = true
	h.col.set("/dev/sda", 55)
	h.cycle()

	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); !math.IsNaN(got) {
		t.Errorf("control_effective = %v after one cycle, want NaN: nothing could be verified yet", got)
	}
}

// The BMC runs its own curve above our floor, so a fan spinning faster than
// commanded is normal and must not be reported as a mismatch.
func TestFanFasterThanFloorIsNotAMismatch(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 40)
	h.cycle()
	h.bmc.mu.Lock()
	h.bmc.reported[0] = 95
	h.bmc.reported[1] = 95
	h.bmc.mu.Unlock()
	h.cycle()

	if got := gaugeValue(t, h, "ilo_fanctl_control_effective"); got != 1 {
		t.Errorf("control_effective = %v, want 1 when the BMC runs above our floor", got)
	}
}

// Blind on every sensor, the controller must fall back to a defined floor
// rather than stop regulating. It holds the last good value first.
func TestSensorFailureHoldsThenFallsBack(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 45) // 40 percent
	h.cycle()
	held := mustCommanded(t, h, 0)

	h.col.fail(fmt.Errorf("smartctl exploded"))
	h.cycle()
	if got, _ := h.bmc.commandedFor(0); got != held {
		t.Errorf("floor moved to %d while holding the last good value, want %d", got, held)
	}

	// Age the blind clock past max_hold and the fallback takes over.
	h.ctrl.mu.Lock()
	for k := range h.ctrl.blindSince {
		h.ctrl.blindSince[k] = time.Now().Add(-2 * time.Minute)
	}
	h.ctrl.lastGood = map[string]float64{}
	h.ctrl.mu.Unlock()
	h.cycle()

	if got, _ := h.bmc.commandedFor(0); got != curve.PctToRaw(45) {
		t.Errorf("floor = %d after the hold expired, want the fixed_pct fallback %d",
			got, curve.PctToRaw(45))
	}
}

func TestDryRunNeverWrites(t *testing.T) {
	h := newHarness(t, testYAML)
	h.ctrl.SetDryRun(true)
	h.col.set("/dev/sda", 58)
	h.cycle()
	if n := h.bmc.callCount(); n != 0 {
		t.Errorf("%d writes in dry run, want none", n)
	}
}

func TestReloadAppliesNewCurve(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 45)
	h.cycle()
	if got, _ := h.bmc.commandedFor(0); got != curve.PctToRaw(40) {
		t.Fatalf("setup: fan 0 = %d, want %d", got, curve.PctToRaw(40))
	}

	// Raise the whole drives curve. The same temperature must now demand more.
	updated := strings.Replace(testYAML,
		"      - { temp: 30, pwm_pct: 10 }\n      - { temp: 60, pwm_pct: 70 }",
		"      - { temp: 30, pwm_pct: 50 }\n      - { temp: 60, pwm_pct: 70 }", 1)
	writeConfig(t, h, updated)

	if err := h.ctrl.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	h.cycle()

	// 30 C -> 50, 60 C -> 70, so 45 C -> 60.
	if got, _ := h.bmc.commandedFor(0); got != curve.PctToRaw(60) {
		t.Errorf("fan 0 = %d after reload, want %d", got, curve.PctToRaw(60))
	}
}

// A config that does not validate must not stop the controller.
func TestReloadRejectsInvalidAndKeepsRunning(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 45)
	h.cycle()
	before := mustCommanded(t, h, 0)

	writeConfig(t, h, strings.Replace(testYAML, "min_floor_pct: 10", "min_floor_pct: 999", 1))
	if err := h.ctrl.Reload(); err == nil {
		t.Fatal("an invalid config was accepted")
	}
	h.cycle()
	if got, _ := h.bmc.commandedFor(0); got != before {
		t.Errorf("floor changed to %d after a rejected reload, want the previous %d", got, before)
	}
	if got := counterValue(t, h, "ilo_fanctl_config_reloads_total", "failure"); got != 1 {
		t.Errorf("failure counter = %v, want 1", got)
	}
}

// A fan removed from the config must be wound down rather than left where it
// was, because nothing else will ever lower it.
func TestReloadWindsDownRemovedFans(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 55)
	h.cycle()
	if _, ok := h.bmc.commandedFor(1); !ok {
		t.Fatal("setup: fan 1 was never commanded")
	}

	writeConfig(t, h, strings.Replace(testYAML,
		"  - { index: 1, label: \"Fan 2\", zones: [chassis] }\n", "", 1))
	if err := h.ctrl.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got, _ := h.bmc.commandedFor(1); got != curve.PctToRaw(10) {
		t.Errorf("removed fan 1 left at %d, want it wound to min_floor_pct %d",
			got, curve.PctToRaw(10))
	}
}

// Reloads arrive on the SIGHUP goroutine while the loop is mid-cycle. Run
// under -race, this is the test that catches a config, registry or client
// swapped out from under a cycle.
func TestConcurrentReloadDuringCycles(t *testing.T) {
	h := newHarness(t, testYAML)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.cycle()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			// Vary all three things a reload can swap: the config itself, the
			// BMC client (any change to the ilo section rebuilds it) and the
			// sensor registry (any change to tools rebuilds it). Varying only
			// the first is not enough. This test passed against the racy
			// version of cycle() until the ilo and tools sections were
			// included here, because nothing was ever reassigning those two
			// fields for the unlocked reads to race with.
			body := strings.Replace(testYAML,
				"min_floor_pct: 10", fmt.Sprintf("min_floor_pct: %d", 10+i%5), 1)
			if i%2 == 0 {
				body = strings.ReplaceAll(body, "connect_timeout: 15s", "connect_timeout: 16s")
				body = strings.ReplaceAll(body, "ipmitool-A", "ipmitool-B")
				body = strings.ReplaceAll(body, "smartctl-A", "smartctl-B")
			}
			writeConfig(t, h, body)
			_ = h.ctrl.Reload()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.col.set("/dev/sda", 30+float64(i%25))
		}
	}()

	// Let the reloader and the mutator finish, then stop the cycler.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	time.AfterFunc(2*time.Second, func() { close(stop) })
	<-done

	if h.ctrl.snapshot() == nil {
		t.Fatal("config went nil under concurrent reload")
	}
}

func TestReleaseWindsFansDown(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 58)
	h.cycle()

	h.ctrl.release()
	for _, idx := range []int{0, 1} {
		if got, _ := h.bmc.commandedFor(idx); got != curve.PctToRaw(10) {
			t.Errorf("fan %d left at %d on shutdown, want min_floor_pct %d",
				idx, got, curve.PctToRaw(10))
		}
	}
	h.bmc.mu.Lock()
	closed := h.bmc.closed
	h.bmc.mu.Unlock()
	if !closed {
		t.Error("the SSH session was not closed on shutdown")
	}
}

// Every commanded value must sit inside the configured bounds, whatever the
// sensors say. This is the guard against one absurd reading turning into an
// absurd floor.
func TestFloorAlwaysWithinBounds(t *testing.T) {
	h := newHarness(t, testYAML)
	for _, temp := range []float64{-40, 0, 30, 55, 100, 500, 10000} {
		h.col.set("/dev/sda", temp)
		h.col.set("02-CPU 1", temp)
		h.cycle()
		for _, idx := range []int{0, 1} {
			raw, ok := h.bmc.commandedFor(idx)
			if !ok {
				continue
			}
			pct := curve.RawToPct(raw)
			if pct < 10-0.5 || pct > 70+0.5 {
				t.Errorf("at %v C fan %d was commanded %.2f pct, outside 10..70", temp, idx, pct)
			}
		}
	}
}

func mustCommanded(t *testing.T, h *harness, idx int) int {
	t.Helper()
	v, ok := h.bmc.commandedFor(idx)
	if !ok {
		t.Fatalf("fan %d was never commanded", idx)
	}
	return v
}

// writeConfig reports rather than fails, because the concurrency test calls it
// from a goroutine, where t.Fatal is not allowed.
func writeConfig(t *testing.T, h *harness, body string) {
	t.Helper()
	key := filepath.Join(filepath.Dir(h.path), "id_rsa")
	if err := os.WriteFile(h.path, []byte(strings.ReplaceAll(body, "KEYPATH", key)), 0o600); err != nil {
		t.Error(err)
	}
}

func gaugeValue(t *testing.T, h *harness, name string) float64 {
	t.Helper()
	return scrape(t, h)[name]
}

func counterValue(t *testing.T, h *harness, name, label string) float64 {
	t.Helper()
	return scrape(t, h)[name+"|"+label]
}

// mismatchTotal sums the per-fan mismatch counters.
func mismatchTotal(t *testing.T, h *harness) float64 {
	t.Helper()
	var total float64
	for key, v := range scrape(t, h) {
		if strings.HasPrefix(key, "ilo_fanctl_readback_mismatch_total|") {
			total += v
		}
	}
	return total
}

// scrape flattens the registry into name|label1|label2 keys.
func scrape(t *testing.T, h *harness) map[string]float64 {
	t.Helper()
	families, err := h.m.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range families {
		for _, mt := range f.GetMetric() {
			key := f.GetName()
			for _, l := range mt.GetLabel() {
				key += "|" + l.GetValue()
			}
			switch {
			case mt.GetGauge() != nil:
				out[key] = mt.GetGauge().GetValue()
			case mt.GetCounter() != nil:
				out[key] = mt.GetCounter().GetValue()
			}
		}
	}
	return out
}

// A cycle is slow because the work in it is slow: several sensor probes, an
// SSH handshake to a BMC that negotiates like it is 2011, and one write per
// fan spaced out because iLO's shell drops commands sent back to back.
// Publishing only at the end meant the whole view sat empty for all of it, and
// eighteen seconds of an empty screen reads as a hung program rather than a
// working one.
//
// Both halves of the fix are asserted together, because they hold each other
// up: the handshake is held open until a snapshot carrying sensor groups has
// reached the observer. A cycle that connects before it collects, or that
// publishes only once at the end, never releases it and is reported by the
// deadline.
func TestSensorsReachTheViewWhileTheBMCHandshakeIsStillOpen(t *testing.T) {
	h := newHarness(t, testYAML)

	release := make(chan struct{})
	var once sync.Once
	h.bmc.onConnect = func() { <-release }

	var mu sync.Mutex
	var snaps []state.Snapshot
	h.ctrl.Observer = func(s state.Snapshot) {
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
		if len(s.Groups) > 0 && !s.Connected {
			once.Do(func() { close(release) })
		}
	}

	done := make(chan struct{})
	go func() { h.cycle(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no sensor group reached the observer while the BMC handshake was open")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(snaps) < 3 {
		t.Fatalf("the cycle published %d snapshots, want the view filling in as it ran", len(snaps))
	}

	// Exactly one snapshot is the finished cycle, and it is the last. A
	// consumer plotting one point per cycle keys on LastCycle, so a partial
	// carrying one would be recorded as a cycle of its own and the trend would
	// show the same reading several times over.
	complete := 0
	for _, s := range snaps {
		if !s.LastCycle.IsZero() {
			complete++
		}
	}
	if complete != 1 {
		t.Errorf("%d of %d snapshots claim a completed cycle, want only the last", complete, len(snaps))
	}
	if snaps[len(snaps)-1].LastCycle.IsZero() {
		t.Error("the cycle did not finish with a completed snapshot")
	}

	// Groups are collected concurrently and arrive in whatever order the
	// hardware answers, but the table they land in is read by a human, so the
	// rows must not reshuffle as it fills.
	for i, s := range snaps {
		if !sort.SliceIsSorted(s.Groups, func(a, b int) bool { return s.Groups[a].Name < s.Groups[b].Name }) {
			t.Errorf("snapshot %d lists its groups out of order: %v", i, groupNames(s.Groups))
		}
	}

	// Every table has its full set of rows from the first publish, laid out
	// from the config with the values unmeasured. Growing the rows as the
	// readings arrived made the panes collapse to one line at the top of every
	// cycle and reflow as they filled.
	for i, s := range snaps {
		if len(s.Groups) != 2 || len(s.Curves) != 2 {
			t.Errorf("snapshot %d has %d group rows and %d curve rows, want 2 and 2 throughout",
				i, len(s.Groups), len(s.Curves))
		}
	}

	// Group by group, not all of them at once. This config has two, so a
	// snapshot where one carries a reading and the other does not is the
	// evidence that the second was not waited for.
	//
	// This also catches the observer being handed rows it does not own. The
	// snapshot is copied by value, which shares the backing array of every
	// slice in it, and the cycle keeps writing each reading into its slot as
	// it lands. Without a copy of the rows, the snapshot published after the
	// first group has both groups filled in by the time anyone reads it.
	partial := false
	for _, s := range snaps {
		known := 0
		for _, g := range s.Groups {
			if state.Known(g.Value) {
				known++
			}
		}
		if known == 1 {
			partial = true
		}
	}
	if !partial {
		t.Error("no snapshot carried one group read and one not, so the groups were published together")
	}

	// The fan rows are never empty on the way through: a partial reports the
	// floors this controller believes it has commanded, with the speeds
	// unknown, rather than nothing at all.
	var lastPartial state.Snapshot
	for i, s := range snaps {
		if len(s.Fans) == 0 {
			t.Errorf("snapshot %d carries no fan rows", i)
		}
		if s.LastCycle.IsZero() {
			lastPartial = s
		}
	}
	// And the partial published after the write reports the floors that were
	// just commanded. publish fills the fan rows in only when they are
	// missing, so without a refresh at that point a cycle whose read-back then
	// failed would report the floors from before it wrote anything.
	if len(lastPartial.Fans) == 0 || !state.Known(lastPartial.Fans[0].Floor) {
		t.Errorf("the snapshot after the write still reports no commanded floor: %+v", lastPartial.Fans)
	}
}

// A cycle takes several seconds, and for all of them the previous cycle's
// readings are the best thing known. Starting the snapshot from blanks emptied
// every table at the top of each interval and refilled it as the reads landed,
// which on screen is every value on the page turning into a dash and back once
// a minute.
func TestASecondCycleShowsTheFirstOnesValuesWhileItReads(t *testing.T) {
	h := newHarness(t, testYAML)
	h.col.set("/dev/sda", 44)
	h.col.set("02-CPU 1", 55)

	// Attached before the first cycle, as main does: what a cycle carries
	// forward is what was last published, so a controller nobody is watching
	// has nothing to carry.
	var snaps []state.Snapshot
	h.ctrl.Observer = func(s state.Snapshot) { snaps = append(snaps, s) }
	h.cycle()
	snaps = nil
	h.cycle()

	if len(snaps) == 0 {
		t.Fatal("the second cycle published nothing")
	}
	first := snaps[0]

	// Every row, not only the ones this cycle happens to have reached. The
	// whole row too: a group that was blind must not read as healthy for the
	// seconds before this cycle re-judges it, which is why the previous row is
	// carried rather than only its value.
	for _, g := range first.Groups {
		if !state.Known(g.Value) {
			t.Errorf("group %q is unmeasured in the second cycle's first publish, want the first cycle's reading",
				g.Name)
		}
	}
	for _, cv := range first.Curves {
		if !state.Known(cv.Temp) || !state.Known(cv.Demand) {
			t.Errorf("curve %q reads %v/%v in the second cycle's first publish, want the first cycle's numbers",
				cv.Name, cv.Temp, cv.Demand)
		}
	}
	for _, f := range first.Fans {
		if !state.Known(f.Observed) {
			t.Errorf("fan %d has no observed speed in the second cycle's first publish, want the first cycle's read-back",
				f.Index)
		}
	}
	// The session is persistent, so it is still up while this cycle waits to
	// find that out. Blanking it made the header blink through "connecting"
	// every interval.
	if !first.Connected {
		t.Error("the second cycle's first publish reports no BMC session, want the established one")
	}

	// The other half of the contract. LastCycle is what marks a cycle
	// complete, and a consumer plotting one point per cycle would record this
	// one twice if a partial carried the previous cycle's timestamp. Err is
	// this cycle's failure or none, never the last one's.
	for i, s := range snaps[:len(snaps)-1] {
		if !s.LastCycle.IsZero() {
			t.Errorf("partial snapshot %d carries a completed-cycle time, want it withheld until this cycle finishes", i)
		}
		if s.Err != "" {
			t.Errorf("partial snapshot %d carries an error %q, want only this cycle's own", i, s.Err)
		}
	}
}

// The other side of carrying the session forward. A cycle reports the previous
// one's session as up for the several seconds before it joins the handshake,
// so the one moment that turns out to be wrong has to clear it explicitly.
func TestABMCThatGoesAwayStopsBeingReportedAsConnected(t *testing.T) {
	h := newHarness(t, testYAML)

	var last state.Snapshot
	h.ctrl.Observer = func(s state.Snapshot) { last = s }
	h.cycle()
	if !last.Connected {
		t.Fatal("the first cycle did not establish a session")
	}

	h.bmc.connectErr = errors.New("dial tcp 192.0.2.10:22: i/o timeout")
	h.cycle()
	if last.Connected {
		t.Error("the BMC is unreachable and the view still reports a session")
	}
	if last.Err == "" {
		t.Error("the cycle reported no error for an unreachable BMC")
	}
}

func groupNames(gs []state.Group) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}
