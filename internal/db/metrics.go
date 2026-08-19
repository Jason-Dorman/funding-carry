package db

import "github.com/prometheus/client_golang/prometheus"

// WriterMetrics is the writer's slice of the Prometheus catalogue (API spec
// section 6). The namespace is the owning binary's metric prefix — `ingest` or
// `carry` — because the writer is shared code but the throughput question
// ("is this binary keeping up?") is asked per binary.
type WriterMetrics struct {
	rowsWritten    *prometheus.CounterVec
	rowsConflicted *prometheus.CounterVec
	batchTime      prometheus.Histogram
	queueDepth     prometheus.Gauge
}

// NewWriterMetrics registers the writer's collectors on the binary's registry.
func NewWriterMetrics(reg prometheus.Registerer, namespace string) *WriterMetrics {
	m := &WriterMetrics{
		rowsWritten: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rows_written_total",
			Help:      "Rows the database actually inserted, by table (command tag, not rows submitted).",
		}, []string{"table"}),
		rowsConflicted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rows_conflicted_total",
			Help:      "Rows the database already had, by table: inserts that hit their natural key and did nothing.",
		}, []string{"table"}),
		batchTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "write_batch_seconds",
			Help:      "Time to send one batch of inserts, including retries.",
			// A healthy batch is single-digit milliseconds; the top of the range
			// is where a stalled database shows up before backpressure does.
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}),
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "write_queue_depth",
			Help:      "Rows waiting on the writer's channel; a rising floor is backpressure.",
		}),
	}
	reg.MustRegister(m.rowsWritten, m.rowsConflicted, m.batchTime, m.queueDepth)
	return m
}

func (m *WriterMetrics) observeQueue(depth int) {
	m.queueDepth.Set(float64(depth))
}

func (m *WriterMetrics) observeBatch(seconds float64) {
	m.batchTime.Observe(seconds)
}

func (m *WriterMetrics) addRows(table string, n int64) {
	m.rowsWritten.WithLabelValues(table).Add(float64(n))
}

// addConflicts records rows the database already had. Separating them from
// rows_written is what makes a retry storm visible: with idempotent inserts a
// repeated batch is silent in every other signal, and a single counter that
// added the two together would read as healthy throughput.
func (m *WriterMetrics) addConflicts(table string, n int64) {
	m.rowsConflicted.WithLabelValues(table).Add(float64(n))
}
