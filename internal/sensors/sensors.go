// Package sensors collects temperatures from the host, not from the BMC's
// aggregate views.
//
// The distinction matters on the hardware this was written for. HPE iLO 4
// exposes an "08-HD Max" sensor that claims to be the maximum drive
// temperature and is not: measured against per-drive SMART on a DL380 Gen9 in
// HBA mode it read 44 to 46 C while the hottest SAS drive was 56 C, tracking
// the coolest drive instead. Its only threshold is Upper Critical 60 C and the
// thresholds are not settable, so the BMC would first react at roughly 70 C of
// real drive temperature. Everything else the BMC reports is accurate, so this
// package reads drives from SMART and everything else from IPMI.
package sensors

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// Reading is one temperature from one physical thing.
type Reading struct {
	Source  string // "smart" or "ipmi"
	Name    string // device path or BMC sensor name
	Celsius float64
}

// Group is the evaluated result for one configured sensor group.
type Group struct {
	Name     string
	Readings []Reading
	Value    float64 // aggregated per the group's Aggregate setting
	Err      error
}

// Collector reads one kind of source.
type Collector interface {
	Collect(ctx context.Context, s config.Sensor) ([]Reading, error)
}

// Registry dispatches configured sensor groups to their collectors.
type Registry struct {
	collectors map[string]Collector
}

// NewRegistry wires the built-in collectors.
func NewRegistry(ipmitoolPath, smartctlPath string) *Registry {
	return NewRegistryWith(map[string]Collector{
		"ipmi":  &IPMI{Path: ipmitoolPath},
		"smart": &SMART{Path: smartctlPath},
	})
}

// NewRegistryWith builds a registry over an explicit collector set, keyed by
// the "kind" a sensor group names. NewRegistry covers the two real sources;
// this exists so the control loop can be exercised against readings that are
// chosen rather than measured.
func NewRegistryWith(collectors map[string]Collector) *Registry {
	return &Registry{collectors: collectors}
}

// CollectAll evaluates every configured group and waits for all of them.
//
// A failing group is reported in its own Group.Err rather than aborting the
// sweep, so one unreadable disk does not blind the controller to the CPU.
func (r *Registry) CollectAll(ctx context.Context, cfg *config.Config) map[string]Group {
	out := make(map[string]Group, len(cfg.Sensors))
	for g := range r.Stream(ctx, cfg) {
		out[g.Name] = g
	}
	return out
}

// Stream evaluates every configured group concurrently and sends each one down
// the channel as it finishes, closing the channel when the last has landed.
//
// Two things come out of this. The groups no longer cost the sum of their
// times: an ipmitool call does not wait behind six smartctl calls, because
// neither reads the other's result. And a caller that draws progress can show
// a group the moment it is read, rather than showing nothing until the slowest
// one returns.
//
// Ordering is completion order and deliberately not configuration order. A
// caller that presents these to a human sorts them itself; sorting here would
// mean waiting for all of them, which is the thing this exists to avoid.
func (r *Registry) Stream(ctx context.Context, cfg *config.Config) <-chan Group {
	// Buffered for the whole set, so a caller that abandons the range cannot
	// wedge a collecting goroutine on a send that nobody will receive.
	out := make(chan Group, len(cfg.Sensors))
	var wg sync.WaitGroup
	for name, s := range cfg.Sensors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out <- r.collect(ctx, name, s)
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (r *Registry) collect(ctx context.Context, name string, s config.Sensor) Group {
	g := Group{Name: name}
	c, ok := r.collectors[s.Kind]
	if !ok {
		g.Err = fmt.Errorf("no collector for kind %q", s.Kind)
		return g
	}
	readings, err := c.Collect(ctx, s)
	if err != nil {
		g.Err = err
		return g
	}
	if len(readings) == 0 {
		g.Err = fmt.Errorf("sensor group %q matched nothing", name)
		return g
	}
	g.Readings = readings
	g.Value = aggregate(readings, s.Aggregate)
	return g
}

func aggregate(rs []Reading, how string) float64 {
	switch how {
	case "mean":
		var sum float64
		for _, r := range rs {
			sum += r.Celsius
		}
		return sum / float64(len(rs))
	default: // "max"
		v := math.Inf(-1)
		for _, r := range rs {
			if r.Celsius > v {
				v = r.Celsius
			}
		}
		return v
	}
}
