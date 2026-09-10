package tui

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/expfmt"
	// Aliased: "model" is the bubbletea model type in this package.
	promodel "github.com/prometheus/common/model"

	dto "github.com/prometheus/client_model/go"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/state"
)

// Scraper rebuilds a Snapshot from a running daemon's /metrics.
//
// This is the read-only half of the TUI. When another process already owns the
// control loop, the only honest way to show its state is to read what it
// publishes, so the view is assembled from two sources: the live numbers come
// from the exporter, and the structure they hang on (fan labels, group names,
// curve names and zones) comes from the config file on disk.
//
// That split is also why the checksum matters. The daemon exports the SHA-256
// of the config it is actually running; if the file on disk has moved on, the
// labels here describe a config nobody is running, and the UI says so rather
// than presenting the mixture as one coherent picture.
type Scraper struct {
	URL string
	cfg *config.Config
	hc  *http.Client
}

// NewScraper targets the exporter at a listen address in host:port form.
func NewScraper(listen string, cfg *config.Config) *Scraper {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "127.0.0.1", "9873"
	}
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		// A wildcard bind is not an address you can connect to.
		host = "127.0.0.1"
	}
	return &Scraper{
		URL: "http://" + net.JoinHostPort(host, port) + "/metrics",
		cfg: cfg,
		hc:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Scrape fetches and parses one sample.
func (s *Scraper) Scrape(ctx context.Context) (state.Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return state.Snapshot{}, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return state.Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return state.Snapshot{}, fmt.Errorf("%s: %s", s.URL, resp.Status)
	}

	// The zero-value parser panics: its name validation scheme is a required
	// field with no usable default, and an unset one is fatal rather than
	// lenient. UTF8Validation is the current default elsewhere in the library.
	p := expfmt.NewTextParser(promodel.UTF8Validation)
	fams, err := p.TextToMetricFamilies(resp.Body)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("parse %s: %w", s.URL, err)
	}
	return s.build(fams), nil
}

func (s *Scraper) build(fams map[string]*dto.MetricFamily) state.Snapshot {
	cfg := s.cfg
	// The daemon leaves this gauge absent until a floor has had a full
	// interval to take effect, so absence means "no verdict yet" rather than
	// "not effective". Collapsing the two would make a freshly started daemon
	// look like one whose writes are being ignored.
	effective := single(fams, "ilo_fanctl_control_effective")
	snap := state.Snapshot{
		At:             time.Now(),
		Mode:           state.ModeViewing,
		BMCHost:        cfg.ILO.Host,
		Interval:       cfg.Interval.Duration,
		ConfigChecksum: label(fams, "ilo_fanctl_config_info", "checksum"),
		Connected:      single(fams, "ilo_fanctl_ilo_connected") == 1,
		Effective:      effective == 1,
		EffectiveKnown: state.Known(effective),
		Critical:       single(fams, "ilo_fanctl_critical_active") == 1,
		BlindGroups:    int(zeroNaN(single(fams, "ilo_fanctl_blind_sensor_groups"))),
		Writes:         zeroNaN(single(fams, "ilo_fanctl_fan_writes_total")),
		Mismatches:     zeroNaN(sum(fams, "ilo_fanctl_readback_mismatch_total")),
		Cycles:         zeroNaN(single(fams, "ilo_fanctl_cycles_total")),
		LastCycle:      unixTime(single(fams, "ilo_fanctl_last_successful_cycle_timestamp_seconds")),
		StartedAt:      unixTime(single(fams, "ilo_fanctl_start_time_seconds")),
	}

	applied := byLabel(fams, "ilo_fanctl_fan_applied_percent", "fan")
	observed := byLabel(fams, "ilo_fanctl_fan_observed_percent", "fan")
	// The daemon's verdict, taken as given rather than recomputed from the two
	// gauges above. It judges each fan against the floor that has been in
	// effect for a full interval, and that floor is not exported, so comparing
	// applied against observed here is not a cheaper route to the same answer,
	// it is a different and wrong one: a fan told to go from 12 to 55 percent
	// reads as below its floor for one cycle while it spins up. Doing exactly
	// that painted all six fans of the reference machine as a reverted
	// ilo4_unlock patch every time the drives warmed, which is the one alarm in
	// this program that has to mean what it says.
	//
	// Anything other than 1 is not a mismatch, and that includes the NaN the
	// daemon publishes for a fan it could not fairly judge.
	mismatch := byLabel(fams, "ilo_fanctl_fan_mismatch", "fan")
	for _, f := range cfg.Fans {
		key := strconv.Itoa(f.Index)
		sf := state.Fan{
			Index:    f.Index,
			Label:    f.Label,
			Floor:    lookup(applied, key),
			Observed: lookup(observed, key),
			Mismatch: lookup(mismatch, key) == 1,
		}
		snap.Fans = append(snap.Fans, sf)
	}

	groupC := byLabel(fams, "ilo_fanctl_sensor_group_celsius", "group")
	groupBlind := byLabel(fams, "ilo_fanctl_sensor_group_blind", "group")
	groupHeld := byLabel(fams, "ilo_fanctl_sensor_group_held", "group")
	readings := attributeReadings(cfg, fams)

	names := make([]string, 0, len(cfg.Sensors))
	for name := range cfg.Sensors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		snap.Groups = append(snap.Groups, state.Group{
			Name:     name,
			Kind:     cfg.Sensors[name].Kind,
			Value:    lookup(groupC, name),
			Readings: readings[name],
			Blind:    lookup(groupBlind, name) == 1,
			Held:     lookup(groupHeld, name) == 1,
		})
	}

	demands := byLabel(fams, "ilo_fanctl_curve_demand_percent", "curve")
	// Which curve is driving which fan is not exported, because it is not a
	// measurement: it falls out of the demands and the zone map. Recomputing it
	// here the same way the loop does keeps one rule rather than two.
	best := map[int]float64{}
	for _, f := range cfg.Fans {
		best[f.Index] = cfg.Safety.MinFloorPct
	}
	winner := map[int]int{}
	for i, cv := range cfg.Curves {
		d := lookup(demands, cv.Name)
		sc := state.Curve{
			Name:   cv.Name,
			Sensor: cv.Sensor,
			Temp:   lookup(groupC, cv.Sensor),
			Demand: d,
		}
		sc.Fallback = !state.Known(sc.Temp) && state.Known(d)
		snap.Curves = append(snap.Curves, sc)
		if !state.Known(d) {
			continue
		}
		for _, idx := range cfg.FanIndexesForZones(cv.Zones) {
			if d > best[idx] {
				best[idx] = d
				winner[idx] = i
			}
		}
	}
	for _, i := range winner {
		snap.Curves[i].Driving = true
	}

	return snap
}

// attributeReadings maps individual sensor readings back onto the group that
// asked for them.
//
// The exporter labels a reading by source and name only, because that is what
// identifies the physical thing; the grouping is a config-side concept. So the
// association is rebuilt from the config's own match and device lists, which is
// exact for every explicitly named sensor. A "smart" group set to "auto" takes
// whatever no other group claimed by name.
func attributeReadings(cfg *config.Config, fams map[string]*dto.MetricFamily) map[string][]state.Reading {
	type key struct{ source, name string }
	owner := map[key]string{}
	autoGroup := ""
	for group, s := range cfg.Sensors {
		for _, n := range s.Match {
			owner[key{"ipmi", n}] = group
		}
		for _, d := range s.Devices {
			if d == "auto" {
				autoGroup = group
				continue
			}
			owner[key{"smart", d}] = group
		}
	}

	out := map[string][]state.Reading{}
	fam, ok := fams["ilo_fanctl_sensor_celsius"]
	if !ok {
		return out
	}
	for _, m := range fam.GetMetric() {
		var src, name string
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "source":
				src = l.GetValue()
			case "name":
				name = l.GetValue()
			}
		}
		group, found := owner[key{src, name}]
		if !found {
			if src != "smart" || autoGroup == "" {
				continue
			}
			group = autoGroup
		}
		out[group] = append(out[group], state.Reading{Name: name, Celsius: value(m)})
	}
	for g := range out {
		sort.Slice(out[g], func(a, b int) bool { return out[g][a].Name < out[g][b].Name })
	}
	return out
}

// value reads whichever of the three scalar shapes the metric carries.
func value(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Untyped != nil:
		return m.Untyped.GetValue()
	}
	return state.Unknown()
}

// single returns the value of an unlabelled family, or Unknown if the daemon
// has not published it yet.
func single(fams map[string]*dto.MetricFamily, name string) float64 {
	fam, ok := fams[name]
	if !ok || len(fam.GetMetric()) == 0 {
		return state.Unknown()
	}
	return value(fam.GetMetric()[0])
}

// sum totals every series in a family, for counters split by a label the UI
// only needs in aggregate.
func sum(fams map[string]*dto.MetricFamily, name string) float64 {
	fam, ok := fams[name]
	if !ok {
		return state.Unknown()
	}
	var total float64
	for _, m := range fam.GetMetric() {
		total += value(m)
	}
	return total
}

// byLabel indexes a family by one label's value.
func byLabel(fams map[string]*dto.MetricFamily, name, key string) map[string]float64 {
	out := map[string]float64{}
	fam, ok := fams[name]
	if !ok {
		return out
	}
	for _, m := range fam.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == key {
				out[l.GetValue()] = value(m)
				break
			}
		}
	}
	return out
}

// label returns one label's value from a single-series info metric.
func label(fams map[string]*dto.MetricFamily, name, key string) string {
	fam, ok := fams[name]
	if !ok {
		return ""
	}
	for _, m := range fam.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == key {
				return l.GetValue()
			}
		}
	}
	return ""
}

func lookup(m map[string]float64, key string) float64 {
	if v, ok := m[key]; ok {
		return v
	}
	return state.Unknown()
}

func zeroNaN(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

func unixTime(v float64) time.Time {
	if math.IsNaN(v) || v <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9))
}
