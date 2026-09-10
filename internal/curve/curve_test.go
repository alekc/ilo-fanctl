package curve

import (
	"math"
	"testing"

	"github.com/alekc/ilo-fanctl/internal/config"
)

func points() []config.Point {
	return []config.Point{
		{Temp: 35, PWMPct: 12},
		{Temp: 45, PWMPct: 30},
		{Temp: 55, PWMPct: 70},
	}
}

func TestEval(t *testing.T) {
	cases := []struct {
		name string
		temp float64
		want float64
	}{
		{"below the first point clamps", 10, 12},
		{"on the first point", 35, 12},
		{"midway up the first segment", 40, 21},
		{"on an interior point", 45, 30},
		{"midway up the second segment", 50, 50},
		{"on the last point", 55, 70},
		// Extrapolating instead of clamping is how one bad sensor read turns
		// into a demand above 100 percent, so the hot end must clamp too.
		{"above the last point clamps", 200, 70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eval(points(), tc.temp); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Eval(%v) = %v, want %v", tc.temp, got, tc.want)
			}
		})
	}
}

func TestEvalNeverDecreasesWithTemperature(t *testing.T) {
	prev := -1.0
	for temp := 0.0; temp <= 120; temp += 0.25 {
		got := Eval(points(), temp)
		if got < prev {
			t.Fatalf("demand fell from %v to %v at %v C", prev, got, temp)
		}
		prev = got
	}
}

// The 0..255 scale is linear and exact. 51 raw reading back as precisely 20
// percent is the measurement that proved the patched firmware was honouring
// the command at all, so it is pinned here.
func TestPctToRaw(t *testing.T) {
	cases := []struct {
		pct float64
		raw int
	}{
		{0, 0},
		{20, 51},
		{50, 128},
		{100, 255},
		{-5, 0},    // clamped, not negative
		{140, 255}, // clamped, not out of range
	}
	for _, tc := range cases {
		if got := PctToRaw(tc.pct); got != tc.raw {
			t.Errorf("PctToRaw(%v) = %d, want %d", tc.pct, got, tc.raw)
		}
	}
}

func TestRawToPctRoundTrip(t *testing.T) {
	for raw := 0; raw <= 255; raw++ {
		if got := PctToRaw(RawToPct(raw)); got != raw {
			t.Errorf("PctToRaw(RawToPct(%d)) = %d", raw, got)
		}
	}
}
