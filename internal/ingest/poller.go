package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Poller is the REST half of ingest: the product metadata and the figures that
// appear nowhere on the WebSocket.
//
// Open interest is the reason it exists at a data level — no channel carries it
// — and the trading session is the reason it matters operationally: the product
// payload names the exact close and reopen, where the configured maintenance
// window only knows a weekly rule.
//
// A poll failure degrades to staleness and never stops the binary (architecture
// section 8). The consequence of a failed poll is that open interest and the
// session age out of the venue-state row, which is visible, rather than that
// ingest dies.
type Poller struct {
	perp     string
	client   *RESTClient
	state    *VenueState
	sink     Sink
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time

	// lastProduct is what was last written to cb_products. The row is an upsert
	// on a single primary key, so re-writing an unchanged one costs a round trip
	// and moves updated_at for no reason; the table is metadata, not a series.
	lastProduct db.ProductRow
	haveProduct bool
}

// NewPoller builds the REST poller.
func NewPoller(perp string, client *RESTClient, state *VenueState, sink Sink,
	interval time.Duration, log *slog.Logger, now func() time.Time,
) *Poller {
	if now == nil {
		now = time.Now
	}
	return &Poller{
		perp:     perp,
		client:   client,
		state:    state,
		sink:     sink,
		interval: interval,
		log:      log.With("component", "rest_poller"),
		now:      now,
	}
}

// Run polls on the interval until ctx is canceled.
func (p *Poller) Run(ctx context.Context) {
	// One poll immediately, so a restart does not leave open interest and the
	// session absent for a whole interval.
	if err := p.Poll(ctx); err != nil && ctx.Err() == nil {
		p.log.Info("poller stopping", "reason", err)
		return
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.Poll(ctx); err != nil {
				if ctx.Err() == nil {
					p.log.Info("poller stopping", "reason", err)
				}
				return
			}
		}
	}
}

// Poll reads the product once. It returns an error only when the writer has
// gone; a venue failure is logged and left for the next tick.
func (p *Poller) Poll(ctx context.Context) error {
	product, err := p.client.Product(ctx, p.perp)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The one thing worth distinguishing in the log: a refusal the venue
		// will repeat, versus one that a moment will clear.
		var status *StatusError
		retryable := !errors.As(err, &status) || status.Retryable()
		p.log.Warn("product poll failed", "error", err, "retryable", retryable)
		return nil
	}

	now := p.now()
	p.state.ObserveOpenInterest(p.perp, product.OpenInterest)
	p.state.ObserveSession(p.perp, product.Session)
	p.state.ObserveVenueFunding(p.perp, product.FundingRate, product.FundingTime, now)

	row := productRow(product, now)
	if p.haveProduct && sameProduct(p.lastProduct, row) {
		return nil
	}
	if err := submit(ctx, p.sink, row); err != nil {
		return err
	}
	p.lastProduct, p.haveProduct = row, true
	p.log.Info("product metadata updated",
		"product", product.ProductID,
		"contract_size", product.ContractSize.Decimal.String(),
		"status", product.Status,
		"session_open", product.Session.IsOpen,
		"venue_funding_rate", product.FundingRate.Decimal.String(),
		"funding_interval", product.FundingInterval)
	return nil
}

// productRow maps the venue's product to the row cb_products stores.
//
// The fee and leverage columns stay NULL: the public payload carries neither
// (max_leverage comes back empty for this contract), and they need the
// authenticated fee-tier endpoint. NULL is "not observed", which is the truth.
func productRow(p Product, at time.Time) db.ProductRow {
	return db.ProductRow{
		ProductID:    p.ProductID,
		ContractSize: p.ContractSize,
		Tick:         p.Tick,
		Status:       p.Status,
		UpdatedAt:    at,
	}
}

// sameProduct compares everything but the timestamp, since the timestamp is the
// thing that would otherwise make every row look new.
func sameProduct(a, b db.ProductRow) bool {
	return a.ProductID == b.ProductID &&
		a.Status == b.Status &&
		sameNum(a.ContractSize, b.ContractSize) &&
		sameNum(a.Tick, b.Tick)
}

func sameNum(a, b decimal.NullDecimal) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Decimal.Equal(b.Decimal)
}
