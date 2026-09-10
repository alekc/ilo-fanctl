// Package config defines the on-disk YAML schema and validates it eagerly.
//
// Every constraint this program relies on is checked here, at load time, so a
// misconfiguration fails the process rather than surfacing later as a fan that
// quietly never moves. There are deliberately no silent defaults for anything
// that affects cooling.
package config

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration lets the YAML carry human durations such as "30s" or "350ms".
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Config is the whole file.
type Config struct {
	ILO      ILO               `yaml:"ilo"`
	Interval Duration          `yaml:"interval"`
	Listen   string            `yaml:"listen"`
	Tools    Tools             `yaml:"tools"`
	Fans     []Fan             `yaml:"fans"`
	Sensors  map[string]Sensor `yaml:"sensors"`
	Curves   []Curve           `yaml:"curves"`
	Safety   Safety            `yaml:"safety"`

	// Checksum is the SHA-256 of the bytes this config was decoded from. The
	// reload watcher compares it rather than the file's mtime: an editor that
	// writes via a temp file and rename produces a new inode, and some
	// deployment tools restore the original timestamp, so mtime both misses
	// real changes and reports changes that are not there.
	Checksum string `yaml:"-"`
}

// Tools locates the external binaries the collectors shell out to.
//
// Both default to a bare name resolved through PATH. Pinning absolute paths is
// worth doing for a daemon: systemd's PATH is not a login shell's, and
// smartctl in particular lives in /usr/sbin, which is often absent from it.
type Tools struct {
	IPMItoolPath string `yaml:"ipmitool_path"`
	SmartctlPath string `yaml:"smartctl_path"`
}

// ILO holds connection details for the BMC.
//
// The crypto lists are configurable because iLO 4 speaks SHA-1 era algorithms
// that current Go and OpenSSH both disable by default, and the exact set that
// works varies across firmware builds. Hard-coding them would mean recompiling
// to talk to a slightly different BMC.
type ILO struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	User string `yaml:"user"`

	// Exactly one auth source must be set.
	PrivateKeyPath string `yaml:"private_key_path"`
	PasswordFile   string `yaml:"password_file"`

	// HostKey is the pinned public key in authorized_keys form. Leaving it
	// empty is permitted but logged loudly at every startup, because an
	// unauthenticated BMC channel is worth noticing.
	HostKey string `yaml:"host_key"`

	KexAlgorithms     []string `yaml:"kex_algorithms"`
	HostKeyAlgorithms []string `yaml:"host_key_algorithms"`
	Ciphers           []string `yaml:"ciphers"`
	MACs              []string `yaml:"macs"`

	ConnectTimeout Duration `yaml:"connect_timeout"`
	// CommandInterval paces writes into the BMC shell. iLO 4's SSH server is
	// slow enough that commands sent back to back are dropped; upstream
	// reports settled on roughly 350ms.
	CommandInterval Duration `yaml:"command_interval"`
}

// Fan maps an iLO fan index to a label and one or more zones.
//
// The index is zero-based: index 0 is the fan the BMC labels "Fan 1". This is
// confirmed behaviour on Gen8 and Gen9, and getting it wrong addresses the
// neighbouring fan silently.
type Fan struct {
	Index int      `yaml:"index"`
	Label string   `yaml:"label"`
	Zones []string `yaml:"zones"`
}

// Sensor describes one temperature source.
type Sensor struct {
	// Kind is "smart" or "ipmi".
	Kind string `yaml:"kind"`
	// Devices applies to kind "smart". The single entry "auto" discovers
	// block devices via smartctl --scan.
	Devices []string `yaml:"devices"`
	// Match applies to kind "ipmi" and lists exact BMC sensor names.
	Match []string `yaml:"match"`
	// Aggregate is "max" or "mean". Default "max".
	Aggregate string `yaml:"aggregate"`
}

// Point is one vertex of a piecewise linear curve.
type Point struct {
	Temp   float64 `yaml:"temp"`
	PWMPct float64 `yaml:"pwm_pct"`
}

// Curve maps a sensor's aggregated temperature onto a demanded fan floor.
type Curve struct {
	Name   string   `yaml:"name"`
	Sensor string   `yaml:"sensor"`
	Zones  []string `yaml:"zones"`
	Points []Point  `yaml:"points"`
}

// Critical raises the floor to Safety.MaxFloorPct when a sensor crosses Temp.
type Critical struct {
	Sensor string  `yaml:"sensor"`
	Temp   float64 `yaml:"temp"`
}

// SensorErrorPolicy decides what happens when a collector fails.
//
// Doing nothing is not an option: a controller that stops regulating because
// it cannot read a disk is indistinguishable, from the fans' point of view,
// from one that decided no cooling was needed.
type SensorErrorPolicy struct {
	// Action is "hold_last" or "fixed".
	Action   string   `yaml:"action"`
	MaxHold  Duration `yaml:"max_hold"`
	FixedPct float64  `yaml:"fixed_pct"`
}

// Safety carries the clamps and the smoothing behaviour.
//
// Note what is absent: there is no maximum-speed setting anywhere in this
// schema, and none in the program. iLO's "fan p N max" command caps a fan and
// can therefore starve the machine of cooling; upstream attributes a
// fans-to-100-percent thermal shutdown partly to one left applied. This
// program only ever raises floors, so the BMC's own curve remains free to run
// the fans as fast as it wants.
type Safety struct {
	MaxFloorPct         float64           `yaml:"max_floor_pct"`
	MinFloorPct         float64           `yaml:"min_floor_pct"`
	HysteresisPct       float64           `yaml:"hysteresis_pct"`
	RampDownPerCyclePct float64           `yaml:"ramp_down_per_cycle_pct"`
	OnSensorError       SensorErrorPolicy `yaml:"on_sensor_error"`
	Critical            []Critical        `yaml:"critical"`
}

// Load reads, parses and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo'd key is an error, not a silent default
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.Checksum = fmt.Sprintf("%x", sha256.Sum256(raw))
	return &c, nil
}

// SameILO reports whether two configs describe the same BMC connection.
//
// A reload that changes any of this has to tear the SSH session down and
// build a new one, so the controller needs to know.
func (c *Config) SameILO(o *Config) bool {
	return reflect.DeepEqual(c.ILO, o.ILO)
}

// SameTools reports whether the collector binaries are unchanged.
func (c *Config) SameTools(o *Config) bool { return c.Tools == o.Tools }

func (c *Config) applyDefaults() {
	if c.ILO.Port == 0 {
		c.ILO.Port = 22
	}
	if c.Interval.Duration == 0 {
		c.Interval.Duration = 30 * time.Second
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9873"
	}
	if c.ILO.ConnectTimeout.Duration == 0 {
		c.ILO.ConnectTimeout.Duration = 15 * time.Second
	}
	if c.ILO.CommandInterval.Duration == 0 {
		c.ILO.CommandInterval.Duration = 350 * time.Millisecond
	}
	if len(c.ILO.KexAlgorithms) == 0 {
		c.ILO.KexAlgorithms = []string{"diffie-hellman-group14-sha1"}
	}
	if len(c.ILO.HostKeyAlgorithms) == 0 {
		c.ILO.HostKeyAlgorithms = []string{"ssh-rsa"}
	}
	if c.Safety.MaxFloorPct == 0 {
		c.Safety.MaxFloorPct = 70
	}
	if c.Safety.OnSensorError.Action == "" {
		c.Safety.OnSensorError.Action = "hold_last"
	}
	if c.Safety.OnSensorError.MaxHold.Duration == 0 {
		c.Safety.OnSensorError.MaxHold.Duration = 5 * time.Minute
	}
	for name, s := range c.Sensors {
		if s.Aggregate == "" {
			s.Aggregate = "max"
			c.Sensors[name] = s
		}
	}
}

// Validate enforces every invariant the control loop assumes.
func (c *Config) Validate() error {
	if c.ILO.Host == "" {
		return fmt.Errorf("ilo.host is required")
	}
	if c.ILO.User == "" {
		return fmt.Errorf("ilo.user is required")
	}
	switch {
	case c.ILO.PrivateKeyPath != "" && c.ILO.PasswordFile != "":
		return fmt.Errorf("ilo: set exactly one of private_key_path or password_file, not both")
	case c.ILO.PrivateKeyPath == "" && c.ILO.PasswordFile == "":
		return fmt.Errorf("ilo: one of private_key_path or password_file is required")
	}
	if err := readable(c.ILO.PrivateKeyPath); err != nil {
		return err
	}
	if err := readable(c.ILO.PasswordFile); err != nil {
		return err
	}
	if c.Interval.Duration < time.Second {
		return fmt.Errorf("interval %s is too short; iLO's SSH shell cannot keep up", c.Interval)
	}
	if len(c.Fans) == 0 {
		return fmt.Errorf("at least one fan must be configured")
	}
	seenIdx := map[int]bool{}
	zones := map[string]bool{}
	for i, f := range c.Fans {
		if f.Index < 0 {
			return fmt.Errorf("fans[%d]: index must be >= 0 (indexing is zero-based)", i)
		}
		if seenIdx[f.Index] {
			return fmt.Errorf("fans[%d]: duplicate index %d", i, f.Index)
		}
		seenIdx[f.Index] = true
		if len(f.Zones) == 0 {
			return fmt.Errorf("fans[%d] (index %d): at least one zone is required", i, f.Index)
		}
		for _, z := range f.Zones {
			zones[z] = true
		}
	}
	// Every sensor group is validated, not only the ones a curve references. A
	// group with no curve is still collected and exported as a metric, so a
	// bad "kind" there fails at runtime rather than at startup if this loop
	// only covers curve targets.
	if len(c.Sensors) == 0 {
		return fmt.Errorf("at least one sensor must be configured")
	}
	for name, s := range c.Sensors {
		switch s.Kind {
		case "smart":
			if len(s.Devices) == 0 {
				return fmt.Errorf("sensors[%q]: kind \"smart\" requires devices (or the single entry \"auto\")", name)
			}
			if len(s.Match) > 0 {
				return fmt.Errorf("sensors[%q]: match applies to kind \"ipmi\", not \"smart\"", name)
			}
		case "ipmi":
			if len(s.Match) == 0 {
				return fmt.Errorf("sensors[%q]: kind \"ipmi\" requires match, listing exact BMC sensor names", name)
			}
			if len(s.Devices) > 0 {
				return fmt.Errorf("sensors[%q]: devices applies to kind \"smart\", not \"ipmi\"", name)
			}
		default:
			return fmt.Errorf("sensors[%q]: kind must be \"smart\" or \"ipmi\", got %q", name, s.Kind)
		}
		if s.Aggregate != "max" && s.Aggregate != "mean" {
			return fmt.Errorf("sensors[%q]: aggregate must be \"max\" or \"mean\", got %q", name, s.Aggregate)
		}
	}
	if len(c.Curves) == 0 {
		return fmt.Errorf("at least one curve must be configured")
	}
	for i, cv := range c.Curves {
		if cv.Name == "" {
			return fmt.Errorf("curves[%d]: name is required", i)
		}
		if _, ok := c.Sensors[cv.Sensor]; !ok {
			return fmt.Errorf("curves[%q]: unknown sensor %q", cv.Name, cv.Sensor)
		}
		if len(cv.Zones) == 0 {
			return fmt.Errorf("curves[%q]: at least one zone is required", cv.Name)
		}
		for _, z := range cv.Zones {
			if !zones[z] {
				return fmt.Errorf("curves[%q]: zone %q matches no configured fan", cv.Name, z)
			}
		}
		if len(cv.Points) < 2 {
			return fmt.Errorf("curves[%q]: at least two points are required", cv.Name)
		}
		// Points must ascend in temperature and must not descend in demand.
		// A curve that asks for less cooling as things get hotter is always a
		// mistake, and a silent one, so it is refused here.
		sorted := append([]Point(nil), cv.Points...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].Temp < sorted[b].Temp })
		for j := range cv.Points {
			if cv.Points[j] != sorted[j] {
				return fmt.Errorf("curves[%q]: points must be listed in ascending temperature order", cv.Name)
			}
			if p := cv.Points[j]; p.PWMPct < 0 || p.PWMPct > 100 {
				return fmt.Errorf("curves[%q]: pwm_pct %.1f out of range 0..100", cv.Name, p.PWMPct)
			}
			if j > 0 {
				if cv.Points[j].Temp == cv.Points[j-1].Temp {
					return fmt.Errorf("curves[%q]: duplicate temperature %.1f", cv.Name, cv.Points[j].Temp)
				}
				if cv.Points[j].PWMPct < cv.Points[j-1].PWMPct {
					return fmt.Errorf("curves[%q]: pwm_pct decreases from %.1f to %.1f as temperature rises",
						cv.Name, cv.Points[j-1].PWMPct, cv.Points[j].PWMPct)
				}
			}
		}
	}
	if c.Safety.MaxFloorPct <= 0 || c.Safety.MaxFloorPct > 100 {
		return fmt.Errorf("safety.max_floor_pct %.1f out of range 0..100", c.Safety.MaxFloorPct)
	}
	if c.Safety.MinFloorPct < 0 || c.Safety.MinFloorPct > c.Safety.MaxFloorPct {
		return fmt.Errorf("safety.min_floor_pct %.1f must be between 0 and max_floor_pct", c.Safety.MinFloorPct)
	}
	switch c.Safety.OnSensorError.Action {
	case "hold_last", "fixed":
	default:
		return fmt.Errorf("safety.on_sensor_error.action must be \"hold_last\" or \"fixed\"")
	}
	// fixed_pct is load-bearing under both actions: "fixed" uses it
	// immediately, and "hold_last" falls back to it once max_hold expires. So
	// it is required either way rather than defaulting to zero, which would
	// mean a blind controller quietly dropping to min_floor_pct.
	if p := c.Safety.OnSensorError.FixedPct; p <= 0 || p > 100 {
		return fmt.Errorf("safety.on_sensor_error.fixed_pct must be set, in range 0..100 (got %.1f); "+
			"it is the fallback for both \"fixed\" and an expired \"hold_last\"", p)
	}
	if c.Safety.OnSensorError.Action == "hold_last" && c.Safety.OnSensorError.MaxHold.Duration <= 0 {
		return fmt.Errorf("safety.on_sensor_error.max_hold must be positive when action is \"hold_last\"")
	}
	for i, cr := range c.Safety.Critical {
		if _, ok := c.Sensors[cr.Sensor]; !ok {
			return fmt.Errorf("safety.critical[%d]: unknown sensor %q", i, cr.Sensor)
		}
	}
	return nil
}

// FanIndexesForZones returns the iLO indexes of every fan in any of the zones.
func (c *Config) FanIndexesForZones(zones []string) []int {
	want := map[string]bool{}
	for _, z := range zones {
		want[z] = true
	}
	var out []int
	for _, f := range c.Fans {
		for _, z := range f.Zones {
			if want[z] {
				out = append(out, f.Index)
				break
			}
		}
	}
	return out
}

func readable(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	return f.Close()
}
