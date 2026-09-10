package sensors

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/alekc/ilo-fanctl/internal/config"
)

// maxParallelDrives bounds the smartctl fan-out. A JBOD shelf can be dozens of
// devices, and spawning one process per drive at once is a burst the host does
// not need to absorb every interval; a handful in flight already hides almost
// all of the latency.
const maxParallelDrives = 8

// SMART reads per-drive temperature directly from the drives.
//
// This is the source of truth the BMC's drive sensor is not. See the package
// comment for the measured discrepancy.
type SMART struct {
	Path string
}

type smartAttrs struct {
	Temperature struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`
}

type smartScan struct {
	Devices []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"devices"`
}

// Collect reads every configured device, or discovers them when the device
// list is the single entry "auto".
func (s *SMART) Collect(ctx context.Context, cfg config.Sensor) ([]Reading, error) {
	bin := s.Path
	if bin == "" {
		bin = "smartctl"
	}
	devices := cfg.Devices
	if len(devices) == 1 && devices[0] == "auto" {
		var err error
		devices, err = s.scan(ctx, bin)
		if err != nil {
			return nil, err
		}
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no drives to read")
	}
	// One smartctl per drive, run together rather than one after another.
	// These are independent SCSI passthroughs and the drives answer in
	// parallel, so six of them cost one drive's latency instead of six. On the
	// DL380 this was the largest single component of the cycle.
	//
	// Results are written into fixed slots and read back in configuration
	// order afterwards, because the readings end up in a table a human reads
	// and completion order is not stable between cycles.
	got := make([]Reading, len(devices))
	errs := make([]error, len(devices))
	sem := make(chan struct{}, maxParallelDrives)
	var wg sync.WaitGroup
	for i, dev := range devices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := s.readOne(ctx, bin, dev)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %v", dev, err)
				return
			}
			got[i] = Reading{Source: "smart", Name: dev, Celsius: c}
		}()
	}
	wg.Wait()

	var readings []Reading
	var failures []string
	for i := range devices {
		if errs[i] != nil {
			failures = append(failures, errs[i].Error())
			continue
		}
		readings = append(readings, got[i])
	}
	if len(failures) > 0 {
		// Partial success is still useful: report what was read and name what
		// was not, so the caller can decide whether the gap matters.
		return readings, fmt.Errorf("smart read failures: %s", strings.Join(failures, "; "))
	}
	return readings, nil
}

func (s *SMART) scan(ctx context.Context, bin string) ([]string, error) {
	out, err := exec.CommandContext(ctx, bin, "--scan", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("%s --scan: %w", bin, err)
	}
	var sc smartScan
	if err := json.Unmarshal(out, &sc); err != nil {
		return nil, fmt.Errorf("parse smartctl --scan output: %w", err)
	}
	var devs []string
	for _, d := range sc.Devices {
		devs = append(devs, d.Name)
	}
	return devs, nil
}

func (s *SMART) readOne(ctx context.Context, bin, dev string) (float64, error) {
	// smartctl exits non-zero for benign reasons (bit 2 means a device error
	// log entry exists, for instance), so the exit code is not treated as
	// fatal; the JSON is parsed regardless and judged on its contents.
	out, _ := exec.CommandContext(ctx, bin, "-A", "-j", dev).Output()
	if len(out) == 0 {
		return 0, fmt.Errorf("no output")
	}
	var a smartAttrs
	if err := json.Unmarshal(out, &a); err != nil {
		return 0, fmt.Errorf("parse json: %w", err)
	}
	if a.Temperature.Current == nil {
		return 0, fmt.Errorf("no temperature reported")
	}
	return *a.Temperature.Current, nil
}
