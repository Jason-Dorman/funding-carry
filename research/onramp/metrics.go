package main

import "github.com/prometheus/client_golang/prometheus"

// namespace prefixes every series this binary exports. `onramp_` is deliberately
// outside the catalogue in api-spec section 6: the exercise must not be mistaken
// for a service, and nothing scrapes it but a human with curl.
const namespace = "onramp"

// onrampMetrics is the whole of the toy's instrumentation, split by the
// component that owns each collector. Collectors are registered on the binary's
// own registry rather than the client_golang default, so what this binary
// exports is a visible property of the wiring instead of a side effect of
// whichever packages happen to be imported (internal/metrics).
type onrampMetrics struct {
	feed   *feedMetrics
	writer *writerMetrics
}

type feedMetrics struct {
	ticks *prometheus.CounterVec
}

type writerMetrics struct {
	rowsWritten prometheus.Counter
	batchTime   prometheus.Histogram
	queueDepth  prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *onrampMetrics {
	m := &onrampMetrics{
		feed: &feedMetrics{
			ticks: prometheus.NewCounterVec(prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "ticks_produced_total",
				Help:      "Fake feed messages sent onto the fan-in channel, by product.",
			}, []string{"product"}),
		},
		writer: &writerMetrics{
			rowsWritten: prometheus.NewCounter(prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "rows_written_total",
				Help:      "Ticks persisted to onramp_ticks.",
			}),
			batchTime: prometheus.NewHistogram(prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "write_batch_seconds",
				Help:      "Time to send one batch of inserts.",
				Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12),
			}),
			queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "write_queue_depth",
				Help:      "Ticks waiting on the fan-in channel; a rising floor is backpressure.",
			}),
		},
	}
	reg.MustRegister(m.feed.ticks, m.writer.rowsWritten, m.writer.batchTime, m.writer.queueDepth)
	return m
}

// The counters are wrapped in methods so the call sites read as events rather
// than as Prometheus API calls, and so the float64 conversions Prometheus
// requires live in one place instead of being sprinkled through the feed and
// writer — every one of them is a float that must never touch a price.

func (m *feedMetrics) produced(product string) {
	m.ticks.WithLabelValues(product).Inc()
}

func (m *writerMetrics) written(rows int) {
	m.rowsWritten.Add(float64(rows))
}

func (m *writerMetrics) batchSeconds(seconds float64) {
	m.batchTime.Observe(seconds)
}

func (m *writerMetrics) queued(depth int) {
	m.queueDepth.Set(float64(depth))
}
