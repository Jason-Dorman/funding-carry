package exec

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
)

// Metrics is the execution slice of carry's catalogue (API spec section 6).
type Metrics struct {
	roundtrip *prometheus.HistogramVec
	reports   *prometheus.CounterVec
}

// NewMetrics registers the collectors on the binary's own registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		roundtrip: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: carry.Namespace,
			Name:      "order_roundtrip_seconds",
			Help:      "Submit to terminal ExecReport, per venue.",
			// From the simulator's twenty-millisecond latency up to a resting
			// order that waited most of a minute for the market to come to it.
			// Anything beyond the last bucket is the +Inf bucket, which is the
			// right place for an order that sat for an hour: the histogram is
			// for the round trip, and an order that rests is not a round trip
			// the venue was slow on.
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"venue"}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: carry.Namespace,
			Name:      "exec_reports_total",
			Help: "ExecReports by what the state machine did with them. " +
				"duplicate rising after a reconnect is the resend recovery doing its job; " +
				"stale, late and invalid are a venue whose reports disagree with themselves.",
		}, []string{"venue", "outcome"}),
	}
	reg.MustRegister(m.roundtrip, m.reports)
	return m
}

// venue binds the label once, so a Tracker cannot spell it two ways.
func (m *Metrics) venue(name string) *venueMetrics {
	v := &venueMetrics{roundtrip: m.roundtrip.WithLabelValues(name), reports: m.reports, name: name}
	// Every outcome exists at zero from the start: an "invalid" that has never
	// fired is a visible zero, and a panel over it does not have to know whether
	// the series is absent or the count is nothing.
	for _, o := range allOutcomes {
		m.reports.WithLabelValues(name, string(o)).Add(0)
	}
	return v
}

type venueMetrics struct {
	name      string
	roundtrip prometheus.Observer
	reports   *prometheus.CounterVec
}

func (v *venueMetrics) observeRoundtrip(d time.Duration) { v.roundtrip.Observe(d.Seconds()) }

func (v *venueMetrics) report(o Outcome) { v.reports.WithLabelValues(v.name, string(o)).Inc() }
