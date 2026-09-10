package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validYAML is the smallest config that passes. %s is the key path, filled in
// per test so the file actually exists.
const validYAML = `
ilo:
  host: 192.0.2.10
  user: ilofanuser
  private_key_path: %s
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
      - { temp: 35, pwm_pct: 12 }
      - { temp: 55, pwm_pct: 70 }
safety:
  max_floor_pct: 70
  min_floor_pct: 12
  on_sensor_error:
    action: hold_last
    max_hold: 5m
    fixed_pct: 45
`

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(key, []byte("not a real key, only its readability is checked here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(body, "%s", key, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ILO.Port != 22 {
		t.Errorf("default port = %d, want 22", c.ILO.Port)
	}
	if c.Interval.Duration.Seconds() != 30 {
		t.Errorf("default interval = %v, want 30s", c.Interval)
	}
	if c.Sensors["drives"].Aggregate != "max" {
		t.Errorf("default aggregate = %q, want max", c.Sensors["drives"].Aggregate)
	}
	if c.ILO.CommandInterval.Duration.Milliseconds() != 350 {
		t.Errorf("default command_interval = %v, want 350ms", c.ILO.CommandInterval)
	}
	if c.Checksum == "" {
		t.Error("checksum was not computed, so hot reload cannot detect a change")
	}
}

// Reject-cases matter more than the accept-case here. Each of these produces a
// controller that looks like it is working: the process starts, the loop runs,
// and the fans are wrong.
func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "a typo'd key is not a silent default",
			edit: func(s string) string { return s + "\nintervals: 60s\n" },
			want: "field intervals not found",
		},
		{
			name: "a curve that cools less as it heats up",
			edit: func(s string) string {
				return strings.Replace(s, "{ temp: 55, pwm_pct: 70 }", "{ temp: 55, pwm_pct: 5 }", 1)
			},
			want: "decreases",
		},
		{
			name: "points listed out of order",
			edit: func(s string) string {
				return strings.Replace(s,
					"- { temp: 35, pwm_pct: 12 }\n      - { temp: 55, pwm_pct: 70 }",
					"- { temp: 55, pwm_pct: 70 }\n      - { temp: 35, pwm_pct: 12 }", 1)
			},
			want: "ascending",
		},
		{
			name: "a curve pointing at a sensor that does not exist",
			edit: func(s string) string { return strings.Replace(s, "sensor: drives", "sensor: nope", 1) },
			want: "unknown sensor",
		},
		{
			name: "a curve zone matching no fan",
			edit: func(s string) string {
				return strings.Replace(s, "zones: [chassis]\n    points", "zones: [rear]\n    points", 1)
			},
			want: "matches no configured fan",
		},
		{
			name: "duplicate fan indexes",
			edit: func(s string) string {
				return strings.Replace(s, `{ index: 1, label: "Fan 2"`, `{ index: 0, label: "Fan 2"`, 1)
			},
			want: "duplicate index",
		},
		{
			name: "a smart group with no devices",
			edit: func(s string) string { return strings.Replace(s, "    devices: [/dev/sda]\n", "", 1) },
			want: "requires devices",
		},
		{
			name: "an ipmi group with no match list",
			edit: func(s string) string { return strings.Replace(s, `    match: ["02-CPU 1"]`, "", 1) },
			want: "requires match",
		},
		{
			// The "inlet" group has no curve, so a validator that only walks
			// curve targets would let this through to a runtime failure.
			name: "a bad kind on a group with no curve",
			edit: func(s string) string {
				return strings.Replace(s, "curves:", "  inlet:\n    kind: typo\n    match: [\"01-Inlet Ambient\"]\ncurves:", 1)
			},
			want: `kind must be "smart" or "ipmi"`,
		},
		{
			name: "fixed_pct left unset, which hold_last also needs",
			edit: func(s string) string { return strings.Replace(s, "    fixed_pct: 45\n", "", 1) },
			want: "fixed_pct must be set",
		},
		{
			name: "min floor above max floor",
			edit: func(s string) string { return strings.Replace(s, "min_floor_pct: 12", "min_floor_pct: 90", 1) },
			want: "min_floor_pct",
		},
		{
			name: "both auth sources",
			edit: func(s string) string {
				return strings.Replace(s, "fans:", "  password_file: /dev/null\nfans:", 1)
			},
			want: "not both",
		},
		{
			name: "no auth source",
			edit: func(s string) string {
				return strings.Replace(s, "  private_key_path: %s\n", "", 1)
			},
			want: "one of private_key_path or password_file is required",
		},
		{
			name: "a private key that is not readable",
			edit: func(s string) string {
				return strings.Replace(s, "private_key_path: %s", "private_key_path: /nonexistent/id_rsa", 1)
			},
			want: "cannot read",
		},
		{
			name: "an interval the BMC shell cannot keep up with",
			edit: func(s string) string { return s + "\ninterval: 100ms\n" },
			want: "too short",
		},
		{
			name: "a negative fan index",
			edit: func(s string) string {
				return strings.Replace(s, "{ index: 0,", "{ index: -1,", 1)
			},
			want: "zero-based",
		},
		{
			name: "a single-point curve",
			edit: func(s string) string {
				return strings.Replace(s, "      - { temp: 55, pwm_pct: 70 }\n", "", 1)
			},
			want: "at least two points",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.edit(validYAML)))
			if err == nil {
				t.Fatal("config was accepted, want it rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// There must be no way to set a fan maximum. That is the one command capable
// of starving the machine of cooling, and the guarantee is worth a test
// rather than a comment: an added field would fail here.
func TestNoMaximumFanSetting(t *testing.T) {
	for _, key := range []string{"max_pct", "max_speed", "fan_max", "max_fan_pct", "maximum_pct"} {
		path := write(t, validYAML+"\n  "+key+": 100\n")
		if _, err := Load(path); err == nil {
			t.Errorf("config with a %q key was accepted", key)
		}
	}
}

func TestFanIndexesForZones(t *testing.T) {
	c, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	got := c.FanIndexesForZones([]string{"chassis"})
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("FanIndexesForZones(chassis) = %v, want [0 1]", got)
	}
	if got := c.FanIndexesForZones([]string{"nowhere"}); len(got) != 0 {
		t.Errorf("FanIndexesForZones(nowhere) = %v, want empty", got)
	}
}

func TestChecksumChangesWithContent(t *testing.T) {
	a, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(write(t, strings.Replace(validYAML, "min_floor_pct: 12", "min_floor_pct: 15", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Checksum == b.Checksum {
		t.Error("two different configs share a checksum, so a reload would miss the change")
	}
}

func TestSameILOAndSameTools(t *testing.T) {
	a, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	// Different temp dirs, so the key paths differ and the BMC settings must
	// register as changed.
	if a.SameILO(b) {
		t.Error("SameILO reported no change across different private_key_path values")
	}
	if !a.SameTools(b) {
		t.Error("SameTools reported a change where the tools sections are identical")
	}
	if !a.SameILO(a) {
		t.Error("SameILO reported a change against itself")
	}
}
