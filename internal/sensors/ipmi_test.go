package sensors

import "testing"

func TestParseSDRLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantOK  bool
		wantNam string
		wantVal float64
	}{
		{
			name:    "a normal reading",
			line:    "01-Inlet Ambient | 05h | ok  |  64.1 | 19 degrees C",
			wantOK:  true,
			wantNam: "01-Inlet Ambient",
			wantVal: 19,
		},
		{
			name:    "a CPU sensor",
			line:    "02-CPU 1          | 06h | ok  |  3.1 | 40 degrees C",
			wantOK:  true,
			wantNam: "02-CPU 1",
			wantVal: 40,
		},
		{
			// This is the case that matters. A Disabled sensor read as 0 C
			// would drag a "mean" aggregate down and, worse, would look like
			// a perfectly cold component.
			name:   "a disabled sensor is not a zero reading",
			line:   "04-P1 DIMM 1-6    | 08h | ns  |  8.1 | Disabled",
			wantOK: false,
		},
		{
			name:   "a no-reading sensor",
			line:   "09-Exp Bay Drive  | 0Dh | ns  | 11.1 | No Reading",
			wantOK: false,
		},
		{name: "a header line", line: "", wantOK: false},
		{name: "too few fields", line: "01-Inlet Ambient | 05h | ok", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, val, ok := parseSDRLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if name != tc.wantNam {
				t.Errorf("name = %q, want %q", name, tc.wantNam)
			}
			if val != tc.wantVal {
				t.Errorf("celsius = %v, want %v", val, tc.wantVal)
			}
		})
	}
}

func TestAggregate(t *testing.T) {
	rs := []Reading{
		{Celsius: 40},
		{Celsius: 56},
		{Celsius: 44},
	}
	if got := aggregate(rs, "max"); got != 56 {
		t.Errorf("max = %v, want 56", got)
	}
	if got := aggregate(rs, "mean"); got != 140.0/3 {
		t.Errorf("mean = %v, want %v", got, 140.0/3)
	}
	// An unknown aggregate must fall back to max, never to zero: the safe
	// direction for a cooling decision is the hottest reading.
	if got := aggregate(rs, "nonsense"); got != 56 {
		t.Errorf("unknown aggregate = %v, want the max 56", got)
	}
}
