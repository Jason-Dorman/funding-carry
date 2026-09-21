package exec

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
)

// Metrics is the execution slice of carry's catalogue (API spec section 6).
type Metrics struct {
	roundtrip   *prometheus.HistogramVec
	ack         *prometheus.HistogramVec
	ackTimeouts *prometheus.CounterVec
	reports     *prometheus.CounterVec
	open        *prometheus.GaugeVec
	overdue     *prometheus.GaugeVec
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
		// The acknowledgement latency is the distribution ORDER_TIMEOUT is
		// supposed to be set from, and the reason the two overdue series
		// could not do it: a gauge says that the deadline was missed, never
		// by how much (CHANGE-002).
		ack: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: carry.Namespace,
			Name:      "order_ack_seconds",
			Help: "Time to the venue's FIRST RESPONSE, in seconds: a NEW, a fill or a rejection, " +
				"whichever answered the order first. Measured from the Ack returned by Submit to " +
				"the moment the report reached this process, both on our clock, because that is " +
				"the wait ORDER_TIMEOUT races. Observed only for orders this process submitted " +
				"and only when a response arrived; a response that missed the deadline is observed " +
				"here at its true latency AND counted in carry_order_ack_timeouts_total, and one " +
				"that never arrived is only counted there — a synthetic observation would corrupt " +
				"the one distribution ORDER_TIMEOUT is meant to be set from.",
			// Fine where acknowledgements normally land (the simulator answers
			// in tens of milliseconds, a live venue sub-second), coarse out to
			// and past ORDER_TIMEOUT so the pathological tail stays visible.
			// An observation at or beyond 30 s is an acknowledgement that came
			// back after the deadline had already passed.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
		}, []string{"venue", "leg"}),
		ackTimeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: carry.Namespace,
			Name:      "order_ack_timeouts_total",
			Help: "Orders that passed ORDER_TIMEOUT with no response, counted once each. " +
				"This OVERLAPS carry_order_ack_seconds rather than complementing it: an order " +
				"answered late is counted here and observed there, one never answered is only " +
				"here. Do not divide this into that histogram's count - it double-counts the " +
				"late answers; the timeout rate is this against orders submitted.",
		}, []string{"venue", "leg"}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: carry.Namespace,
			Name:      "exec_reports_total",
			Help: "ExecReports by what the state machine did with them. " +
				"duplicate rising after a reconnect is the resend recovery doing its job; " +
				"stale, late and invalid are a venue whose reports disagree with themselves.",
		}, []string{"venue", "outcome"}),
		open: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: carry.Namespace,
			Name:      "orders_open",
			Help:      "Orders handed to the venue and not yet ended.",
		}, []string{"venue"}),
		overdue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: carry.Namespace,
			Name:      "orders_overdue",
			Help: "Orders the venue has not acknowledged within ORDER_TIMEOUT. " +
				"Rising and staying is a venue that has stopped answering; it shows that the " +
				"timeout is being missed but not by how much — carry_order_ack_seconds is the " +
				"series the value is set from.",
		}, []string{"venue"}),
	}
	reg.MustRegister(m.roundtrip, m.ack, m.ackTimeouts, m.reports, m.open, m.overdue)
	return m
}

// venue binds the label once, so a Tracker cannot spell it two ways.
func (m *Metrics) venue(name string) *venueMetrics {
	v := &venueMetrics{
		roundtrip:   m.roundtrip.WithLabelValues(name),
		ack:         m.ack,
		ackTimeouts: m.ackTimeouts,
		reports:     m.reports,
		open:        m.open.WithLabelValues(name),
		overdue:     m.overdue.WithLabelValues(name),
		name:        name,
	}
	// The two leg-labelled series are deliberately NOT created at zero the way
	// the outcomes below are. A visible zero is worth having for something
	// that can happen and has not; leg="spot" is something that cannot happen
	// yet — no spot order reaches this tracker in v1 — and a series sitting at
	// zero would advertise a path the system does not have. They appear with
	// the first order of a leg.
	// The two gauges exist at zero before any order, so "no orders open" is a
	// visible zero rather than an absent series.
	v.open.Set(0)
	v.overdue.Set(0)
	// Every outcome exists at zero from the start: an "invalid" that has never
	// fired is a visible zero, and a panel over it does not have to know whether
	// the series is absent or the count is nothing.
	for _, o := range allOutcomes {
		m.reports.WithLabelValues(name, string(o)).Add(0)
	}
	return v
}

type venueMetrics struct {
	name        string
	roundtrip   prometheus.Observer
	ack         *prometheus.HistogramVec
	ackTimeouts *prometheus.CounterVec
	reports     *prometheus.CounterVec
	open        prometheus.Gauge
	overdue     prometheus.Gauge
}

func (v *venueMetrics) observeRoundtrip(d time.Duration) { v.roundtrip.Observe(d.Seconds()) }

// observeAck records how long the venue took to answer an order. The leg is
// carried per call rather than bound with the venue, because one tracker
// follows one venue and both legs.
func (v *venueMetrics) observeAck(leg carry.Leg, d time.Duration) {
	v.ack.WithLabelValues(v.name, string(leg)).Observe(d.Seconds())
}

// ackTimedOut counts an order that passed its deadline unacknowledged, once.
func (v *venueMetrics) ackTimedOut(leg carry.Leg) {
	v.ackTimeouts.WithLabelValues(v.name, string(leg)).Inc()
}
func (v *venueMetrics) observeOpen(n int)    { v.open.Set(float64(n)) }
func (v *venueMetrics) observeOverdue(n int) { v.overdue.Set(float64(n)) }

func (v *venueMetrics) report(o Outcome) { v.reports.WithLabelValues(v.name, string(o)).Inc() }
