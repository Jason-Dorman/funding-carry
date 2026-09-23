package venue

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// namespace is carry's metric prefix. It is spelled here rather than imported
// from internal/carry because this package must stay importable by the
// packages that will consume the cache — features, risk, and carry's own
// decision engine — and an import in the other direction would close a
// cycle.
const namespace = "carry"

// Metrics is the venue-state slice of carry's catalogue (API spec section 6).
type Metrics struct {
	age     *prometheus.GaugeVec
	stale   *prometheus.GaugeVec
	feedOK  prometheus.Gauge
	refresh prometheus.Histogram
}

// NewMetrics registers the collectors on the binary's registry.
//
// Every source's series is created at once, reading missing (age +Inf, stale
// 1) and feed_ok 0, so that a source nothing has ever recorded is a visible
// stale rather than an absent series — PromQL over a series that does not
// exist yields nothing, and an alert on nothing never fires.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		age: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "venue_state_age_seconds",
			Help: "Age of the newest row per source as of the last refresh: now minus the row's own " +
				"sampling timestamp, so the age of the observation and not of the query. +Inf when " +
				"the source has no row at all.",
		}, []string{"source"}),
		stale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "venue_state_stale",
			Help: "1 when the source is missing or older than STALE_FEED_SECS, else 0. This is the " +
				"typed flag the risk engine reads, exported as decided rather than recomputed from the age.",
		}, []string{"source"}),
		feedOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "feed_ok",
			// The two selectors are quoted because an operator copies them into
			// Prometheus or Grafana, and PromQL rejects an unquoted label value
			// outright. This string exists so the gate condition travels with
			// the series; one that does not parse would teach the wrong syntax
			// at exactly the point of use (Part 9 adversarial review).
			Help: "MARKET-DATA freshness, not account state: 1 when both the perp and the spot " +
				"source are fresh, else 0. Never a 'safe to act' signal on its own - anything that " +
				`gates order flow gates on this AND carry_venue_state_stale{source="account"} == 0 AND ` +
				`carry_venue_state_stale{source="wallet"} == 0. Alone it is the public-stack reading, ` +
				"where those two sources do not exist.",
		}),
		refresh: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "venue_refresh_seconds",
			Help:      "Time one refresh of the venue state cache took, all sources included.",
			// 0.05 is a bucket boundary on purpose: the part's acceptance
			// criterion is a refresh under fifty milliseconds, so the share of
			// refreshes inside it reads straight off the cumulative count.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}),
	}
	reg.MustRegister(m.age, m.stale, m.feedOK, m.refresh)

	for _, src := range Sources {
		m.set(src, Freshness{Missing: true, Stale: true})
	}
	m.feedOK.Set(0)
	return m
}

// observe publishes one refresh's result.
func (m *Metrics) observe(s State, took time.Duration) {
	for _, src := range Sources {
		m.set(src, s.Staleness.Of(src))
	}
	m.feedOK.Set(boolGauge(s.Staleness.FeedOK()))
	m.refresh.Observe(took.Seconds())
}

func (m *Metrics) set(src Source, f Freshness) {
	age := math.Inf(1)
	if !f.Missing {
		age = f.Age.Seconds()
	}
	m.age.WithLabelValues(string(src)).Set(age)
	m.stale.WithLabelValues(string(src)).Set(boolGauge(f.Stale))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
