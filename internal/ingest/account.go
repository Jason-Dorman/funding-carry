package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/coinbase"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// AccountSource is the authenticated account API as this package needs it.
//
// Declared at the consumer so the poller is testable without a credential, and
// so that internal/coinbase — the one package that holds the key — is a
// dependency of this file rather than of the whole binary.
type AccountSource interface {
	BalanceSummary(ctx context.Context) (coinbase.Account, error)
	Positions(ctx context.Context) ([]coinbase.Position, error)
	IntradayMarginEnabled(ctx context.Context) (bool, error)
}

// AccountPoller writes cb_account_state: the polled truth the risk engine reads
// its margin ratio from, and the balances the treasury reconciles against.
//
// The system never derives a liquidation price of its own (API spec section
// 5.2). It reads the venue's own available margin and liquidation threshold and
// divides them, which is why a failure here degrades to staleness rather than
// to an estimate.
type AccountPoller struct {
	source   AccountSource
	product  string
	sink     Sink
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time

	// warned keeps a repeating failure to one log line per transition rather
	// than one per tick; a credential that is wrong is wrong every five seconds.
	warned bool
}

// NewAccountPoller builds the poller. product is the perp, used to pick this
// system's position out of whatever the account holds.
func NewAccountPoller(source AccountSource, product string, sink Sink,
	interval time.Duration, log *slog.Logger, now func() time.Time,
) *AccountPoller {
	if now == nil {
		now = time.Now
	}
	return &AccountPoller{
		source: source, product: product, sink: sink, interval: interval,
		log: log.With("component", "account_poller"), now: now,
	}
}

// Run polls on the interval until ctx is canceled.
func (a *AccountPoller) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.Poll(ctx); err != nil {
				if ctx.Err() == nil {
					a.log.Info("account poller stopping", "reason", err)
				}
				return
			}
		}
	}
}

// Poll writes one snapshot. It returns an error only when the writer has gone.
//
// The row is written on the sampling boundary, not at time.Now(): (ts) is its
// identity and a clock reading would make the key decorative (API spec section
// 5.3).
func (a *AccountPoller) Poll(ctx context.Context) error {
	at := a.now().UTC()
	row := db.AccountStateRow{TS: at.Truncate(a.interval)}

	account, err := a.source.BalanceSummary(ctx)
	if err != nil {
		return a.degrade(ctx, "balance summary", err)
	}
	a.warned = false

	row.AvailableMargin = account.AvailableMargin
	row.LiquidationThreshold = account.LiquidationThreshold
	row.MarginRatio = account.MarginRatio()
	row.CFMUSDBalance = account.CFMBalance
	row.CBIUSDBalance = account.CBIBalance
	row.FuturesBuyingPower = account.FuturesBuyingPower
	row.UnrealizedPnL = account.UnrealizedPnL

	// Positions and the margin setting are polled beside the balances but are
	// not allowed to sink the row: a snapshot with margin and no position is
	// still the figure the risk engine needs.
	if positions, perr := a.source.Positions(ctx); perr != nil {
		a.log.Warn("positions poll failed", "error", perr)
	} else {
		for _, p := range positions {
			if p.ProductID != a.product {
				continue
			}
			row.ContractsHeld = db.Opt(p.Contracts)
			row.AvgEntryPrice = p.AvgEntryPrice
			if p.UnrealizedPnL.Valid {
				row.UnrealizedPnL = p.UnrealizedPnL
			}
		}
	}

	if intraday, merr := a.source.IntradayMarginEnabled(ctx); merr != nil {
		a.log.Warn("intraday margin setting poll failed", "error", merr)
	} else {
		row.IntradayMarginEnabled = db.Opt(intraday)
		if intraday {
			// v1 requires this off (spec section 9). It is a setting a human can
			// change from a phone, so it is checked every poll and shouted about
			// rather than assumed from config.
			a.log.Error("intraday margin is ENABLED on this account; v1 requires it off",
				"remedy", "turn it off in the Coinbase futures settings before trading")
		}
	}

	return submit(ctx, a.sink, row)
}

// degrade turns a venue failure into staleness. Only the writer going away
// stops the poller (architecture section 8).
func (a *AccountPoller) degrade(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errWriterGone) {
		return err
	}
	if !a.warned {
		var apiErr *coinbase.APIError
		retryable := !errors.As(err, &apiErr) || apiErr.Retryable()
		a.log.Warn("account poll failed; margin ratio is going stale",
			"what", what, "error", err, "retryable", retryable)
		a.warned = true
	}
	return nil
}
