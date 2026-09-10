package metrics

import "testing"

// The name and the label here are a published contract, not an internal
// detail: a dashboard panel and a "which hosts are still on the old build"
// query both bind to them by string. Renaming either is a breaking change for
// anything already scraping the daemon, so it should break a test rather than
// a dashboard nobody opens until an incident.
func TestBuildInfoCarriesTheVersionItWasBuiltWith(t *testing.T) {
	m := New("v1.2.3")

	families, err := m.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() != "ilo_fanctl_build_info" {
			continue
		}
		if got := len(f.GetMetric()); got != 1 {
			t.Fatalf("ilo_fanctl_build_info: got %d series, want exactly 1", got)
		}
		series := f.GetMetric()[0]

		labels := series.GetLabel()
		if len(labels) != 1 {
			t.Fatalf("labels: got %d, want exactly 1 (version)", len(labels))
		}
		if got := labels[0].GetName(); got != "version" {
			t.Errorf("label name: got %q, want %q", got, "version")
		}
		if got := labels[0].GetValue(); got != "v1.2.3" {
			t.Errorf("label value: got %q, want the version New was given", got)
		}

		// Always 1 is what makes the info-metric idiom work: the value
		// carries nothing, so a query joins on the label and the series is
		// guaranteed present for every running daemon.
		if got := series.GetGauge().GetValue(); got != 1 {
			t.Errorf("value: got %v, want 1", got)
		}
		return
	}
	t.Fatal("ilo_fanctl_build_info is not exposed at all")
}
