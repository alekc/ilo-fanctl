package ilo

import "testing"

// clpFanOutput is a real "show -a /system1/fan1" response captured from an
// HPE ProLiant DL380 Gen9 running iLO 4 firmware 2.77, patched with
// ilo4_unlock. It is verbatim, including the echoed command, the status
// block, the blank-line run, and the second /system1/fan1 header that follows
// the first. Reconstructing this by hand is exactly how a parser ends up
// matching a shape the device never emits.
const clpFanOutput = "show -a /system1/fan1\n" +
	"\n" +
	"status=0\n" +
	"status_tag=COMMAND COMPLETED\n" +
	"Wed Sep  9 12:42:22 2026\n" +
	"\n\n\n\n" +
	"/system1/fan1\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 1\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=9 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"/system1/fan2\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 2\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=11 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"/system1/fan3\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 3\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=11 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"/system1/fan4\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 4\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=17 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"/system1/fan5\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 5\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=18 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"/system1/fan6\n" +
	"  Targets\n" +
	"  Properties\n" +
	"    DeviceID=Fan 6\n" +
	"    ElementName=System\n" +
	"    OperationalStatus=Ok\n" +
	"    VariableSpeed=Yes\n" +
	"    DesiredSpeed=17 percent\n" +
	"    HealthState=Ok\n" +
	"  Verbs\n" +
	"    cd version exit show\n" +
	"\n" +
	"</>hpiLO-> "

func TestParseFanSpeeds(t *testing.T) {
	got, err := parseFanSpeeds(clpFanOutput)
	if err != nil {
		t.Fatalf("parseFanSpeeds: %v", err)
	}
	// Keys are zero-based "fan p" indexes, so /system1/fan1 is key 0.
	want := map[int]float64{0: 9, 1: 11, 2: 11, 3: 17, 4: 18, 5: 17}
	if len(got) != len(want) {
		t.Fatalf("got %d fans, want %d: %v", len(got), len(want), got)
	}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("fan index %d: got %v percent, want %v", idx, got[idx], w)
		}
	}
}

// A response the device never sent must not be reported as "all fans at zero".
// Silence and a real reading have to be distinguishable, because the whole
// read-back exists to detect the firmware ignoring us.
func TestParseFanSpeedsRejectsEmpty(t *testing.T) {
	for _, in := range []string{
		"",
		"</>hpiLO-> ",
		"show -a /system1/fan1\n\nstatus=2\nstatus_tag=COMMAND PROCESSING FAILED\n</>hpiLO-> ",
	} {
		if got, err := parseFanSpeeds(in); err == nil {
			t.Errorf("parseFanSpeeds(%q) = %v, want an error", in, got)
		}
	}
}

// DeviceID lines carry "Fan 1" as text and must not be mistaken for a target
// header, and a target with no DesiredSpeed must not inherit the next one's.
func TestParseFanSpeedsPartialBlock(t *testing.T) {
	in := "/system1/fan1\n" +
		"    DeviceID=Fan 1\n" +
		"    OperationalStatus=Unknown\n" +
		"/system1/fan2\n" +
		"    DeviceID=Fan 2\n" +
		"    DesiredSpeed=42 percent\n" +
		"</>hpiLO-> "
	got, err := parseFanSpeeds(in)
	if err != nil {
		t.Fatalf("parseFanSpeeds: %v", err)
	}
	if _, ok := got[0]; ok {
		t.Errorf("fan index 0 reported %v despite having no DesiredSpeed", got[0])
	}
	if got[1] != 42 {
		t.Errorf("fan index 1: got %v, want 42", got[1])
	}
}
