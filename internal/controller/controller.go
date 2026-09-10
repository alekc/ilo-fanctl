// Package controller runs the sense, decide, write, verify loop.
//
// The single invariant everything here is built around: this program raises
// fan floors and never caps fan speed. iLO's own thermal curve continues to
// run underneath, free to spin the fans faster than any floor set here, so a
// bug in a curve or a stuck sensor can make the machine loud but cannot make
// it hot. The command that could make it hot, "fan p N max", is not
// implemented, has no config field, and no code path reaches it.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/curve"
	"github.com/alekc/ilo-fanctl/internal/ilo"
	"github.com/alekc/ilo-fanctl/internal/metrics"
	"github.com/alekc/ilo-fanctl/internal/sensors"
	"github.com/alekc/ilo-fanctl/internal/state"
)

// bmcClient is the slice of the BMC this loop uses.
//
// It is declared here rather than in package ilo, next to the consumer that
// needs it, so the control logic can be exercised against a fan bank whose
// responses are chosen rather than measured. *ilo.Client satisfies it.
type bmcClient interface {
	Connect(ctx context.Context) error
	Close()
	SetFanMin(ctx context.Context, index, raw int) error
	FanSpeeds(ctx context.Context) (map[int]float64, error)
	HostKeyPinned() bool
}

// Controller owns the loop, the live config, and the control state that has to
// survive a reload.
type Controller struct {
	path string
	log  *slog.Logger
	m    *metrics.Metrics

	// DryRun collects, evaluates and reads back exactly as normal, and skips
	// only the writes. Since every read here is harmless and every decision is
	// logged, this makes a new curve fully reviewable before it touches a fan.
	// The TUI toggles it at runtime, so it is read under the lock.
	dryRun atomic.Bool

	// Observer, when set, receives a snapshot several times per cycle as it
	// fills in, and again at the end, including for cycles that failed part
	// way. Only the TUI uses it, and only main sets it, before the loop starts:
	// it is not safe to attach one to a running controller, and nothing needs
	// to. It is called on the loop goroutine, so it must not block.
	Observer func(state.Snapshot)

	startedAt time.Time

	// kick asks the loop to run a cycle now instead of at the next tick. It is
	// buffered and written non-blockingly, so a UI holding the key down cannot
	// queue up cycles or stall waiting for the loop.
	kick chan struct{}

	// Counters mirrored for the snapshot. Prometheus counters cannot be read
	// back without gathering the whole registry, and the UI wants three numbers
	// every cycle, not a parse of everything.
	cycles, writes, mismatches atomic.Uint64

	// newBMC builds a client for a given BMC config. It is a field so a
	// reload that changes the connection settings can replace the live client,
	// and so tests can supply one that does not need a BMC.
	newBMC func(config.ILO) bmcClient

	mu   sync.RWMutex
	cfg  *config.Config
	sens *sensors.Registry
	bmc  bmcClient

	// applied is the floor, in percent, this controller last commanded per
	// zero-based fan index. It is control state, not config, so a reload
	// carries it across rather than resetting it.
	applied map[int]float64

	// settled is applied as it stood at the start of the current cycle, so
	// every floor in it has had a full interval to take effect. The read-back
	// is judged against this and never against a floor written moments ago.
	// See verify.
	settled map[int]float64

	// lastGood and blindSince back the sensor-error policy, per group.
	lastGood   map[string]float64
	blindSince map[string]time.Time

	// exported records which per-sensor series each group currently has on the
	// registry, so the ones a later cycle did not measure can be removed. See
	// pruneSensors.
	exported map[string]map[[2]string]bool

	// last is the snapshot as it was published most recently. A cycle starts
	// from it rather than from blanks, so a value that has been read once
	// stays on screen while the next read is in progress. Without it every
	// table emptied itself at the top of each interval and refilled a few
	// seconds later, which is worse to watch than a number that is briefly a
	// few seconds old. How old is already on screen: the header carries the
	// time of the last completed cycle.
	//
	// Its slices are never written to after publishing, only read, so sharing
	// them with the consumer that received them is safe. See carryForward for
	// what is and is not taken from it.
	last state.Snapshot
}

// New builds a controller around an already-validated config.
func New(path string, cfg *config.Config, log *slog.Logger, m *metrics.Metrics) *Controller {
	newBMC := func(c config.ILO) bmcClient { return ilo.New(c) }
	c := &Controller{
		path:       path,
		log:        log,
		m:          m,
		newBMC:     newBMC,
		cfg:        cfg,
		sens:       sensors.NewRegistry(cfg.Tools.IPMItoolPath, cfg.Tools.SmartctlPath),
		bmc:        newBMC(cfg.ILO),
		startedAt:  time.Now(),
		kick:       make(chan struct{}, 1),
		applied:    map[int]float64{},
		settled:    map[int]float64{},
		lastGood:   map[string]float64{},
		blindSince: map[string]time.Time{},
		exported:   map[string]map[[2]string]bool{},
	}
	m.ConfigLoadedUnix.SetToCurrentTime()
	m.SetConfigInfo(cfg.Checksum)
	if !c.bmc.HostKeyPinned() {
		log.Warn("ilo.host_key is not set, the BMC's identity is not verified; " +
			"pin it with: ssh-keyscan -t rsa " + cfg.ILO.Host)
	}
	return c
}

// Run drives the loop until ctx is cancelled, then releases the fans.
func (c *Controller) Run(ctx context.Context) error {
	interval := c.snapshot().Interval.Duration
	tick := time.NewTicker(interval)
	defer tick.Stop()

	// The config is polled more often than the control loop runs so an edit
	// takes effect promptly, without making the loop itself faster than the
	// BMC's shell can keep up with.
	watch := time.NewTicker(minDuration(5*time.Second, interval))
	defer watch.Stop()

	c.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			c.release()
			return ctx.Err()
		case <-watch.C:
			if c.reloadIfChanged() {
				if got := c.snapshot().Interval.Duration; got != interval {
					interval = got
					tick.Reset(interval)
					watch.Reset(minDuration(5*time.Second, interval))
					c.log.Info("control interval changed", "interval", interval)
				}
			}
		case <-c.kick:
			c.cycle(ctx)
			// Reset so an on-demand cycle does not land immediately before a
			// scheduled one; the interval exists because iLO's shell is slow.
			tick.Reset(interval)
		case <-tick.C:
			c.cycle(ctx)
		}
	}
}

// Trigger asks for a cycle now. It never blocks, and a request made while one
// is already pending is dropped rather than queued.
func (c *Controller) Trigger() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Controller) snapshot() *config.Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// state returns a consistent view of everything one cycle needs.
//
// Taken once at the top of a cycle rather than field by field, because a
// reload arriving from the SIGHUP goroutine replaces the config, the sensor
// registry and the BMC client together. Reading them separately would let a
// cycle evaluate the new curves and write them through the old, already closed
// SSH session.
func (c *Controller) state() (*config.Config, *sensors.Registry, bmcClient) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg, c.sens, c.bmc
}

// Reload re-reads the config file and swaps it in if it is valid.
//
// It fails closed. A file that does not parse, or that fails validation,
// leaves the previous config running and is reported; a daemon that stops
// regulating because someone fat-fingered a YAML key is worse than one running
// slightly stale settings.
func (c *Controller) Reload() error {
	next, err := config.Load(c.path)
	if err != nil {
		c.m.ReloadsTotal.WithLabelValues("failure").Inc()
		c.log.Error("config reload rejected, previous config still running", "error", err)
		return err
	}

	c.mu.Lock()
	prev := c.cfg
	if prev.Checksum == next.Checksum {
		c.mu.Unlock()
		return nil
	}
	if next.Listen != prev.Listen {
		// The metrics listener is already bound. Rebinding under a live
		// scrape would drop it silently, so this one field is deliberately
		// restart-only and the old value stays in force.
		c.log.Warn("listen address changed but is only applied at startup; restart to take effect",
			"running", prev.Listen, "in_file", next.Listen)
		next.Listen = prev.Listen
	}
	if !prev.SameILO(next) {
		c.log.Info("BMC connection settings changed, dropping the SSH session")
		c.bmc.Close()
		c.bmc = c.newBMC(next.ILO)
		c.m.ILOConnected.Set(0)
		if !c.bmc.HostKeyPinned() {
			c.log.Warn("ilo.host_key is not set, the BMC's identity is not verified")
		}
	}
	if !prev.SameTools(next) {
		c.sens = sensors.NewRegistry(next.Tools.IPMItoolPath, next.Tools.SmartctlPath)
	}

	// Fans dropped from the config keep whatever floor was last commanded,
	// because nothing can restore the factory minimum: iLO exposes no way to
	// read it, so there is no value to put back. They are wound down to the
	// configured baseline instead, and named in the log so the operator knows
	// a floor is still applied. "reset /map1" on the BMC clears all of them.
	stillManaged := map[int]bool{}
	for _, f := range next.Fans {
		stillManaged[f.Index] = true
	}
	var dropped []int
	for idx := range c.applied {
		if !stillManaged[idx] {
			dropped = append(dropped, idx)
		}
	}
	sort.Ints(dropped)

	// A sensor group that disappeared should not keep a stale held value
	// around to be resurrected if the group comes back under the same name,
	// and should not keep exporting either.
	for name := range prev.Sensors {
		if _, ok := next.Sensors[name]; !ok {
			c.forgetGroup(name)
		}
	}

	c.cfg = next
	bmc := c.bmc // captured under the lock; a later reload may replace it
	c.mu.Unlock()

	if len(dropped) > 0 && !c.dryRun.Load() {
		c.log.Warn("fans removed from config, winding them down to the baseline floor",
			"fans", dropped, "floor_pct", next.Safety.MinFloorPct)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for _, idx := range dropped {
			if err := bmc.SetFanMin(ctx, idx, curve.PctToRaw(next.Safety.MinFloorPct)); err != nil {
				c.log.Error("could not wind down removed fan", "fan", idx, "error", err)
				continue
			}
			c.mu.Lock()
			delete(c.applied, idx)
			delete(c.settled, idx)
			c.mu.Unlock()
			c.m.FanAppliedPct.DeleteLabelValues(fmt.Sprint(idx), labelFor(prev, idx))
		}
		cancel()
	}

	c.m.ReloadsTotal.WithLabelValues("success").Inc()
	c.m.ConfigLoadedUnix.SetToCurrentTime()
	c.m.SetConfigInfo(next.Checksum)
	c.log.Info("config reloaded", "path", c.path, "checksum", next.Checksum[:12])
	return nil
}

// reloadIfChanged is the polling half of hot reload. It returns true when a
// new config was actually swapped in.
func (c *Controller) reloadIfChanged() bool {
	before := c.snapshot().Checksum
	if err := c.Reload(); err != nil {
		return false
	}
	return c.snapshot().Checksum != before
}

// cycle is one full sense, decide, write, verify pass.
func (c *Controller) cycle(ctx context.Context) {
	start := time.Now()
	c.m.CycleTotal.Inc()
	c.cycles.Add(1)

	cfg, sens, bmc := c.state()
	snap := state.Snapshot{
		At:             start,
		Mode:           c.mode(),
		BMCHost:        cfg.ILO.Host,
		ConfigChecksum: cfg.Checksum,
		Interval:       cfg.Interval.Duration,
		StartedAt:      c.startedAt,
		// verify judges every configured fan and records the verdict on the
		// fan, in the same pass that decides Effective, so the two can never
		// be seen apart here and the UI's unnamed-mismatch fallback is never
		// the right reading of this snapshot.
		FanVerdictsKnown: true,
	}
	// Published even when the cycle fails part way, so the UI shows a BMC that
	// has gone away rather than freezing on the last good reading.
	defer func() {
		c.m.CycleSeconds.Observe(time.Since(start).Seconds())
		c.publish(cfg, &snap)
	}()

	// The BMC session has nothing to do with the sensors, so the SSH handshake
	// runs while smartctl and ipmitool do instead of after them. iLO 4 speaks
	// SHA-1 era key exchange and is not quick about it; on the DL380 that is
	// several seconds off the front of every cold cycle, and the first cycle
	// is the one the operator sits and watches. Nothing touches bmc between
	// here and the receive below, so the session is not shared while it is
	// being established.
	connected := make(chan error, 1)
	go func() { connected <- bmc.Connect(ctx) }()
	snap.Connecting = true

	// The tables have every row from the config and every value from the last
	// cycle before this one has read anything, and each reading replaces its
	// row as it lands. Building them up as the readings arrived instead made
	// the panes collapse to one line at the top of each interval and reflow as
	// they filled; blanking the values made them flicker.
	c.carryForward(cfg, &snap)

	values := make(map[string]float64, len(cfg.Sensors))
	for g := range sens.Stream(ctx, cfg) {
		sg, v, ok := c.resolveGroup(cfg, g)
		if ok {
			values[g.Name] = v
		}
		for i := range snap.Groups {
			if snap.Groups[i].Name == sg.Name {
				snap.Groups[i] = sg
				break
			}
		}
		// Counted off the rows rather than tallied as they arrive, so it means
		// the same thing part way through a cycle as it does at the end: how
		// many groups are blind as far as anyone currently knows.
		snap.BlindGroups = countBlind(snap.Groups)
		// Each group is on screen the moment it is read. Publishing only at
		// the end meant the whole view sat still for the length of the slowest
		// sensor, which made a working program look hung.
		c.publishPartial(cfg, &snap)
	}
	c.m.BlindGroups.Set(float64(snap.BlindGroups))

	demands, critical := c.decide(cfg, values, &snap)
	c.m.CriticalActive.Set(boolToFloat(critical))
	snap.Critical = critical
	c.publishPartial(cfg, &snap)

	err := <-connected
	snap.Connecting = false
	if err != nil {
		c.m.ILOConnected.Set(0)
		c.m.CycleErrors.WithLabelValues("connect").Inc()
		c.log.Error("cannot reach the BMC", "error", err)
		// Cleared explicitly, because the session carried forward from the last
		// cycle was reported as up for the whole of this one. This is the one
		// moment that assumption is proved wrong, and a header still reading
		// "connected" beside an unreachable BMC is the worst of both.
		snap.Connected = false
		snap.Err = err.Error()
		return
	}
	c.m.ILOConnected.Set(1)
	snap.Connected = true
	c.publishPartial(cfg, &snap)

	// Freeze what the fans have been running on for the past interval, before
	// this cycle's writes overwrite it. verify judges against this.
	c.freezeSettled()

	if err := c.apply(ctx, cfg, bmc, demands, critical); err != nil {
		c.m.CycleErrors.WithLabelValues("write").Inc()
		c.log.Error("applying fan floors failed", "error", err)
		snap.Err = err.Error()
		// Fall through to the read-back anyway: knowing where the fans
		// actually ended up matters more after a partial write than after a
		// clean one.
	}
	// The floors are written. Show them before the read-back, which is another
	// round trip to the BMC.
	c.publishPartial(cfg, &snap)
	if err := c.verify(ctx, cfg, bmc, &snap); err != nil {
		c.m.CycleErrors.WithLabelValues("readback").Inc()
		c.log.Error("reading fan speeds back failed", "error", err)
		snap.Err = err.Error()
		return
	}
	c.m.LastCycleUnix.SetToCurrentTime()
	snap.LastCycle = time.Now()
}

func (c *Controller) mode() state.Mode {
	if c.dryRun.Load() {
		return state.ModeDryRun
	}
	return state.ModeControlling
}

// publishPartial hands the observer everything known so far, part way through
// a cycle. The snapshot it sends deliberately has no LastCycle: this cycle has
// not finished, and a consumer plotting one point per cycle must not record
// the same cycle several times as it fills in.
func (c *Controller) publishPartial(cfg *config.Config, snap *state.Snapshot) {
	if c.Observer == nil {
		return
	}
	// The read-back has not happened yet, so the fan rows are the floors this
	// controller believes it has commanded, with the speeds unknown. Rebuilt
	// on every partial publish rather than left to publish's own fallback,
	// because apply changes the floors part way through.
	snap.Fans = c.fanState(cfg, nil)
	c.publish(cfg, snap)
}

// publish fills in the parts of the snapshot that are the same however the
// cycle ended, and hands it to the observer.
func (c *Controller) publish(cfg *config.Config, snap *state.Snapshot) {
	if c.Observer == nil {
		return
	}
	snap.Cycles = float64(c.cycles.Load())
	snap.Writes = float64(c.writes.Load())
	snap.Mismatches = float64(c.mismatches.Load())
	if snap.Fans == nil {
		// The cycle did not reach the read-back, so report the floors this
		// controller believes it has set and mark the speeds unknown.
		snap.Fans = c.fanState(cfg, nil)
	}

	// The observer gets its own row slices. Copying the snapshot struct alone
	// shares their backing arrays, and the cycle goes on writing each reading
	// into its slot as it lands, so a consumer holding an earlier snapshot
	// would see it change under it, on another goroutine, with no lock in
	// sight. The rows themselves are built once and never written again, so
	// one level of copying is enough.
	out := *snap
	out.Groups = slices.Clone(snap.Groups)
	out.Curves = slices.Clone(snap.Curves)
	out.Fans = slices.Clone(snap.Fans)

	// Remembered as the starting point for the next cycle. The copies above
	// are already private to this call, so they can be kept without another.
	c.mu.Lock()
	c.last = out
	c.mu.Unlock()

	c.Observer(out)
}

// fanState pairs the commanded floor with the observed speed for every
// configured fan. A nil speeds map means the read-back did not happen.
func (c *Controller) fanState(cfg *config.Config, speeds map[int]float64) []state.Fan {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]state.Fan, 0, len(cfg.Fans))
	for _, f := range cfg.Fans {
		sf := state.Fan{Index: f.Index, Label: f.Label, Floor: state.Unknown(), Observed: state.Unknown()}
		if v, ok := c.applied[f.Index]; ok {
			sf.Floor = v
		}
		if speeds == nil {
			// No read-back yet this cycle. Carry the last one's speed and
			// verdict rather than blanking the column: it is the most recent
			// thing known, and re-judging a stale speed against a floor that
			// has moved since would invent a verdict nothing measured.
			for _, prev := range c.last.Fans {
				if prev.Index == f.Index {
					sf.Observed, sf.Mismatch = prev.Observed, prev.Mismatch
					break
				}
			}
			out = append(out, sf)
			continue
		}
		if v, ok := speeds[f.Index]; ok {
			sf.Observed = v
			// Judged against the settled floor, not the displayed one, so
			// the UI does not paint a fan red for the one cycle it spends
			// spooling up to a floor it was only just given.
			if want, ok := c.verifiableFloorLocked(f.Index); ok {
				sf.Mismatch = state.Mismatched(want, v)
			}
		}
		out = append(out, sf)
	}
	return out
}

// carryForward starts a cycle's snapshot from what is currently known, rather
// than from blanks that fill in over the next several seconds.
//
// Every table keeps its shape from the config, so a reload that adds or
// removes a group or a curve shows immediately, and every row keeps the whole
// of what was last known about it. The whole row, not only its value: a group
// that was blind, or running on a held value, must not read as healthy for the
// few seconds before this cycle re-judges it.
//
// Deliberately not carried: LastCycle, which is what marks a cycle complete
// and has to stay zero until this one is; Err, which is this cycle's failure
// or none at all; and everything the cycle takes from the config at the top.
func (c *Controller) carryForward(cfg *config.Config, snap *state.Snapshot) {
	c.mu.RLock()
	prev := c.last
	c.mu.RUnlock()

	// An established session stays established: Connect is a no-op when the
	// session is already open, but this cycle does not learn that until it
	// joins the handshake, and the header must not blink through "connecting"
	// every interval on the strength of not having asked yet.
	snap.Connected = prev.Connected
	snap.Critical = prev.Critical
	snap.Effective, snap.EffectiveKnown = prev.Effective, prev.EffectiveKnown

	// Name order rather than map order, because map order is random per
	// iteration and these are table rows.
	groups := make([]state.Group, 0, len(cfg.Sensors))
	for name, s := range cfg.Sensors {
		g := state.Group{Name: name, Kind: s.Kind, Value: state.Unknown()}
		for _, p := range prev.Groups {
			if p.Name == name && p.Kind == s.Kind {
				g = p
				break
			}
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	snap.Groups = groups
	snap.BlindGroups = countBlind(groups)

	// Configuration order, which is the order decide fills them in.
	curves := make([]state.Curve, 0, len(cfg.Curves))
	for _, cv := range cfg.Curves {
		sc := state.Curve{
			Name:   cv.Name,
			Sensor: cv.Sensor,
			Temp:   state.Unknown(),
			Demand: state.Unknown(),
		}
		for _, p := range prev.Curves {
			if p.Name == cv.Name && p.Sensor == cv.Sensor {
				sc = p
				break
			}
		}
		curves = append(curves, sc)
	}
	snap.Curves = curves
}

func countBlind(gs []state.Group) int {
	n := 0
	for _, g := range gs {
		if g.Blind {
			n++
		}
	}
	return n
}

// resolveGroup turns one raw collector result into a usable value, applying
// the sensor-error policy where the read failed. It returns the group as the
// UI will show it and, when there is one, the value the curves should run on.
// No value at all means the caller substitutes fixed_pct directly as the
// demand, because there is no temperature to put through a curve.
//
// Every side effect in here, the metrics, the held value, the blind-since
// clock and the log line, happens exactly once per group per cycle. That is
// why the caller resolves each group as it arrives and keeps the result,
// rather than re-resolving the set to redraw it.
func (c *Controller) resolveGroup(cfg *config.Config, g sensors.Group) (state.Group, float64, bool) {
	now := time.Now()
	name := g.Name

	c.mu.Lock()
	defer c.mu.Unlock()

	sg := state.Group{Name: name, Kind: cfg.Sensors[name].Kind, Value: state.Unknown()}
	for _, r := range g.Readings {
		c.m.SensorCelsius.WithLabelValues(r.Source, r.Name).Set(r.Celsius)
		sg.Readings = append(sg.Readings, state.Reading{Name: r.Name, Celsius: r.Celsius})
	}
	c.pruneSensors(name, g.Readings)

	// Every exit below goes through here, so the exported state of a group and
	// the state the UI renders cannot drift apart.
	finish := func(sg state.Group, value float64, ok bool) (state.Group, float64, bool) {
		c.m.GroupBlind.WithLabelValues(sg.Name).Set(boolToFloat(sg.Blind))
		c.m.GroupHeld.WithLabelValues(sg.Name).Set(boolToFloat(sg.Held))
		return sg, value, ok
	}

	if g.Err == nil {
		c.m.GroupCelsius.WithLabelValues(name).Set(g.Value)
		c.lastGood[name] = g.Value
		delete(c.blindSince, name)
		sg.Value = g.Value
		return finish(sg, g.Value, true)
	}

	sg.Blind = true
	sg.Err = g.Err.Error()
	c.m.SensorErrors.WithLabelValues(name).Inc()
	if _, ok := c.blindSince[name]; !ok {
		c.blindSince[name] = now
	}
	c.log.Error("sensor group unreadable", "group", name, "error", g.Err,
		"blind_for", now.Sub(c.blindSince[name]).Round(time.Second))

	// Partial reads still count. A group of six drives where one fails
	// has a real maximum from the other five, and that is better
	// information than the fallback.
	if len(g.Readings) > 0 {
		c.m.GroupCelsius.WithLabelValues(name).Set(g.Value)
		c.lastGood[name] = g.Value
		sg.Value = g.Value
		return finish(sg, g.Value, true)
	}

	p := cfg.Safety.OnSensorError
	held, haveHeld := c.lastGood[name]
	expired := now.Sub(c.blindSince[name]) > p.MaxHold.Duration
	if p.Action == "hold_last" && haveHeld && !expired {
		sg.Value = held
		sg.Held = true
		c.m.GroupCelsius.WithLabelValues(name).Set(held)
		return finish(sg, held, true)
	}
	// Nothing is being acted on for this group, so nothing should be exported
	// for it either. Left in place the gauge would keep serving its last value
	// indefinitely, which reads as a current measurement.
	c.m.GroupCelsius.DeleteLabelValues(name)
	return finish(sg, 0, false)
}

// pruneSensors removes the per-sensor series a group used to export and did not
// measure this cycle. The caller holds the write lock.
//
// A Prometheus gauge holds its last value forever, so a drive that dropped off
// the bus, or a whole group whose collector failed, would keep reporting the
// temperature it had when it was last readable. On a fan controller that is the
// worst kind of wrong: it looks exactly like a cool, healthy component.
func (c *Controller) pruneSensors(group string, readings []sensors.Reading) {
	cur := make(map[[2]string]bool, len(readings))
	for _, r := range readings {
		cur[[2]string{r.Source, r.Name}] = true
	}
	for k := range c.exported[group] {
		if !cur[k] {
			c.m.SensorCelsius.DeleteLabelValues(k[0], k[1])
		}
	}
	if len(cur) == 0 {
		delete(c.exported, group)
		return
	}
	c.exported[group] = cur
}

// forgetGroup drops everything exported for a group that is no longer
// configured, so a rename does not leave the old name on the dashboard forever.
// The caller holds the write lock.
func (c *Controller) forgetGroup(name string) {
	c.pruneSensors(name, nil)
	c.m.GroupCelsius.DeleteLabelValues(name)
	c.m.GroupBlind.DeleteLabelValues(name)
	c.m.GroupHeld.DeleteLabelValues(name)
	delete(c.lastGood, name)
	delete(c.blindSince, name)
}

// decide evaluates every curve and returns the demanded floor per fan index.
//
// Where several curves cover the same fan, the highest demand wins. That is
// the whole answer to "the CPU is hot but the drives are cold": the cpu curve
// and the drives curve raise the same fans, and neither can hold the other
// down.
func (c *Controller) decide(cfg *config.Config, values map[string]float64, snap *state.Snapshot) (map[int]float64, bool) {
	fallback := cfg.Safety.OnSensorError.FixedPct
	demands := make(map[int]float64, len(cfg.Fans))
	for _, f := range cfg.Fans {
		demands[f.Index] = cfg.Safety.MinFloorPct
	}

	// winner tracks which curve currently owns each fan, so the UI can show
	// which sensor is actually in charge rather than only the resulting
	// number. With one airflow zone that is a single curve; with several it
	// can legitimately be a different one per zone.
	winner := map[int]int{}

	// Built here and assigned at the end rather than appended to snap.Curves,
	// which already holds a placeholder row per curve so the table has its
	// full height before anything has been decided.
	curves := make([]state.Curve, 0, len(cfg.Curves))
	for i, cv := range cfg.Curves {
		sc := state.Curve{Name: cv.Name, Sensor: cv.Sensor, Temp: state.Unknown()}
		var demand float64
		if temp, ok := values[cv.Sensor]; ok {
			demand = curve.Eval(cv.Points, temp)
			sc.Temp = temp
		} else {
			demand = fallback
			sc.Fallback = true
		}
		sc.Demand = demand
		c.m.CurveDemandPct.WithLabelValues(cv.Name).Set(demand)
		for _, idx := range cfg.FanIndexesForZones(cv.Zones) {
			if demand > demands[idx] {
				demands[idx] = demand
				winner[idx] = i
			}
		}
		curves = append(curves, sc)
	}
	for _, i := range winner {
		if i < len(curves) {
			curves[i].Driving = true
		}
	}
	snap.Curves = curves

	// A critical threshold bypasses the curves and the ramp-down limit
	// entirely. It applies to every fan, not only the ones in the tripping
	// sensor's zones: at this point the question is no longer which component
	// is hot, it is getting air through the chassis.
	critical := false
	for _, cr := range cfg.Safety.Critical {
		temp, ok := values[cr.Sensor]
		if !ok || temp < cr.Temp {
			continue
		}
		critical = true
		c.log.Warn("critical temperature exceeded, going to the maximum floor",
			"sensor", cr.Sensor, "celsius", temp, "threshold", cr.Temp,
			"floor_pct", cfg.Safety.MaxFloorPct)
	}
	if critical {
		for idx := range demands {
			demands[idx] = cfg.Safety.MaxFloorPct
		}
	}

	for idx, d := range demands {
		demands[idx] = clamp(d, cfg.Safety.MinFloorPct, cfg.Safety.MaxFloorPct)
	}
	return demands, critical
}

// apply writes the floors that actually changed.
func (c *Controller) apply(ctx context.Context, cfg *config.Config, bmc bmcClient, demands map[int]float64, critical bool) error {
	idxs := make([]int, 0, len(demands))
	for idx := range demands {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)

	var errs []error
	for _, idx := range idxs {
		target := demands[idx]

		c.mu.RLock()
		prev, had := c.applied[idx]
		c.mu.RUnlock()

		if had && !critical {
			// Floors rise at once and fall slowly. Ramping down damps
			// oscillation around a curve knee, and the direction it errs in
			// is more cooling rather than less.
			if target < prev {
				if prev-target < cfg.Safety.HysteresisPct {
					continue
				}
				if step := cfg.Safety.RampDownPerCyclePct; step > 0 && prev-target > step {
					target = prev - step
				}
			}
		}
		target = clamp(target, cfg.Safety.MinFloorPct, cfg.Safety.MaxFloorPct)

		raw := curve.PctToRaw(target)
		// Compare on the wire scale, not in percent. The BMC takes 0..255, so
		// two demands that round to the same raw value are the same command,
		// and re-sending it every cycle would be pure SSH traffic.
		if had && raw == curve.PctToRaw(prev) {
			continue
		}
		if c.dryRun.Load() {
			c.log.Info("dry run, not writing", "fan", idx, "label", labelFor(cfg, idx),
				"would_set_pct", math.Round(target*10)/10, "raw", raw, "critical", critical)
			continue
		}
		if err := bmc.SetFanMin(ctx, idx, raw); err != nil {
			errs = append(errs, fmt.Errorf("fan %d: %w", idx, err))
			continue
		}
		c.m.WritesTotal.Inc()
		c.writes.Add(1)

		actual := curve.RawToPct(raw)
		c.mu.Lock()
		c.applied[idx] = actual
		c.mu.Unlock()
		c.m.FanAppliedPct.WithLabelValues(fmt.Sprint(idx), labelFor(cfg, idx)).Set(actual)
		c.log.Info("fan floor set", "fan", idx, "label", labelFor(cfg, idx),
			"percent", math.Round(actual*10)/10, "raw", raw, "critical", critical)
	}
	return errors.Join(errs...)
}

// verify reads the speeds back and reports whether the floors took effect.
//
// This is the only thing standing between a working controller and a silently
// broken one. The fan commands print nothing on success and nothing on
// failure, and an HPE firmware upgrade reverts the ilo4_unlock patch without
// changing that, so a machine can go from regulated to unregulated with every
// write still "succeeding". The gap between applied and observed is where
// that shows up.
func (c *Controller) verify(ctx context.Context, cfg *config.Config, bmc bmcClient, snap *state.Snapshot) error {
	speeds, err := bmc.FanSpeeds(ctx)
	if err != nil {
		return err
	}
	snap.Fans = c.fanState(cfg, speeds)

	c.mu.RLock()
	defer c.mu.RUnlock()

	effective := true
	judged := false
	for _, f := range cfg.Fans {
		idx := fmt.Sprint(f.Index)
		observed, ok := speeds[f.Index]
		if !ok {
			// Nothing was read for this fan, so there is no verdict to publish
			// about it. NaN rather than 0 for the same reason the effective
			// gauge starts there: an absence of evidence must not read as an
			// assurance of health.
			c.m.FanMismatch.WithLabelValues(idx, f.Label).Set(state.Unknown())
			continue
		}
		c.m.FanObservedPct.WithLabelValues(idx, f.Label).Set(observed)

		want, ok := c.verifiableFloorLocked(f.Index)
		if !ok {
			c.m.FanMismatch.WithLabelValues(idx, f.Label).Set(state.Unknown())
			continue
		}
		judged = true
		// The BMC reports its own desired speed, which is the higher of our
		// floor and whatever its curve wants. So observed above the floor is
		// normal and expected; only observed below it is a problem.
		bad := state.Mismatched(want, observed)
		// Published, not left to be recomputed downstream. The floor this was
		// judged against is the settled one, which never leaves this process,
		// so a reader holding only the applied and observed gauges cannot
		// arrive at the same answer and will call a fan that is merely still
		// spinning up a reverted ilo4_unlock patch.
		c.m.FanMismatch.WithLabelValues(idx, f.Label).Set(boolToFloat(bad))
		if bad {
			effective = false
			c.m.MismatchTotal.WithLabelValues(idx).Inc()
			c.mismatches.Add(1)
			c.log.Error("commanded floor is not being honoured by the BMC",
				"fan", f.Index, "label", f.Label,
				"commanded_pct", math.Round(want*10)/10, "observed_pct", observed,
				"hint", "an HPE firmware upgrade reverts the ilo4_unlock patch")
		}
	}
	// Until at least one fan has been judged there is no evidence either way,
	// and claiming control is effective on no evidence is the one thing this
	// whole read-back exists to avoid. Leave the gauge absent instead.
	if judged {
		c.m.ControlEffective.Set(boolToFloat(effective))
	}
	// Unjudged reads as false rather than true so that the direct snapshot and
	// one rebuilt from an absent gauge say the same thing. Consumers must test
	// EffectiveKnown before reading it either way.
	snap.Effective = effective && judged
	snap.EffectiveKnown = judged
	return nil
}

// verifiableFloor returns the floor fan idx can fairly be held to right now.
//
// Two floors are in play at read-back time: the one that has been in effect
// for the past interval, and the one written seconds ago. Only the first has
// had time to show up in the BMC's reported speed, so a fan told to go from
// 12 to 55 percent must not be judged against 55 yet. Taking the lower of the
// two covers both directions: a raised floor is judged against the old one
// until the next cycle, and a lowered floor is judged against the new one,
// which the BMC applies to its reported speed at once.
//
// Judging against the just-written floor is what the first live run on the
// reference machine did, and it reported all six fans as unhonoured on every
// startup: a false positive on the single signal that means the ilo4_unlock
// patch has been reverted.
// The caller must hold c.mu.
func (c *Controller) verifiableFloorLocked(idx int) (float64, bool) {
	settled, hadSettled := c.settled[idx]
	if !hadSettled {
		// Nothing has been in effect for a full interval, so there is nothing
		// this fan can be held to.
		return 0, false
	}
	if current, ok := c.applied[idx]; ok && current < settled {
		return current, true
	}
	return settled, true
}

// freezeSettled copies the floors in effect into settled, before this cycle's
// writes change them.
func (c *Controller) freezeSettled() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.settled)
	for idx, pct := range c.applied {
		c.settled[idx] = pct
	}
}

// release winds every managed fan down to the configured baseline on shutdown.
//
// Leaving a high floor applied after the daemon exits would keep the machine
// loud indefinitely with nothing left to lower it. Winding down to
// min_floor_pct rather than to zero is deliberate: iLO offers no way to read
// the factory per-fan minimum, so there is no original value to restore, and
// commanding zero would permit the fans below whatever the factory floor was.
// "reset /map1" on the BMC clears the floors properly.
func (c *Controller) release() {
	c.mu.RLock()
	cfg, bmc := c.cfg, c.bmc
	idxs := make([]int, 0, len(c.applied))
	for idx := range c.applied {
		idxs = append(idxs, idx)
	}
	c.mu.RUnlock()
	if len(idxs) == 0 || c.dryRun.Load() {
		bmc.Close()
		return
	}
	sort.Ints(idxs)

	// A fresh context: the one that cancelled is why we are here.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.log.Info("shutting down, winding fans to the baseline floor",
		"fans", idxs, "floor_pct", cfg.Safety.MinFloorPct)
	for _, idx := range idxs {
		if err := bmc.SetFanMin(ctx, idx, curve.PctToRaw(cfg.Safety.MinFloorPct)); err != nil {
			c.log.Error("could not wind fan down on shutdown", "fan", idx, "error", err)
		}
	}
	bmc.Close()
	c.m.ILOConnected.Set(0)
}

func labelFor(cfg *config.Config, idx int) string {
	for _, f := range cfg.Fans {
		if f.Index == idx {
			return f.Label
		}
	}
	return ""
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SetDryRun turns writing off or on while the loop is running. The TUI uses it
// to pause writes without restarting anything. Turning it off does not undo
// floors already applied; it only stops new ones being sent.
func (c *Controller) SetDryRun(v bool) { c.dryRun.Store(v) }

// DryRun reports whether writes are currently suppressed.
func (c *Controller) DryRun() bool { return c.dryRun.Load() }
