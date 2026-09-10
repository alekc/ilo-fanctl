// Package curve evaluates piecewise linear temperature to fan-floor mappings.
package curve

import "github.com/alekc/ilo-fanctl/internal/config"

// Eval maps a temperature onto a demanded fan floor in percent.
//
// Points are assumed sorted ascending by temperature and non-decreasing in
// demand; config.Validate guarantees both, so this function does not re-check.
// Below the first point the first point's demand applies, above the last the
// last point's does. Clamping rather than extrapolating is deliberate: linear
// extrapolation past the hot end of a curve produces demands above 100 percent
// from a single bad sensor read.
func Eval(points []config.Point, temp float64) float64 {
	if len(points) == 0 {
		return 0
	}
	if temp <= points[0].Temp {
		return points[0].PWMPct
	}
	last := points[len(points)-1]
	if temp >= last.Temp {
		return last.PWMPct
	}
	for i := 1; i < len(points); i++ {
		hi := points[i]
		if temp > hi.Temp {
			continue
		}
		lo := points[i-1]
		span := hi.Temp - lo.Temp
		if span <= 0 {
			return hi.PWMPct
		}
		frac := (temp - lo.Temp) / span
		return lo.PWMPct + frac*(hi.PWMPct-lo.PWMPct)
	}
	return last.PWMPct
}

// PctToRaw converts a percentage to iLO's 0..255 PWM scale.
//
// The scale is linear and exact: 51 yields precisely 20 percent, measured on
// both Gen8 and Gen9. Rounding is to nearest.
func PctToRaw(pct float64) int {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return int(pct/100*255 + 0.5)
}

// RawToPct is the inverse of PctToRaw, used when reporting what was commanded.
func RawToPct(raw int) float64 {
	return float64(raw) / 255 * 100
}
