package sensors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// rendezvous is a collector that will not answer until every other one has
// been asked. Run the groups one after another and the first call never
// returns, which is the failure this shape is here to make visible.
type rendezvous struct {
	arrive chan struct{}
	open   chan struct{}
	temp   float64
}

func (r *rendezvous) Collect(_ context.Context, s config.Sensor) ([]Reading, error) {
	r.arrive <- struct{}{}
	<-r.open
	return []Reading{{Source: s.Kind, Name: s.Match[0], Celsius: r.temp}}, nil
}

func TestGroupsAreCollectedTogetherRatherThanOneAfterAnother(t *testing.T) {
	const groups = 3
	r := &rendezvous{arrive: make(chan struct{}, groups), open: make(chan struct{}), temp: 41}
	reg := NewRegistryWith(map[string]Collector{"ipmi": r})
	cfg := &config.Config{Sensors: map[string]config.Sensor{
		"cpu":    {Kind: "ipmi", Aggregate: "max", Match: []string{"02-CPU 1"}},
		"hba":    {Kind: "ipmi", Aggregate: "max", Match: []string{"27-HD Controller"}},
		"inlet":  {Kind: "ipmi", Aggregate: "max", Match: []string{"01-Inlet Ambient"}},
		"absent": {Kind: "nosuch"},
	}}

	got := make(chan map[string]Group, 1)
	go func() { got <- reg.CollectAll(context.Background(), cfg) }()

	// Every group has to be in flight at the same time before any of them is
	// allowed to answer.
	for range groups {
		select {
		case <-r.arrive:
		case <-time.After(5 * time.Second):
			t.Error("the groups are collected one after another, not together")
			close(r.open)
			return
		}
	}
	close(r.open)

	out := <-got
	if len(out) != 4 {
		t.Fatalf("collected %d groups, want all 4", len(out))
	}
	if out["cpu"].Value != 41 {
		t.Errorf("cpu = %v, want the collected 41", out["cpu"].Value)
	}
	// A kind with no collector is reported in its own group rather than
	// aborting the sweep, same as before the fan-out.
	if out["absent"].Err == nil {
		t.Error("a group with no collector came back clean")
	}
}

// The six smartctl invocations of a drive group run together. Serially they
// were the largest single component of a cycle on the DL380, and the drives
// answer independently.
//
// The stub blocks until every drive has been asked for, so a serial
// implementation never gets past the first one and comes back with nothing.
func TestSmartReadsEveryDriveAtOnce(t *testing.T) {
	devices := []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"}
	dir := t.TempDir()
	bin := filepath.Join(dir, "smartctl")
	// The bounded wait matters: unbounded, a regression hangs the suite
	// instead of failing it.
	script := fmt.Sprintf(`#!/bin/sh
touch "%[1]s/$(basename "$3").asked"
n=0
while [ "$(ls "%[1]s" | grep -c '\.asked$')" -lt %[2]d ]; do
  n=$((n+1))
  if [ "$n" -gt 300 ]; then exit 1; fi
  sleep 0.01
done
echo '{"temperature":{"current":42}}'
`, dir, len(devices))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &SMART{Path: bin}
	got, err := s.Collect(context.Background(), config.Sensor{Kind: "smart", Devices: devices})
	if err != nil {
		t.Fatalf("the drives were read one after another: %v", err)
	}
	if len(got) != len(devices) {
		t.Fatalf("read %d drives, want %d", len(got), len(devices))
	}
	// Configuration order, not completion order: these end up in a table a
	// human reads, and the order the drives happen to answer in is not stable
	// between cycles.
	for i, r := range got {
		if r.Name != devices[i] {
			t.Errorf("reading %d is %s, want %s in configuration order", i, r.Name, devices[i])
		}
	}
}
