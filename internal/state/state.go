// Package state carries the point-in-time view of the controller that the TUI
// renders.
//
// It is a package of its own, depending on nothing, because the same shape is
// produced two different ways: directly by the control loop when the TUI is
// driving it, and by scraping a running daemon's /metrics when it is not. The
// renderer must not be able to tell which one it is looking at.
package state

import (
	"math"
	"time"
)

// Unknown marks a value that was not measured this cycle, which is different
// from a value that was measured as zero. Rendering has to tell those apart:
// a fan whose speed could not be read is not a fan that is stopped.
func Unknown() float64 { return math.NaN() }

// Known reports whether a value was actually measured.
func Known(v float64) bool { return !math.IsNaN(v) }

// ReadbackTolerancePct is how far the BMC's reported speed may sit below the
// commanded floor before it counts as a mismatch.
//
// Two things make an exact comparison wrong. The PWM scale is 0..255, so one
// step is 0.39 percent, and the CLP reports whole percent only. Two points of
// slack absorbs both without hiding a floor that is being ignored outright,
// which is what this check exists to catch.
//
// The tolerance is smaller in practice than it looks. The CLP's whole-percent
// rounding alone eats most of a point at the speeds this runs at, so what is
// left over for a fan that is genuinely still moving is closer to one point
// than to two.
const ReadbackTolerancePct = 2.0

// Mismatched reports whether the BMC is running a fan below the floor that was
// commanded for it. That is the reverted-patch signal: an HPE firmware upgrade
// removes the ilo4_unlock patch without making the write fail, so this gap is
// the only place it shows up.
//
// The controller is the only caller, and that is deliberate rather than
// incidental. The floor passed here is the one that has been in effect for a
// full interval, which only the loop knows, so a fan just told to go from 12 to
// 55 percent is judged against 12 and not called a failure for spinning up.
// Nothing outside the process can reproduce that, which is why the verdict is
// exported as ilo_fanctl_fan_mismatch instead of the inputs to it. The TUI used
// to call this itself on the applied and observed gauges, and reported all six
// fans of the reference machine as an ignored floor every time the drives
// warmed enough to raise one.
func Mismatched(floor, observed float64) bool {
	return Known(floor) && Known(observed) && observed < floor-ReadbackTolerancePct
}

// Reading is one physical sensor.
type Reading struct {
	Name    string
	Celsius float64
}

// Group is one configured sensor group after aggregation.
type Group struct {
	Name     string
	Kind     string
	Value    float64
	Readings []Reading

	// Blind means the collector failed this cycle. Held additionally means the
	// controller is running on a previously good value rather than a fallback,
	// which is worth showing differently: one is degraded, the other is
	// guessing.
	Blind bool
	Held  bool
	Err   string
}

// Fan pairs what was commanded with what the BMC reports.
type Fan struct {
	Index    int
	Label    string
	Floor    float64 // percent this controller commanded, Unknown if never
	Observed float64 // percent the BMC reports, Unknown if not read
	// Mismatch is the reverted-patch signal: the BMC is running the fan below
	// the floor that was commanded for it.
	Mismatch bool
}

// Curve is one evaluated curve.
type Curve struct {
	Name   string
	Sensor string
	Temp   float64 // Unknown when the sensor could not be resolved
	Demand float64
	// Driving means this curve won the maximum for at least one fan, so it is
	// the one currently deciding that fan's speed.
	Driving bool
	// Fallback means the demand came from the sensor-error policy rather than
	// from evaluating the curve.
	Fallback bool
}

// Mode is how the TUI is attached.
type Mode string

const (
	// ModeControlling means this process owns the control loop.
	ModeControlling Mode = "CONTROLLING"
	// ModeDryRun means it owns the loop but will not write.
	ModeDryRun Mode = "DRY RUN"
	// ModeViewing means another process owns the loop and this one is reading
	// its exported metrics.
	ModeViewing Mode = "VIEWING"
)

// Snapshot is one cycle's worth of everything the UI shows.
type Snapshot struct {
	At        time.Time
	Mode      Mode
	BMCHost   string
	Connected bool
	// Connecting is set while the BMC handshake is in flight. Without it a
	// session being opened is indistinguishable from one that failed, and the
	// first seconds of every startup reported a fault that was not there. Only
	// the process running the loop can know this; a read-only view attached to
	// another daemon's metrics sees an established session or none.
	Connecting     bool
	ConfigChecksum string
	Interval       time.Duration
	LastCycle      time.Time
	StartedAt      time.Time

	Fans   []Fan
	Groups []Group
	Curves []Curve

	// Effective is only meaningful when EffectiveKnown is set. Before the
	// first floor has had a full interval to take effect there is no evidence
	// either way, and a controller that claims its writes are working on no
	// evidence is the exact failure this program is built to detect.
	Effective      bool
	EffectiveKnown bool

	Critical    bool
	BlindGroups int
	Writes      float64
	Mismatches  float64
	Cycles      float64

	// Err is set when the cycle could not complete, for instance because the
	// BMC was unreachable. The rest of the snapshot is still populated as far
	// as the cycle got.
	Err string
}
