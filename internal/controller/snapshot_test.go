package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alekc/ilo-fanctl/internal/state"
	"github.com/alekc/ilo-fanctl/internal/tui"
)

// The TUI renders one shape from two sources: the loop publishes it directly,
// and a second process rebuilds it by scraping /metrics. Those two paths are
// written independently, so nothing but a test stops them describing the same
// machine differently. This is that test.
//
// It matters because the scraped path is the one an operator uses when
// something is already wrong, which is exactly when a silently wrong reading is
// most expensive.
func TestSnapshotSurvivesTheMetricsRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		check func(t *testing.T, direct state.Snapshot)
	}{
		{
			name:  "drives driving",
			setup: func(h *harness) { h.col.set("/dev/sda", 55); h.col.set("02-CPU 1", 45) },
			check: func(t *testing.T, s state.Snapshot) {
				if got := drivingCurve(s); got != "drives" {
					t.Fatalf("expected the drives curve to be driving, got %q", got)
				}
			},
		},
		{
			name:  "cpu driving",
			setup: func(h *harness) { h.col.set("/dev/sda", 30); h.col.set("02-CPU 1", 85) },
			check: func(t *testing.T, s state.Snapshot) {
				if got := drivingCurve(s); got != "cpu" {
					t.Fatalf("expected the cpu curve to be driving, got %q", got)
				}
			},
		},
		{
			// The degraded case is the one worth carrying across the wire
			// intact: a group running on a held value looks identical to a
			// healthy one unless the state is exported alongside the number.
			name: "a blind group held on its last good value",
			setup: func(h *harness) {
				h.cycle()
				h.col.fail(errors.New("smartctl exploded"))
			},
			check: func(t *testing.T, s state.Snapshot) {
				for _, g := range s.Groups {
					if !g.Blind || !g.Held {
						t.Fatalf("group %q: blind=%v held=%v, want both", g.Name, g.Blind, g.Held)
					}
				}
				if s.BlindGroups != 2 {
					t.Fatalf("blind groups = %d, want 2", s.BlindGroups)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testYAML)
			var direct state.Snapshot
			h.ctrl.Observer = func(s state.Snapshot) { direct = s }

			tc.setup(h)
			h.cycle()
			if direct.At.IsZero() {
				t.Fatal("the cycle published no snapshot")
			}
			tc.check(t, direct)

			srv := httptest.NewServer(h.m.Handler())
			defer srv.Close()
			sc := tui.NewScraper("127.0.0.1:9873", h.ctrl.snapshot())
			sc.URL = srv.URL + "/metrics"

			scraped, err := sc.Scrape(context.Background())
			if err != nil {
				t.Fatalf("scrape: %v", err)
			}
			compareSnapshots(t, direct, scraped)
			tc.check(t, scraped)
		})
	}
}

// A daemon that has not completed a cycle yet still serves /metrics. The view
// must come back empty rather than reporting zeroes as measurements.
func TestScrapeBeforeTheFirstCycle(t *testing.T) {
	h := newHarness(t, testYAML)
	srv := httptest.NewServer(h.m.Handler())
	defer srv.Close()
	sc := tui.NewScraper("127.0.0.1:9873", h.ctrl.snapshot())
	sc.URL = srv.URL + "/metrics"

	s, err := sc.Scrape(context.Background())
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if len(s.Fans) != 2 {
		t.Fatalf("fans = %d, want the 2 from the config", len(s.Fans))
	}
	for _, f := range s.Fans {
		if state.Known(f.Floor) || state.Known(f.Observed) {
			t.Errorf("fan %d: floor=%v observed=%v, want both unknown before any cycle",
				f.Index, f.Floor, f.Observed)
		}
		if f.Mismatch {
			t.Errorf("fan %d reported a mismatch with nothing measured", f.Index)
		}
	}
	if !s.LastCycle.IsZero() {
		t.Errorf("LastCycle = %v, want zero", s.LastCycle)
	}
}

// A scrape that cannot be served must fail rather than return a blank snapshot
// that reads as a healthy, idle machine.
func TestScrapeErrorsRatherThanReturningBlank(t *testing.T) {
	h := newHarness(t, testYAML)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	sc := tui.NewScraper("127.0.0.1:9873", h.ctrl.snapshot())
	sc.URL = srv.URL + "/metrics"

	if _, err := sc.Scrape(context.Background()); err == nil {
		t.Fatal("a 500 from the exporter was reported as a successful scrape")
	}
}

// A Prometheus gauge holds its last value forever. On a fan controller that
// makes an unreadable sensor indistinguishable from a cool one, which is the
// single most dangerous way for this program to be wrong, so a reading that was
// not taken this cycle must leave the registry rather than freeze.
func TestUnreadableSensorsStopBeingExported(t *testing.T) {
	h := newHarness(t, strings.Replace(testYAML, "max_hold: 1m", "max_hold: 20ms", 1))
	// scrape keys append label values in the gathered order, which Prometheus
	// sorts by label name: "name" before "source".
	const (
		drive = "ilo_fanctl_sensor_celsius|/dev/sda|smart"
		group = "ilo_fanctl_sensor_group_celsius|drives"
		held  = "ilo_fanctl_sensor_group_held|drives"
		blind = "ilo_fanctl_sensor_group_blind|drives"
	)

	h.cycle()
	if got, ok := scrape(t, h)[drive]; !ok || got != 30 {
		t.Fatalf("%s = %v (present %v), want 30", drive, got, ok)
	}

	h.col.fail(errors.New("smartctl exploded"))
	h.cycle()
	s := scrape(t, h)
	if _, ok := s[drive]; ok {
		t.Errorf("%s is still exported after the read failed", drive)
	}
	if s[blind] != 1 || s[held] != 1 {
		t.Errorf("blind=%v held=%v, want both 1", s[blind], s[held])
	}
	if got, ok := s[group]; !ok || got != 30 {
		t.Errorf("%s = %v (present %v), want the held 30 while the hold is in force", group, got, ok)
	}

	// Once the hold expires the controller is running on the fixed fallback,
	// acting on no temperature at all, and the group gauge has to go too.
	time.Sleep(30 * time.Millisecond)
	h.cycle()
	s = scrape(t, h)
	if _, ok := s[group]; ok {
		t.Errorf("%s survived the hold expiring; it reports a temperature nothing measured", group)
	}
	if s[held] != 0 {
		t.Errorf("held = %v after expiry, want 0", s[held])
	}
}

// A group renamed out of the config must not keep a series under the old name.
func TestReloadForgetsRemovedSensorGroups(t *testing.T) {
	h := newHarness(t, testYAML)
	h.cycle()
	if _, ok := scrape(t, h)["ilo_fanctl_sensor_group_celsius|cpu"]; !ok {
		t.Fatal("the cpu group was never exported")
	}

	// Drop the cpu group, and the curve and critical rule that reference it.
	trimmed := testYAML
	for _, cut := range []string{
		"  cpu:\n    kind: ipmi\n    match: [\"02-CPU 1\"]\n",
		"  - name: cpu\n    sensor: cpu\n    zones: [chassis]\n    points:\n" +
			"      - { temp: 40, pwm_pct: 10 }\n      - { temp: 90, pwm_pct: 60 }\n",
		"    - { sensor: cpu, temp: 95 }\n",
	} {
		if !strings.Contains(trimmed, cut) {
			t.Fatalf("the test config no longer contains:\n%s", cut)
		}
		trimmed = strings.Replace(trimmed, cut, "", 1)
	}
	writeConfig(t, h, trimmed)
	if err := h.ctrl.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	s := scrape(t, h)
	for _, key := range []string{
		"ilo_fanctl_sensor_group_celsius|cpu",
		"ilo_fanctl_sensor_group_blind|cpu",
		"ilo_fanctl_sensor_group_held|cpu",
		"ilo_fanctl_sensor_celsius|02-CPU 1|ipmi",
	} {
		if _, ok := s[key]; ok {
			t.Errorf("%s survived the group being removed from the config", key)
		}
	}
}

func drivingCurve(s state.Snapshot) string {
	for _, c := range s.Curves {
		if c.Driving {
			return c.Name
		}
	}
	return ""
}

// compareSnapshots asserts on every field that is supposed to survive the
// round trip. Mode, At and Err deliberately do not: the mode is what the two
// paths differ by, and the error text is not a metric.
func compareSnapshots(t *testing.T, direct, scraped state.Snapshot) {
	t.Helper()

	if scraped.Mode != state.ModeViewing {
		t.Errorf("scraped mode = %q, want %q", scraped.Mode, state.ModeViewing)
	}
	eq(t, "BMCHost", direct.BMCHost, scraped.BMCHost)
	eq(t, "ConfigChecksum", direct.ConfigChecksum, scraped.ConfigChecksum)
	eq(t, "Interval", direct.Interval, scraped.Interval)
	eq(t, "Connected", direct.Connected, scraped.Connected)
	eq(t, "Effective", direct.Effective, scraped.Effective)
	eq(t, "Critical", direct.Critical, scraped.Critical)
	eq(t, "BlindGroups", direct.BlindGroups, scraped.BlindGroups)
	eq(t, "Cycles", direct.Cycles, scraped.Cycles)
	eq(t, "Writes", direct.Writes, scraped.Writes)
	eq(t, "Mismatches", direct.Mismatches, scraped.Mismatches)
	near(t, "LastCycle", direct.LastCycle, scraped.LastCycle, time.Second)
	near(t, "StartedAt", direct.StartedAt, scraped.StartedAt, 5*time.Second)

	if len(direct.Fans) != len(scraped.Fans) {
		t.Fatalf("fans: %d direct, %d scraped", len(direct.Fans), len(scraped.Fans))
	}
	for i, a := range direct.Fans {
		b := scraped.Fans[i]
		p := fmt.Sprintf("fan[%d]", i)
		eq(t, p+".Index", a.Index, b.Index)
		eq(t, p+".Label", a.Label, b.Label)
		sameFloat(t, p+".Floor", a.Floor, b.Floor)
		sameFloat(t, p+".Observed", a.Observed, b.Observed)
		eq(t, p+".Mismatch", a.Mismatch, b.Mismatch)
	}

	if len(direct.Groups) != len(scraped.Groups) {
		t.Fatalf("groups: %d direct, %d scraped", len(direct.Groups), len(scraped.Groups))
	}
	for i, a := range direct.Groups {
		b := scraped.Groups[i]
		p := fmt.Sprintf("group[%s]", a.Name)
		eq(t, p+".Name", a.Name, b.Name)
		eq(t, p+".Kind", a.Kind, b.Kind)
		sameFloat(t, p+".Value", a.Value, b.Value)
		eq(t, p+".Blind", a.Blind, b.Blind)
		eq(t, p+".Held", a.Held, b.Held)
		if len(a.Readings) != len(b.Readings) {
			t.Errorf("%s readings: %d direct, %d scraped", p, len(a.Readings), len(b.Readings))
			continue
		}
		for j := range a.Readings {
			eq(t, fmt.Sprintf("%s.readings[%d].Name", p, j), a.Readings[j].Name, b.Readings[j].Name)
			sameFloat(t, fmt.Sprintf("%s.readings[%d].Celsius", p, j),
				a.Readings[j].Celsius, b.Readings[j].Celsius)
		}
	}

	if len(direct.Curves) != len(scraped.Curves) {
		t.Fatalf("curves: %d direct, %d scraped", len(direct.Curves), len(scraped.Curves))
	}
	for i, a := range direct.Curves {
		b := scraped.Curves[i]
		p := fmt.Sprintf("curve[%s]", a.Name)
		eq(t, p+".Name", a.Name, b.Name)
		eq(t, p+".Sensor", a.Sensor, b.Sensor)
		sameFloat(t, p+".Temp", a.Temp, b.Temp)
		sameFloat(t, p+".Demand", a.Demand, b.Demand)
		eq(t, p+".Driving", a.Driving, b.Driving)
		eq(t, p+".Fallback", a.Fallback, b.Fallback)
	}
}

func eq[T comparable](t *testing.T, what string, want, got T) {
	t.Helper()
	if want != got {
		t.Errorf("%s: direct %v, scraped %v", what, want, got)
	}
}

// sameFloat treats two unknowns as equal, which plain == does not: an
// unmeasured value is NaN by design and NaN != NaN.
func sameFloat(t *testing.T, what string, want, got float64) {
	t.Helper()
	if math.IsNaN(want) && math.IsNaN(got) {
		return
	}
	if math.Abs(want-got) > 0.001 {
		t.Errorf("%s: direct %v, scraped %v", what, want, got)
	}
}

func near(t *testing.T, what string, want, got time.Time, slack time.Duration) {
	t.Helper()
	if want.IsZero() != got.IsZero() {
		t.Errorf("%s: direct zero=%v, scraped zero=%v", what, want.IsZero(), got.IsZero())
		return
	}
	if d := want.Sub(got); d > slack || d < -slack {
		t.Errorf("%s: direct %v, scraped %v, %v apart", what, want, got, d)
	}
}
