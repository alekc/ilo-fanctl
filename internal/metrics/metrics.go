// Package metrics exposes the controller's state to Prometheus.
//
// The series that matter most are the ones that catch the controller lying to
// itself: fan_speed_applied_percent against fan_speed_observed_percent, and
// readback_mismatch_total. A firmware upgrade that reverts the ilo4_unlock
// patch leaves the writes succeeding and the fans ignoring them, and the gap
// between those two gauges is the only place that shows up.
package metrics

import (
	"math"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Metrics holds every collector the controller updates.
type Metrics struct {
	reg *prometheus.Registry

	SensorCelsius    *prometheus.GaugeVec
	GroupCelsius     *prometheus.GaugeVec
	GroupBlind       *prometheus.GaugeVec
	GroupHeld        *prometheus.GaugeVec
	CurveDemandPct   *prometheus.GaugeVec
	FanAppliedPct    *prometheus.GaugeVec
	FanObservedPct   *prometheus.GaugeVec
	CycleTotal       prometheus.Counter
	CycleErrors      *prometheus.CounterVec
	WritesTotal      prometheus.Counter
	MismatchTotal    *prometheus.CounterVec
	SensorErrors     *prometheus.CounterVec
	CriticalActive   prometheus.Gauge
	LastCycleUnix    prometheus.Gauge
	CycleSeconds     prometheus.Histogram
	ILOConnected     prometheus.Gauge
	ControlEffective prometheus.Gauge
	ReloadsTotal     *prometheus.CounterVec
	ConfigLoadedUnix prometheus.Gauge
	ConfigInfo       *prometheus.GaugeVec
	BuildInfo        *prometheus.GaugeVec
	StartTimeUnix    prometheus.Gauge
	BlindGroups      prometheus.Gauge
}

// New builds the registry and every collector on it.
//
// version is a parameter rather than something a caller sets afterwards
// because it can then never be missing: the compiler asks every call site for
// it. A daemon whose /metrics does not say what it was built from is one you
// have to SSH to in order to answer "is this host still on the old binary",
// which is exactly the question worth answering from Prometheus.
func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		SensorCelsius: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_sensor_celsius",
			Help: "Temperature of one physical sensor, labelled by its source and name.",
		}, []string{"source", "name"}),
		GroupCelsius: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_sensor_group_celsius",
			Help: "Aggregated temperature for one configured sensor group.",
		}, []string{"group"}),
		GroupBlind: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_sensor_group_blind",
			Help: "1 while a sensor group cannot be read at all.",
		}, []string{"group"}),
		GroupHeld: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_sensor_group_held",
			Help: "1 while a group's demand comes from its last good value rather than a fresh read.",
		}, []string{"group"}),
		CurveDemandPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_curve_demand_percent",
			Help: "Fan floor demanded by one curve, before the per-fan maximum is taken.",
		}, []string{"curve"}),
		FanAppliedPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_fan_applied_percent",
			Help: "Minimum fan speed this controller last commanded, in percent.",
		}, []string{"fan", "label"}),
		FanObservedPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_fan_observed_percent",
			Help: "Fan speed the BMC reports, read back from the SMASH CLP.",
		}, []string{"fan", "label"}),
		CycleTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ilo_fanctl_cycles_total",
			Help: "Control cycles started.",
		}),
		CycleErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ilo_fanctl_cycle_errors_total",
			Help: "Control cycles that failed, by stage.",
		}, []string{"stage"}),
		WritesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ilo_fanctl_fan_writes_total",
			Help: "Fan floor commands sent to the BMC.",
		}),
		MismatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ilo_fanctl_readback_mismatch_total",
			Help: "Times a commanded floor was not reflected in the BMC's reported speed.",
		}, []string{"fan"}),
		SensorErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ilo_fanctl_sensor_errors_total",
			Help: "Sensor group read failures.",
		}, []string{"group"}),
		CriticalActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_critical_active",
			Help: "1 while a critical temperature threshold is exceeded.",
		}),
		LastCycleUnix: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_last_successful_cycle_timestamp_seconds",
			Help: "Unix time of the last cycle that completed without error.",
		}),
		CycleSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ilo_fanctl_cycle_duration_seconds",
			Help:    "Wall time of one control cycle.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60},
		}),
		ILOConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_ilo_connected",
			Help: "1 while an SSH session to the BMC is established.",
		}),
		ControlEffective: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_control_effective",
			Help: "1 when a commanded floor was confirmed by read-back, 0 when the firmware is ignoring it.",
		}),
		ReloadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ilo_fanctl_config_reloads_total",
			Help: "Config reload attempts, by result. A failure means the previous config is still running.",
		}, []string{"result"}),
		ConfigLoadedUnix: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_config_loaded_timestamp_seconds",
			Help: "Unix time the running config was loaded.",
		}),
		ConfigInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_config_info",
			Help: "Always 1, labelled with the SHA-256 of the config the daemon is actually running.",
		}, []string{"checksum"}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ilo_fanctl_build_info",
			Help: "Always 1, labelled with the version the daemon was built from.",
		}, []string{"version"}),
		StartTimeUnix: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_start_time_seconds",
			Help: "Unix time the process started.",
		}),
		BlindGroups: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ilo_fanctl_blind_sensor_groups",
			Help: "Sensor groups currently unreadable, so running on a held or fallback value.",
		}),
	}
	reg.MustRegister(
		m.SensorCelsius, m.GroupCelsius, m.GroupBlind, m.GroupHeld, m.CurveDemandPct,
		m.FanAppliedPct, m.FanObservedPct,
		m.CycleTotal, m.CycleErrors, m.WritesTotal, m.MismatchTotal, m.SensorErrors,
		m.CriticalActive, m.LastCycleUnix, m.CycleSeconds,
		m.ILOConnected, m.ControlEffective,
		m.ReloadsTotal, m.ConfigLoadedUnix, m.ConfigInfo, m.BuildInfo,
		m.StartTimeUnix, m.BlindGroups,
	)
	m.BuildInfo.WithLabelValues(version).Set(1)
	m.StartTimeUnix.SetToCurrentTime()
	// NaN, not 0, until a floor has had a full interval to take effect. A
	// plain gauge is always present, so 0 here would assert "the BMC is
	// ignoring the floors" from the moment the process starts, which is the
	// alarm this metric exists to raise. NaN compares false against both 0
	// and 1, so neither alert fires while the answer is genuinely unknown.
	m.ControlEffective.Set(math.NaN())
	return m
}

// SetConfigInfo records which config is running, replacing the previous
// checksum rather than leaving both series exposed.
func (m *Metrics) SetConfigInfo(checksum string) {
	m.ConfigInfo.Reset()
	m.ConfigInfo.WithLabelValues(checksum).Set(1)
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Gather returns the current values, the same set /metrics would serve. It
// lets a caller assert on what the loop recorded without an HTTP round trip.
func (m *Metrics) Gather() ([]*dto.MetricFamily, error) { return m.reg.Gather() }
