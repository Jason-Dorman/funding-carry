package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The "newest row" reads behind carry's venue state cache (Part 9).
//
// Each answers one question — what is the most recent thing recorded for this
// source? — and reports "nothing recorded" as ok=false rather than as an error,
// because an empty table is what a fresh stack looks like, and the cache's job
// is to call that stale rather than to fail on it.
//
// They return the same row types the writer takes. A table has one shape, and
// the row struct beside its migration is already that shape; a second "stored"
// struct per table would be a copy that could drift from the first. Every
// optional column comes back as it was stored: NULL stays NULL, and the cache
// forwards it as such. Whether a missing mark is a gap to carry as NULL or a
// reason to block is the feature engine's and the risk engine's to decide, one
// layer up (API spec section 5: "a zero would be a value the decision engine
// acts on").

// LatestVenueState returns the newest cb_venue_state row for a product, and
// whether there was one.
func (r *Reader) LatestVenueState(ctx context.Context, product string) (VenueStateRow, bool, error) {
	return one(ctx, r.q, "latest venue state for "+product,
		`SELECT ts, product_id, futures_mark, spot_mark, mid,
		        funding_rate_hourly, funding_rate_est, funding_source,
		        funding_annualized, premium_proxy, spread_bps, open_interest,
		        maintenance_window
		   FROM cb_venue_state
		  WHERE product_id = $1
		  ORDER BY ts DESC LIMIT 1`,
		[]any{product},
		func(rows pgx.Rows) (VenueStateRow, error) {
			var v VenueStateRow
			var source *string
			err := rows.Scan(&v.TS, &v.ProductID, &v.FuturesMark, &v.SpotMark, &v.Mid,
				&v.FundingRateHourly, &v.FundingRateEst, &source,
				&v.FundingAnnualized, &v.PremiumProxy, &v.SpreadBps, &v.OpenInterest,
				&v.MaintenanceWindow)
			v.TS = v.TS.UTC()
			v.FundingSource = deref(source)
			return v, err
		})
}

// LatestAccountState returns the newest cb_account_state row, and whether
// there was one. The table has no product dimension: it is one account.
func (r *Reader) LatestAccountState(ctx context.Context) (AccountStateRow, bool, error) {
	return one(ctx, r.q, "latest account state",
		`SELECT ts, available_margin, liquidation_threshold, margin_ratio,
		        cfm_usd_balance, cbi_usd_balance, futures_buying_power,
		        contracts_held, avg_entry_price, unrealized_pnl,
		        intraday_margin_enabled
		   FROM cb_account_state
		  ORDER BY ts DESC LIMIT 1`,
		nil,
		func(rows pgx.Rows) (AccountStateRow, error) {
			var a AccountStateRow
			err := rows.Scan(&a.TS, &a.AvailableMargin, &a.LiquidationThreshold, &a.MarginRatio,
				&a.CFMUSDBalance, &a.CBIUSDBalance, &a.FuturesBuyingPower,
				&a.ContractsHeld, &a.AvgEntryPrice, &a.UnrealizedPnL,
				&a.IntradayMarginEnabled)
			a.TS = a.TS.UTC()
			return a, err
		})
}

// LatestBaseState returns the newest base_state row, and whether there was
// one. One wallet, so no dimension beyond time.
func (r *Reader) LatestBaseState(ctx context.Context) (BaseStateRow, bool, error) {
	return one(ctx, r.q, "latest base state",
		`SELECT ts, spot_px, spot_px_source, wallet_eth, wallet_usdc, gas_gwei
		   FROM base_state
		  ORDER BY ts DESC LIMIT 1`,
		nil,
		func(rows pgx.Rows) (BaseStateRow, error) {
			var b BaseStateRow
			var source *string
			err := rows.Scan(&b.TS, &b.SpotPx, &source, &b.WalletETH, &b.WalletUSDC, &b.GasGwei)
			b.TS = b.TS.UTC()
			b.SpotPxSource = deref(source)
			return b, err
		})
}

// Product returns the cb_products row for a product, and whether there is one.
//
// It is metadata rather than a series — a single row that `make migrate` seeds
// and the REST poller rewrites — so it has no "latest": there is one row or
// none. UpdatedAt says when the venue last confirmed it.
func (r *Reader) Product(ctx context.Context, product string) (ProductRow, bool, error) {
	return one(ctx, r.q, "product "+product,
		`SELECT product_id, contract_size, tick, tick_value,
		        max_leverage_overnight, max_leverage_intraday,
		        maker_fee_bps, taker_fee_bps, status, updated_at
		   FROM cb_products
		  WHERE product_id = $1`,
		[]any{product},
		func(rows pgx.Rows) (ProductRow, error) {
			var p ProductRow
			var status *string
			err := rows.Scan(&p.ProductID, &p.ContractSize, &p.Tick, &p.TickValue,
				&p.MaxLeverageOvernight, &p.MaxLeverageIntraday,
				&p.MakerFeeBps, &p.TakerFeeBps, &status, &p.UpdatedAt)
			p.UpdatedAt = p.UpdatedAt.UTC()
			p.Status = deref(status)
			return p, err
		})
}

// one runs a query that yields at most one row and scans it. No row is
// (zero, false, nil); a query or scan failure is wrapped with what was being
// read, so the error names the source and not just the SQL state.
func one[T any](ctx context.Context, q Querier, what, sql string, args []any,
	scan func(pgx.Rows) (T, error),
) (T, bool, error) {
	var zero T
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return zero, false, fmt.Errorf("read %s: %w", what, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return zero, false, fmt.Errorf("read %s: %w", what, err)
		}
		return zero, false, nil
	}
	out, err := scan(rows)
	if err != nil {
		return zero, false, fmt.Errorf("scan %s: %w", what, err)
	}
	// Close before Err so a failure that only surfaces at the end of the
	// result set is still reported rather than lost to the deferred Close.
	rows.Close()
	if err = rows.Err(); err != nil {
		return zero, false, fmt.Errorf("read %s: %w", what, err)
	}
	return out, true, nil
}

// deref maps a NULL text column to the empty string, the inverse of the
// writer's text().
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
