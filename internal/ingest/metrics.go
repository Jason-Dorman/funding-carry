package ingest

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Namespace prefixes every series this package exports. It is also the
// namespace cmd/ingest gives the shared writer metrics, so the four
// ingest_rows_* / ingest_write_* series in API spec section 6 line up with the
// three below under one prefix.
const Namespace = "ingest"

// Metrics is the ingest binary's slice of the catalogue (API spec section 6).
// Collectors register on the binary's own registry, never the client_golang
// default, so what this binary exports is a property of the wiring rather than
// of which packages happen to be imported.
type Metrics struct {
	reconnects *prometheus.CounterVec
	gaps       *prometheus.CounterVec
	lastSeen   *prometheus.GaugeVec
}

// NewMetrics registers the per-stream collectors.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		reconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "ws_reconnects_total",
			Help:      "WebSocket reconnects, by stream.",
		}, []string{"stream"}),
		gaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "ws_gaps_total",
			Help:      "Detected data gaps, by stream: sequence discontinuity, silence, or a message that could not be used.",
		}, []string{"stream"}),
		lastSeen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "last_seen_timestamp_seconds",
			Help:      "Venue timestamp of the last data message on each stream. Heartbeats do not advance it.",
		}, []string{"stream"}),
	}
	reg.MustRegister(m.reconnects, m.gaps, m.lastSeen)
	return m
}

// Stream returns the recorder for one stream, with its label bound once. The
// gauge is initialized to zero so a stream that has never received anything is
// visibly stale from the first scrape rather than absent from the output —
// FeedStale cannot fire on a series that does not exist.
func (m *Metrics) Stream(name string) *StreamMetrics {
	r := &StreamMetrics{
		reconnects: m.reconnects.WithLabelValues(name),
		gaps:       m.gaps.WithLabelValues(name),
		lastSeen:   m.lastSeen.WithLabelValues(name),
	}
	r.reconnects.Add(0)
	r.gaps.Add(0)
	r.lastSeen.Set(0)
	return r
}

// StreamMetrics is one stream's three series.
type StreamMetrics struct {
	reconnects prometheus.Counter
	gaps       prometheus.Counter
	lastSeen   prometheus.Gauge
}

func (r *StreamMetrics) reconnect() { r.reconnects.Inc() }

func (r *StreamMetrics) gap() { r.gaps.Inc() }

func (r *StreamMetrics) seen(at time.Time) {
	// The only float in the ingest path, and it is a timestamp rather than
	// money: Prometheus gauges are float64 by definition, and a unix second
	// count is exactly representable for the next few hundred million years.
	r.lastSeen.Set(float64(at.UnixNano()) / float64(time.Second))
}
