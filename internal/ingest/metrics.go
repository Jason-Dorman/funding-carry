package ingest

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/db"
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
	base       *BaseMetrics
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
	m.base = newBaseMetrics(reg)
	return m
}

// Base returns the Base poller's collectors.
func (m *Metrics) Base() *BaseMetrics { return m.base }

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

// ---------------------------------------------------------------------------
// Base poller (Part 6)
// ---------------------------------------------------------------------------

// spotSourceNone is not a value base_state can hold — the schema allows only the
// two real markets — but it is a value this counter must have, because "the row
// carried no price at all" is exactly the outcome an alert needs to see and is
// otherwise indistinguishable from the poller not running.
const spotSourceNone = "none"

// BaseMetrics is what the Base poller exports (API spec section 6).
//
// The block gauge is the staleness signal for this feed. base_state has no
// WebSocket stream behind it, so ingest_last_seen_timestamp_seconds says nothing
// about it; a block height that stops advancing is the equivalent statement, and
// it doubles as the anchor for auditing a row against a block explorer.
type BaseMetrics struct {
	block      prometheus.Gauge
	spotSource *prometheus.CounterVec
}

func newBaseMetrics(reg prometheus.Registerer) *BaseMetrics {
	m := &BaseMetrics{
		block: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "base_last_block",
			Help:      "Block height the last successful Base poll read at. A height that stops advancing is a stale feed.",
		}),
		spotSource: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "base_spot_px_source_total",
			Help:      "Where each base_state.spot_px came from: the Base pool, or the Coinbase mid standing in for it.",
		}, []string{"source"}),
	}
	reg.MustRegister(m.block, m.spotSource)
	// Both label values are initialized so a fallback that has never fired is a
	// visible zero rather than an absent series — the same reason the per-stream
	// collectors are.
	m.spotSource.WithLabelValues(db.SpotSourceDEX).Add(0)
	m.spotSource.WithLabelValues(db.SpotSourceCoinbase).Add(0)
	m.spotSource.WithLabelValues(spotSourceNone).Add(0)
	return m
}

// observed records a completed poll.
func (m *BaseMetrics) observed(block uint64) {
	// Float, like every Prometheus gauge. Base block heights are eight digits
	// and a float64 is exact to sixteen, so this is lossless for the life of the
	// chain.
	m.block.Set(float64(block))
}

// spot records which source a row's price came from.
func (m *BaseMetrics) spot(source string) { m.spotSource.WithLabelValues(source).Inc() }
