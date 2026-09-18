package fix

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Namespace prefixes the series sim-venue owns, and is the namespace the binary
// gives the shared writer metrics, so simv_orders_total sits under the same
// prefix as simv_rows_written_total (API spec section 6).
const Namespace = "simv"

// Directions for fix_msgs_total. A message counted without one would answer
// "how much FIX traffic is there" and not "which way is it going", and the
// second is the question that separates a venue that has stopped replying from
// a client that has stopped asking.
const (
	dirIn  = "in"
	dirOut = "out"
)

// Order outcomes for simv_orders_total. Every value is initialized at zero so a
// rejection path that has never fired is a visible zero rather than an absent
// series — the same reason ingest initializes its per-stream gauges.
const (
	resultAccepted = "accepted"
	resultRejected = "rejected"
	resultFilled   = "filled"
	resultCanceled = "canceled"
)

// SessionCatalogue is the three fix_* series both ends of the session export.
//
// The names carry no binary prefix, which is deliberate and is how API spec
// section 6 lists them: the acceptor in sim-venue and the initiator in carry
// export the same three, and the session label is what separates them. A
// dashboard asking "is the FIX link up" should not have to ask it twice with
// two spellings. This is what carry registers; sim-venue registers Metrics,
// which is this plus its own.
type SessionCatalogue struct {
	sessionUp *prometheus.GaugeVec
	msgs      *prometheus.CounterVec
	resends   *prometheus.CounterVec
}

// NewSessionCatalogue registers the session series on a registry.
func NewSessionCatalogue(reg prometheus.Registerer) *SessionCatalogue {
	c := &SessionCatalogue{
		sessionUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fix_session_up",
			Help: "1 while the FIX session is logged on, 0 otherwise.",
		}, []string{"session"}),
		msgs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fix_msgs_total",
			Help: "FIX messages by session, MsgType and direction.",
		}, []string{"session", "msg_type", "dir"}),
		resends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fix_resend_events_total",
			Help: "Sequence recoveries: ResendRequests seen on the session, either direction.",
		}, []string{"session"}),
	}
	reg.MustRegister(c.sessionUp, c.msgs, c.resends)
	return c
}

// Metrics is the session catalogue plus sim-venue's own three series.
type Metrics struct {
	*SessionCatalogue

	orders  *prometheus.CounterVec
	bookAge prometheus.Gauge
	resting prometheus.Gauge
}

// NewMetrics registers the collectors on the binary's own registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		SessionCatalogue: NewSessionCatalogue(reg),
		orders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "orders_total",
			Help:      "Orders by outcome: accepted, rejected at entry, filled, canceled.",
		}, []string{"result"}),
		bookAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "book_age_seconds",
			Help: "Age of the top-of-book the fill model last priced against. " +
				"Past the configured maximum the simulator refuses orders, so this is " +
				"the series that explains a venue that has started rejecting everything.",
		}),
		resting: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "resting_orders",
			Help:      "Orders live on the simulator's book: accepted, not yet filled or canceled.",
		}),
	}
	reg.MustRegister(m.orders, m.bookAge, m.resting)

	for _, result := range []string{resultAccepted, resultRejected, resultFilled, resultCanceled} {
		m.orders.WithLabelValues(result).Add(0)
	}
	return m
}

// session binds the session label once, which is the only label the three fix_*
// series share and the one a caller would otherwise have to remember to spell
// identically in three places.
func (c *SessionCatalogue) session(id string) *SessionMetrics {
	s := &SessionMetrics{id: id, up: c.sessionUp.WithLabelValues(id), msgs: c.msgs, resends: c.resends.WithLabelValues(id)}
	// The gauge exists before the first logon, at zero: FixSessionDown alerts on
	// `fix_session_up == 0`, and PromQL over a series that does not exist yields
	// nothing rather than firing. A venue that never accepted a connection at all
	// is exactly the case the alert is for.
	s.up.Set(0)
	s.resends.Add(0)
	return s
}

// SessionMetrics is one session's slice of the catalogue.
type SessionMetrics struct {
	id      string
	up      prometheus.Gauge
	msgs    *prometheus.CounterVec
	resends prometheus.Counter
}

func (s *SessionMetrics) loggedOn()  { s.up.Set(1) }
func (s *SessionMetrics) loggedOff() { s.up.Set(0) }

// message counts one message in one direction. msgType is the raw FIX tag 35
// value, which is a closed vocabulary and therefore safe as a label.
func (s *SessionMetrics) message(msgType, dir string) {
	s.msgs.WithLabelValues(s.id, msgType, dir).Inc()
}

func (s *SessionMetrics) resend() { s.resends.Inc() }

func (m *Metrics) order(result string) { m.orders.WithLabelValues(result).Inc() }

// observeBookAge records the age of the snapshot the fill model is pricing
// against. A simulator that has never read one is infinitely stale rather than
// perfectly fresh: zero would be the healthiest possible reading in the one
// state where the venue rejects every order it receives, and the obvious alert —
// simv_book_age_seconds above SIM_BOOK_MAX_AGE_SECS — would never fire on the
// worst case it exists for. Three of the six review lenses found this
// independently.
func (m *Metrics) observeBookAge(d time.Duration, have bool) {
	if !have {
		m.bookAge.Set(math.Inf(1))
		return
	}
	m.bookAge.Set(d.Seconds())
}

func (m *Metrics) observeResting(n int) { m.resting.Set(float64(n)) }
