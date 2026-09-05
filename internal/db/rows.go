package db

import (
	"time"

	"github.com/shopspring/decimal"
)

// Row is one pending insert handed to the writer.
//
// The single method is unexported, so every row type is defined in this package
// beside the migration that creates its table: a producer cannot invent a shape
// the schema does not have, and the set of tables the system writes to is
// enumerable by reading one file.
type Row interface {
	row() rowData
}

// rowData is what the writer needs to turn a typed row into SQL. columns and
// values are positional and must be the same length; the writer builds the
// statement from the first row of a batch and reuses it for the rest.
type rowData struct {
	table    string
	columns  []string
	values   []any
	conflict string // optional ON CONFLICT clause, for the idempotent tables
}

// Closed vocabularies the schema enforces with CHECK constraints, spelled out
// here so a caller never has to guess the literal. The domain types that mirror
// some of these (Leg, Side, ExecState, DecisionState in API spec section 3) are
// defined by their own packages and mapped to these strings at the persistence
// boundary — this package sits below them and must not import them.
const (
	FundingSourceVenue    = "venue"
	FundingSourceComputed = "computed"
	// FundingSourceBackfilled marks an hour reconstructed from candle history
	// rather than recorded live: the venue's own formula on one-minute candle
	// closes instead of three-minute VWAPs of trades. Same formula, coarser
	// inputs, different error profile — which is why it is not 'computed'
	// (ADR-0015).
	FundingSourceBackfilled = "backfilled"

	// SpotSourceDEX and SpotSourceCoinbase are the two markets base_state.spot_px
	// can come from: the configured Base pool's own mid, read on chain at the
	// row's block, and the Coinbase spot mid standing in when the pool cannot be
	// read (ADR-0018). They are different markets, so a row that could not say
	// which one it held would be a price whose market is unknowable after the
	// fact — and the spot leg is marked against this column.
	SpotSourceDEX      = "dex"
	SpotSourceCoinbase = "coinbase"

	FundingKindAccrual    = "ACCRUAL"
	FundingKindSettlement = "SETTLEMENT"

	VenuePaper = "paper"
	VenueSim   = "sim"
	VenueLive  = "live"
)

// Opt marks an optional non-numeric column as present. A nil pointer writes SQL
// NULL, which is how "the venue did not report this" stays distinguishable from
// "the venue reported zero" — a distinction that matters for a trade count, a
// contract count and a margin field alike.
func Opt[T any](v T) *T { return &v }

// Num marks an optional numeric column as present. The zero value of
// decimal.NullDecimal writes NULL, so a field left alone is absent rather than
// zero.
func Num(d decimal.Decimal) decimal.NullDecimal {
	return decimal.NullDecimal{Decimal: d, Valid: true}
}

// text maps the empty string to NULL. Every optional text column in the schema
// is a closed vocabulary with a CHECK constraint, and "" is in none of them.
func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// stamp maps the zero time to NULL, so an unset closed_at or resolved_at is
// absent rather than year one.
func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ---------------------------------------------------------------------------
// Hypertables (API spec section 5.1) — written by ingest
// ---------------------------------------------------------------------------

// VenueStateRow is one sample of the perp venue: the two marks, the hourly
// funding rate with its provenance, and the premium and spread derived from
// them.
type VenueStateRow struct {
	TS                time.Time
	ProductID         string
	FuturesMark       decimal.NullDecimal
	SpotMark          decimal.NullDecimal
	Mid               decimal.NullDecimal
	FundingRateHourly decimal.NullDecimal
	FundingRateEst    decimal.NullDecimal
	FundingSource     string // FundingSourceVenue | FundingSourceComputed | FundingSourceBackfilled
	FundingAnnualized decimal.NullDecimal
	PremiumProxy      decimal.NullDecimal
	SpreadBps         decimal.NullDecimal
	OpenInterest      decimal.NullDecimal
	MaintenanceWindow bool
}

func (r VenueStateRow) row() rowData {
	return rowData{
		table: "cb_venue_state",
		columns: []string{
			"ts", "product_id", "futures_mark", "spot_mark", "mid",
			"funding_rate_hourly", "funding_rate_est", "funding_source",
			"funding_annualized", "premium_proxy", "spread_bps", "open_interest",
			"maintenance_window",
		},
		values: []any{
			r.TS, r.ProductID, r.FuturesMark, r.SpotMark, r.Mid,
			r.FundingRateHourly, r.FundingRateEst, text(r.FundingSource),
			r.FundingAnnualized, r.PremiumProxy, r.SpreadBps, r.OpenInterest,
			r.MaintenanceWindow,
		},
		conflict: "ON CONFLICT (product_id, ts) DO NOTHING",
	}
}

// BarRow is a completed candle. Re-inserting one is a no-op, which is what makes
// REST backfill idempotent.
type BarRow struct {
	TS         time.Time // bar close
	ProductID  string
	TF         string // '5m' from the WS candles channel, '1m' from REST
	Open       decimal.Decimal
	High       decimal.Decimal
	Low        decimal.Decimal
	Close      decimal.Decimal
	Volume     decimal.Decimal
	TradeCount *int32
}

func (r BarRow) row() rowData {
	return rowData{
		table: "cb_bars",
		columns: []string{
			"ts", "product_id", "tf", "open", "high", "low", "close",
			"volume", "trade_count",
		},
		values: []any{
			r.TS, r.ProductID, r.TF, r.Open, r.High, r.Low, r.Close,
			r.Volume, r.TradeCount,
		},
		conflict: "ON CONFLICT (product_id, tf, ts) DO NOTHING",
	}
}

// BookSnapshotRow is a periodic top-N book snapshot with the derived imbalance
// and impact prices the feature engine reads.
type BookSnapshotRow struct {
	TS            time.Time
	ProductID     string
	BestBid       decimal.NullDecimal
	BestAsk       decimal.NullDecimal
	BidPx         []decimal.Decimal
	BidDepth      []decimal.Decimal
	AskPx         []decimal.Decimal
	AskDepth      []decimal.Decimal
	ImbalanceTopN decimal.NullDecimal
	ImpactBidPx   decimal.NullDecimal
	ImpactAskPx   decimal.NullDecimal
}

func (r BookSnapshotRow) row() rowData {
	return rowData{
		table: "cb_book_snapshots",
		columns: []string{
			"ts", "product_id", "best_bid", "best_ask",
			"bid_px", "bid_depth", "ask_px", "ask_depth",
			"imbalance_top_n", "impact_bid_px", "impact_ask_px",
		},
		values: []any{
			r.TS, r.ProductID, r.BestBid, r.BestAsk,
			r.BidPx, r.BidDepth, r.AskPx, r.AskDepth,
			r.ImbalanceTopN, r.ImpactBidPx, r.ImpactAskPx,
		},
		conflict: "ON CONFLICT (product_id, ts) DO NOTHING",
	}
}

// TradesAggRow is one bucket of aggregated trades. The bucket length is part of
// the key: the funding estimator marks against the 3-minute VWAP specifically.
type TradesAggRow struct {
	TS          time.Time // bucket end
	ProductID   string
	BucketSecs  int32
	BuyVol      decimal.Decimal
	SellVol     decimal.Decimal
	TradeCount  int32
	VWAP        decimal.NullDecimal
	MaxSingleSz decimal.NullDecimal
	SweepCount  *int32
}

func (r TradesAggRow) row() rowData {
	return rowData{
		table: "cb_trades_agg",
		columns: []string{
			"ts", "product_id", "bucket_secs", "buy_vol", "sell_vol",
			"trade_count", "vwap", "max_single_sz", "sweep_count",
		},
		values: []any{
			r.TS, r.ProductID, r.BucketSecs, r.BuyVol, r.SellVol,
			r.TradeCount, r.VWAP, r.MaxSingleSz, r.SweepCount,
		},
		conflict: "ON CONFLICT (product_id, bucket_secs, ts) DO NOTHING",
	}
}

// BaseStateRow is the Base side of the book: wallet inventory, reference price
// and gas.
type BaseStateRow struct {
	TS     time.Time
	SpotPx decimal.NullDecimal
	// SpotPxSource is SpotSourceDEX or SpotSourceCoinbase. The schema pairs it
	// with SpotPx: a price with no source and a source with no price are both
	// rejected, so the column cannot drift into being set only sometimes.
	SpotPxSource string
	WalletETH    decimal.NullDecimal
	WalletUSDC   decimal.NullDecimal
	GasGwei      decimal.NullDecimal
}

func (r BaseStateRow) row() rowData {
	return rowData{
		table:    "base_state",
		columns:  []string{"ts", "spot_px", "spot_px_source", "wallet_eth", "wallet_usdc", "gas_gwei"},
		values:   []any{r.TS, r.SpotPx, text(r.SpotPxSource), r.WalletETH, r.WalletUSDC, r.GasGwei},
		conflict: "ON CONFLICT (ts) DO NOTHING",
	}
}

// AccountStateRow is the polled account and margin snapshot. The risk engine
// reads margin_ratio from these rows rather than deriving a liquidation price,
// and the treasury reconciles the two cash balances against them.
type AccountStateRow struct {
	TS                    time.Time
	AvailableMargin       decimal.NullDecimal
	LiquidationThreshold  decimal.NullDecimal
	MarginRatio           decimal.NullDecimal
	CFMUSDBalance         decimal.NullDecimal
	CBIUSDBalance         decimal.NullDecimal
	FuturesBuyingPower    decimal.NullDecimal
	ContractsHeld         *int64
	AvgEntryPrice         decimal.NullDecimal
	UnrealizedPnL         decimal.NullDecimal
	IntradayMarginEnabled *bool
}

func (r AccountStateRow) row() rowData {
	return rowData{
		table: "cb_account_state",
		columns: []string{
			"ts", "available_margin", "liquidation_threshold", "margin_ratio",
			"cfm_usd_balance", "cbi_usd_balance", "futures_buying_power",
			"contracts_held", "avg_entry_price", "unrealized_pnl",
			"intraday_margin_enabled",
		},
		values: []any{
			r.TS, r.AvailableMargin, r.LiquidationThreshold, r.MarginRatio,
			r.CFMUSDBalance, r.CBIUSDBalance, r.FuturesBuyingPower,
			r.ContractsHeld, r.AvgEntryPrice, r.UnrealizedPnL,
			r.IntradayMarginEnabled,
		},
		conflict: "ON CONFLICT (ts) DO NOTHING",
	}
}

// FeatureRow is one decision tick's worth of computed features (spec section
// 6.3). Every numeric is optional because a rolling window that has not filled
// yet must read as NULL: a z-score of zero before thirty days of history is a
// signal the decision engine would act on, and it would be a fabrication.
type FeatureRow struct {
	TS        time.Time
	ProductID string

	// Tier 1: funding and basis
	FundingRateHourly     decimal.NullDecimal
	FundingRateEst        decimal.NullDecimal
	FundingSource         string
	FundingAnnualized     decimal.NullDecimal
	FundingZScore         decimal.NullDecimal
	CumulativeFunding     decimal.NullDecimal
	ExpectedCarryNHours   decimal.NullDecimal
	FuturesMark           decimal.NullDecimal
	SpotMark              decimal.NullDecimal
	Basis                 decimal.NullDecimal
	TradePremium          decimal.NullDecimal
	TimeToNextFundingSecs *int32

	// Tier 2: microstructure
	SpreadBps       decimal.NullDecimal
	ToBImbalance    decimal.NullDecimal
	ImpactImbalance decimal.NullDecimal
	TradeImbalance  decimal.NullDecimal
	SweepIntensity  decimal.NullDecimal
	MidMarkDist     decimal.NullDecimal
	SlippageEstBps  decimal.NullDecimal

	// Tier 3: price
	LogRet1m      decimal.NullDecimal
	EMAFast       decimal.NullDecimal
	EMASlow       decimal.NullDecimal
	ATR           decimal.NullDecimal
	RealizedVol   decimal.NullDecimal
	VWAP          decimal.NullDecimal
	MomentumSlope decimal.NullDecimal

	// Risk
	MarginRatio         decimal.NullDecimal
	MarginRatioAtTarget decimal.NullDecimal
	EffectiveLeverage   decimal.NullDecimal
	NetDelta            decimal.NullDecimal
	ResidualDelta       decimal.NullDecimal
	ContractsHeld       *int64
	Notional            decimal.NullDecimal

	// Pressure (spec section 6.4)
	CrowdedSide    string // LONG | SHORT | NONE
	PressureLevel  string // NORMAL | ELEVATED | EXTREME | FORCED
	ExpectedPain   decimal.NullDecimal
	ExhaustionFlag *bool
}

func (r FeatureRow) row() rowData {
	return rowData{
		table: "cb_features",
		columns: []string{
			"ts", "product_id",
			"funding_rate_hourly", "funding_rate_est", "funding_source",
			"funding_annualized", "funding_zscore", "cumulative_funding",
			"expected_carry_n_hours", "futures_mark", "spot_mark", "basis",
			"trade_premium", "time_to_next_funding_secs",
			"spread_bps", "tob_imbalance", "impact_imbalance", "trade_imbalance",
			"sweep_intensity", "mid_mark_dist", "slippage_est_bps",
			"log_ret_1m", "ema_fast", "ema_slow", "atr", "realized_vol", "vwap",
			"momentum_slope",
			"margin_ratio", "margin_ratio_at_target", "effective_leverage",
			"net_delta", "residual_delta", "contracts_held", "notional",
			"crowded_side", "pressure_level", "expected_pain", "exhaustion_flag",
		},
		values: []any{
			r.TS, r.ProductID,
			r.FundingRateHourly, r.FundingRateEst, text(r.FundingSource),
			r.FundingAnnualized, r.FundingZScore, r.CumulativeFunding,
			r.ExpectedCarryNHours, r.FuturesMark, r.SpotMark, r.Basis,
			r.TradePremium, r.TimeToNextFundingSecs,
			r.SpreadBps, r.ToBImbalance, r.ImpactImbalance, r.TradeImbalance,
			r.SweepIntensity, r.MidMarkDist, r.SlippageEstBps,
			r.LogRet1m, r.EMAFast, r.EMASlow, r.ATR, r.RealizedVol, r.VWAP,
			r.MomentumSlope,
			r.MarginRatio, r.MarginRatioAtTarget, r.EffectiveLeverage,
			r.NetDelta, r.ResidualDelta, r.ContractsHeld, r.Notional,
			text(r.CrowdedSide), text(r.PressureLevel), r.ExpectedPain,
			r.ExhaustionFlag,
		},
		conflict: "ON CONFLICT (product_id, ts) DO NOTHING",
	}
}

// ---------------------------------------------------------------------------
// State tables (API spec section 5.2) — written by carry
// ---------------------------------------------------------------------------

// ProductRow is the venue's product metadata. It upserts: the REST poller
// rewrites the row every time it reads the products endpoint, and the seed at
// migrate time is the same statement with everything but the contract size left
// NULL.
type ProductRow struct {
	ProductID            string
	ContractSize         decimal.NullDecimal
	Tick                 decimal.NullDecimal
	TickValue            decimal.NullDecimal
	MaxLeverageOvernight decimal.NullDecimal
	MaxLeverageIntraday  decimal.NullDecimal
	MakerFeeBps          decimal.NullDecimal
	TakerFeeBps          decimal.NullDecimal
	Status               string
	UpdatedAt            time.Time
}

func (r ProductRow) row() rowData {
	return rowData{
		table: "cb_products",
		columns: []string{
			"product_id", "contract_size", "tick", "tick_value",
			"max_leverage_overnight", "max_leverage_intraday",
			"maker_fee_bps", "taker_fee_bps", "status", "updated_at",
		},
		values: []any{
			r.ProductID, r.ContractSize, r.Tick, r.TickValue,
			r.MaxLeverageOvernight, r.MaxLeverageIntraday,
			r.MakerFeeBps, r.TakerFeeBps, text(r.Status), r.UpdatedAt,
		},
		conflict: `ON CONFLICT (product_id) DO UPDATE SET
			contract_size = EXCLUDED.contract_size,
			tick = EXCLUDED.tick,
			tick_value = EXCLUDED.tick_value,
			max_leverage_overnight = EXCLUDED.max_leverage_overnight,
			max_leverage_intraday = EXCLUDED.max_leverage_intraday,
			maker_fee_bps = EXCLUDED.maker_fee_bps,
			taker_fee_bps = EXCLUDED.taker_fee_bps,
			status = EXCLUDED.status,
			updated_at = EXCLUDED.updated_at`,
	}
}

// DecisionRow is one emitted TargetPosition, persisted before it is acted on.
// InputSnapshot carries the whole feature set the decision was made from, so the
// decision can be re-derived from storage rather than trusted.
type DecisionRow struct {
	TS              time.Time
	State           string // ENTER | HOLD | REBALANCE | EXIT | BLOCKED
	TargetSpot      decimal.NullDecimal
	TargetContracts *int64
	ResidualDelta   decimal.NullDecimal
	ReasonCodes     []string
	Confidence      decimal.NullDecimal
	InputSnapshot   Snapshot
}

func (r DecisionRow) row() rowData {
	// reason_codes is NOT NULL: a decision with no reason is not explainable,
	// and an empty array says that explicitly where a NULL would not.
	codes := r.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	return rowData{
		table: "decisions",
		columns: []string{
			"ts", "state", "target_spot", "target_contracts", "residual_delta",
			"reason_codes", "confidence", "input_snapshot",
		},
		values: []any{
			r.TS, r.State, r.TargetSpot, r.TargetContracts, r.ResidualDelta,
			codes, r.Confidence, r.InputSnapshot,
		},
		conflict: "ON CONFLICT (ts) DO NOTHING",
	}
}

// PositionRow is one carry, bucketed by venue so paper, sim and live P&L never
// mix.
//
// ID is minted by the component that opens the position, not by the database.
// A carry has no property that identifies it — its open time is a fact about it,
// not its identity, and venue, size and price all repeat — so identity here is
// assigned rather than discovered. A ULID (the convention ClOrdID already uses)
// is known before the insert, which is what lets fills and funding events
// reference a position without anyone reading an id back out of the database:
// a read-back from a producer would be a second writer.
type PositionRow struct {
	ID                       string
	OpenedAt                 time.Time
	ClosedAt                 time.Time
	Venue                    string // VenuePaper | VenueSim | VenueLive
	SpotQty                  decimal.Decimal
	PerpContracts            int64
	AvgSpotPx                decimal.NullDecimal
	AvgPerpPx                decimal.NullDecimal
	AccruedFunding           decimal.Decimal
	SettlementPendingFunding decimal.Decimal
	Fees                     decimal.Decimal
	Slippage                 decimal.Decimal
	RealizedPnL              decimal.NullDecimal
	Status                   string
}

func (r PositionRow) row() rowData {
	return rowData{
		table: "positions",
		columns: []string{
			"id", "opened_at", "closed_at", "venue", "spot_qty", "perp_contracts",
			"avg_spot_px", "avg_perp_px", "accrued_funding",
			"settlement_pending_funding", "fees", "slippage", "realized_pnl",
			"status",
		},
		values: []any{
			r.ID, r.OpenedAt, stamp(r.ClosedAt), r.Venue, r.SpotQty, r.PerpContracts,
			r.AvgSpotPx, r.AvgPerpPx, r.AccruedFunding,
			r.SettlementPendingFunding, r.Fees, r.Slippage, r.RealizedPnL,
			r.Status,
		},
		conflict: "ON CONFLICT (id) DO NOTHING",
	}
}

// FillRow is one execution report that moved quantity. Replaying a report after
// a FIX resend inserts nothing new, because the venue's own execution id is
// unique.
type FillRow struct {
	TS         time.Time
	PositionID *string
	ClOrdID    string
	Venue      string
	Leg        string // spot | perp
	Side       string // buy | sell
	Qty        decimal.Decimal
	Px         decimal.Decimal
	Fee        decimal.NullDecimal
	ExecState  string // NEW | PARTIAL | FILLED | CANCELED | REJECTED
	// VenueExecID is the venue's own id for this execution, and the identity of
	// the row: FIX ExecID (tag 17) live and on sim, a minted id on paper. It is
	// required, which is what makes replaying an ExecutionReport a no-op.
	VenueExecID string
	Raw         Snapshot
}

func (r FillRow) row() rowData {
	return rowData{
		table: "fills",
		columns: []string{
			"ts", "position_id", "cl_ord_id", "venue", "leg", "side", "qty",
			"px", "fee", "exec_state", "venue_exec_id", "raw",
		},
		values: []any{
			r.TS, r.PositionID, r.ClOrdID, r.Venue, r.Leg, r.Side, r.Qty,
			r.Px, r.Fee, r.ExecState, r.VenueExecID, r.Raw,
		},
		conflict: "ON CONFLICT (venue, venue_exec_id) DO NOTHING",
	}
}

// FundingEventRow is one side of funding: an hourly ACCRUAL the system computed,
// or a SETTLEMENT cash adjustment observed on the account. Rows with no position
// double as the observed funding series, and re-running the history backfill
// inserts nothing new.
type FundingEventRow struct {
	// TS is the start of the funding hour for an ACCRUAL — the rate is a
	// property of the hour, not of the moment it was computed — and the time the
	// adjustment was observed for a SETTLEMENT.
	TS            time.Time
	ProductID     string
	Kind          string // FundingKindAccrual | FundingKindSettlement
	RateHourly    decimal.NullDecimal
	FundingSource string
	PositionID    *string
	Amount        decimal.Decimal // signed: positive = received
	SettledBy     *int64
	SpotMark      decimal.NullDecimal
	Contracts     *int64
}

func (r FundingEventRow) row() rowData {
	return rowData{
		table: "funding_events",
		columns: []string{
			"ts", "product_id", "kind", "rate_hourly", "funding_source",
			"position_id", "amount", "settled_by", "spot_mark", "contracts",
		},
		values: []any{
			r.TS, r.ProductID, r.Kind, r.RateHourly, text(r.FundingSource),
			r.PositionID, r.Amount, r.SettledBy, r.SpotMark, r.Contracts,
		},
		conflict: "ON CONFLICT (product_id, kind, ts, position_id) DO NOTHING",
	}
}

// RiskEventRow records a hard stop, a staleness trip, a reconciliation
// divergence or a kill-switch engagement, with the state that caused it.
type RiskEventRow struct {
	TS          time.Time
	Kind        string
	Detail      Snapshot
	ActionTaken string
	ResolvedAt  time.Time
}

func (r RiskEventRow) row() rowData {
	return rowData{
		table:    "risk_events",
		columns:  []string{"ts", "kind", "detail", "action_taken", "resolved_at"},
		values:   []any{r.TS, r.Kind, r.Detail, text(r.ActionTaken), stamp(r.ResolvedAt)},
		conflict: "ON CONFLICT (ts, kind) DO NOTHING",
	}
}

// FIXSessionRow is the operational record of a FIX session. quickfixgo's file
// store holds the authoritative sequence numbers; these rows are what the
// dashboard and the resend demo read.
type FIXSessionRow struct {
	SessionID   string
	StartedAt   time.Time
	EndedAt     time.Time
	LastInSeq   *int32
	LastOutSeq  *int32
	Disconnects int32
}

func (r FIXSessionRow) row() rowData {
	return rowData{
		table: "fix_sessions",
		columns: []string{
			"session_id", "started_at", "ended_at", "last_in_seq",
			"last_out_seq", "disconnects",
		},
		values: []any{
			r.SessionID, r.StartedAt, stamp(r.EndedAt), r.LastInSeq,
			r.LastOutSeq, r.Disconnects,
		},
		conflict: "ON CONFLICT (session_id, started_at) DO NOTHING",
	}
}
