package sensors

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// IPMI reads the BMC's own temperature sensors over the local KCS interface.
//
// One invocation returns every sensor, so the whole chassis costs a single
// exec regardless of how many sensor groups reference it.
type IPMI struct {
	Path string
}

// Collect returns the readings whose names appear in s.Match.
//
// Sensors the BMC reports as absent or disabled are skipped rather than read
// as zero. A disabled sensor returning 0 C would drag a "mean" aggregate down
// and, worse, would look like a perfectly cold component.
func (i *IPMI) Collect(ctx context.Context, s config.Sensor) ([]Reading, error) {
	if len(s.Match) == 0 {
		return nil, fmt.Errorf("ipmi sensor group has no match list")
	}
	bin := i.Path
	if bin == "" {
		bin = "ipmitool"
	}
	out, err := exec.CommandContext(ctx, bin, "sdr", "type", "Temperature").Output()
	if err != nil {
		return nil, fmt.Errorf("%s sdr type Temperature: %w", bin, err)
	}
	want := make(map[string]bool, len(s.Match))
	for _, m := range s.Match {
		want[m] = true
	}
	found := map[string]bool{}
	var readings []Reading
	for _, line := range strings.Split(string(out), "\n") {
		name, celsius, ok := parseSDRLine(line)
		if !ok || !want[name] {
			continue
		}
		found[name] = true
		readings = append(readings, Reading{Source: "ipmi", Name: name, Celsius: celsius})
	}
	// A sensor named in the config that the BMC does not report is a config
	// error worth surfacing, not a reading to quietly omit.
	var missing []string
	for _, m := range s.Match {
		if !found[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return readings, fmt.Errorf("ipmi sensors not present or disabled: %s", strings.Join(missing, ", "))
	}
	return readings, nil
}

// parseSDRLine handles the pipe-delimited form ipmitool emits, for example:
//
//	01-Inlet Ambient | 03h | ok  | 64.1 | 19 degrees C
//	04-P1 DIMM 1-6   | 06h | ns  | 32.1 | Disabled
//
// The second example returns ok=false: there is no temperature to read.
func parseSDRLine(line string) (name string, celsius float64, ok bool) {
	parts := strings.Split(line, "|")
	if len(parts) < 5 {
		return "", 0, false
	}
	name = strings.TrimSpace(parts[0])
	status := strings.TrimSpace(parts[2])
	value := strings.TrimSpace(parts[4])
	if name == "" || status != "ok" {
		return "", 0, false
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}
