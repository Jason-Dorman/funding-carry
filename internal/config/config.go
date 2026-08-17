// Package config loads each binary's configuration from the environment into a
// typed struct, exactly once at startup.
//
// The variable catalogue is docs/api-spec.md section 7. Two rules shape this
// package:
//
//   - Money, sizes, thresholds and weights are decimal.Decimal, never float64
//     (spec section 11).
//   - Real thresholds and credentials live only in .env.private; everything
//     committed to this repo is a synthetic placeholder (spec section 9).
//     Credentials are typed as Secret so they cannot reach a log line.
//
// Configuration is immutable after load. The kill switch is the only value the
// running system may change, and it is owned by the risk engine at runtime, not
// re-read from here.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	// Embeds the IANA timezone database in the binary so MAINTENANCE_BREAK can be
	// resolved without tzdata installed in the runtime image.
	_ "time/tzdata"

	"github.com/shopspring/decimal"
)

// divisionPrecision is the number of digits decimal.Div keeps.
//
// Div is the only decimal operation that is not exact — Add, Sub and Mul are
// arbitrary precision — and the library controls it through a mutable
// package-level global that any dependency could change at any time. Pinning it
// here, in the package every binary loads first, makes division behave
// identically in every process, every test and every replay, which the
// reproducibility requirement depends on.
//
// It is a rounding budget for ratios (margin ratio, basis, premium), never for a
// value that has to reconcile against a venue statement. Money that must balance
// is added and multiplied, not divided.
const divisionPrecision = 16

func init() {
	decimal.DivisionPrecision = divisionPrecision

	// The second mutable global with a silent failure mode. When it is true, a
	// decimal marshals as a bare JSON number, and every consumer outside Go —
	// the Python backtester, jq, a browser reading a Grafana panel — parses that
	// number into a float64. Postgres keeps a jsonb number exact, so nothing
	// inside the system ever notices the loss. Decisions persist their whole
	// input snapshot as jsonb (API spec section 3.5), so this pin is what keeps
	// a recorded decision re-derivable; internal/db proves it round trips.
	decimal.MarshalJSONWithoutQuotes = false
}

// Secret is a configuration value that must never appear in a log line, an error
// message, or a %v of the struct that holds it. Both String and LogValue redact,
// so the only way to read one is Reveal at the point of use.
type Secret string

const redacted = "[REDACTED]"

func (s Secret) String() string { return redacted }

// LogValue implements slog.LogValuer, which is what keeps a Secret redacted when
// a config struct is logged as a structured attribute.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the underlying value. Every call site is a place to check that
// the secret is going to the venue and not to a log.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether the secret was supplied, without revealing it.
func (s Secret) IsSet() bool { return s != "" }

// Common is the configuration every binary needs.
type Common struct {
	DatabaseURL   string
	Asset         string
	PerpProductID string
	SpotProductID string
	MetricsAddr   string
	Log           LogConfig
}

// CoinbaseEndpoints are the Advanced Trade base URLs.
type CoinbaseEndpoints struct {
	APIURL string
	WSURL  string
}

// BaseEndpoints is the Base L2 read path (Alchemy JSON-RPC).
type BaseEndpoints struct {
	RPCURL     string
	AlchemyKey Secret
}

// PollIntervals are the ingest tickers.
type PollIntervals struct {
	REST     time.Duration
	Base     time.Duration
	BookSnap time.Duration
}

// FIXSession is the FIX 4.4 session identity shared by the initiator (carry) and
// the acceptor (sim-venue).
type FIXSession struct {
	Sender string
	Target string
	Host   string
	Port   int
}

// Signals are the decision-engine thresholds. Committed values are synthetic
// placeholders; the fitted ones live in .env.private.
type Signals struct {
	ZEnter          decimal.Decimal
	ZExit           decimal.Decimal
	FFlip           decimal.Decimal
	KCostMult       decimal.Decimal
	HorizonHours    int
	BasisExtreme    decimal.Decimal
	SpreadMaxBps    decimal.Decimal
	PressureWeights []decimal.Decimal // w1 z-score, w2 cumulative funding, w3 basis
}

// RiskLimits are the risk engine's bounds and the kill switch's initial state.
type RiskLimits struct {
	DeltaToleranceETH        decimal.Decimal
	MaxNotionalUSD           decimal.Decimal
	MaxLeverage              decimal.Decimal
	MarginRatioFloor         decimal.Decimal
	IntradayMarginOptIn      bool
	DailyLossLimitUSD        decimal.Decimal
	FundingReconToleranceUSD decimal.Decimal
	StaleFeed                time.Duration
	KillSwitch               bool
}

// TreasuryTimeouts bound the four tracked balance transitions (spec section 6.8).
type TreasuryTimeouts struct {
	Transfer   time.Duration // CBI -> CFM auto-transfer
	Sweep      time.Duration // CFM -> CBI sweep
	Settlement time.Duration // funding cash adjustment vs expected settlement
	BaseTx     time.Duration // Base tx submitted -> confirmed
}

// FillModel is sim-venue's fill behavior.
type FillModel struct {
	Latency          time.Duration
	SlippageBps      decimal.Decimal
	PartialThreshold decimal.Decimal
}

// Secrets are the values that exist only in .env.private. They are loaded but
// never required here: the public stack starts without them, and the parts that
// need them (16 and 17) assert their presence at the point of use.
type Secrets struct {
	CBAPIKeyName           Secret
	CBAPIPrivateKey        Secret
	CBPortfolioID          Secret
	WalletAddress          Secret
	SessionKey             Secret
	SessionKeyRouter       Secret
	SessionKeyExpiry       time.Time // zero when unset; Part 17 refuses to start on a past expiry
	SessionKeyAllowanceUSD decimal.Decimal
	GasPolicyID            Secret
}

// Ingest configures cmd/ingest: market data in, TimescaleDB out.
type Ingest struct {
	Common
	Coinbase    CoinbaseEndpoints
	Base        BaseEndpoints
	Poll        PollIntervals
	Maintenance MaintenanceWindow
	Secrets     Secrets
}

// Carry configures cmd/carry: features, pressure, decision, risk, execution.
type Carry struct {
	Common
	Coinbase        CoinbaseEndpoints
	Base            BaseEndpoints
	ContractSizeETH decimal.Decimal
	Signals         Signals
	Risk            RiskLimits
	Treasury        TreasuryTimeouts
	FIX             FIXSession
	Maintenance     MaintenanceWindow
	Secrets         Secrets
}

// MigrateCmd configures cmd/migrate, which applies the schema and seeds the
// product row. It is deliberately not built on Common: a schema migration has no
// venue endpoints, no metrics endpoint and no spot product, and requiring them
// would mean a migration could fail on configuration it never reads.
type MigrateCmd struct {
	DatabaseURL     string
	PerpProductID   string
	ContractSizeETH decimal.Decimal
	Log             LogConfig
}

// SimVenue configures cmd/sim-venue: the FIX 4.4 acceptor and its fill model.
type SimVenue struct {
	Common
	FIX  FIXSession
	Fill FillModel
}

// LoadIngest reads cmd/ingest's configuration from the process environment.
func LoadIngest() (*Ingest, error) { return loadIngest(os.LookupEnv) }

// LoadCarry reads cmd/carry's configuration from the process environment.
func LoadCarry() (*Carry, error) { return loadCarry(os.LookupEnv) }

// LoadSimVenue reads cmd/sim-venue's configuration from the process environment.
func LoadSimVenue() (*SimVenue, error) { return loadSimVenue(os.LookupEnv) }

// LoadMigrateCmd reads cmd/migrate's configuration from the process environment.
func LoadMigrateCmd() (*MigrateCmd, error) { return loadMigrateCmd(os.LookupEnv) }

func loadIngest(lookup lookupFunc) (*Ingest, error) {
	l := newLoader(lookup)
	cfg := &Ingest{
		Common:   l.common("INGEST_METRICS_ADDR", ":9101"),
		Coinbase: l.coinbase(),
		Base:     l.baseChain(),
		Poll: PollIntervals{
			REST:     l.Seconds("POLL_REST_SECS", 5*time.Second),
			Base:     l.Seconds("POLL_BASE_SECS", 30*time.Second),
			BookSnap: l.Seconds("BOOK_SNAP_SECS", 10*time.Second),
		},
		Maintenance: l.Window("MAINTENANCE_BREAK", defaultMaintenanceBreak),
		Secrets:     l.secrets(),
	}
	return cfg, l.err()
}

func loadCarry(lookup lookupFunc) (*Carry, error) {
	l := newLoader(lookup)
	cfg := &Carry{
		Common:          l.common("CARRY_METRICS_ADDR", ":9102"),
		Coinbase:        l.coinbase(),
		Base:            l.baseChain(),
		ContractSizeETH: l.Decimal("CONTRACT_SIZE_ETH", defaultContractSizeETH),
		Signals: Signals{
			ZEnter:          l.Decimal("Z_ENTER", "1.5"),
			ZExit:           l.Decimal("Z_EXIT", "0.5"),
			FFlip:           l.Decimal("F_FLIP", "0"),
			KCostMult:       l.Decimal("K_COST_MULT", "2"),
			HorizonHours:    l.PositiveInt("CARRY_HORIZON_HOURS", 168),
			BasisExtreme:    l.Decimal("BASIS_EXTREME", "0.007"),
			SpreadMaxBps:    l.Decimal("SPREAD_MAX_BPS", "5"),
			PressureWeights: l.DecimalList("PRESSURE_WEIGHTS", "0.5,0.3,0.2", pressureWeightCount),
		},
		Risk: RiskLimits{
			DeltaToleranceETH:        l.Decimal("DELTA_TOLERANCE_ETH", "0.05"),
			MaxNotionalUSD:           l.Decimal("MAX_NOTIONAL_USD", "500"),
			MaxLeverage:              l.Decimal("MAX_LEVERAGE", "3"),
			MarginRatioFloor:         l.Decimal("MARGIN_RATIO_FLOOR", "1.5"),
			IntradayMarginOptIn:      l.Bool("INTRADAY_MARGIN_OPT_IN", false),
			DailyLossLimitUSD:        l.Decimal("DAILY_LOSS_LIMIT_USD", "25"),
			FundingReconToleranceUSD: l.Decimal("FUNDING_RECON_TOLERANCE_USD", "0.50"),
			StaleFeed:                l.Seconds("STALE_FEED_SECS", 60*time.Second),
			KillSwitch:               l.Bool("KILL_SWITCH", false),
		},
		Treasury: TreasuryTimeouts{
			Transfer:   l.Duration("TREASURY_TRANSFER_TIMEOUT", 5*time.Minute),
			Sweep:      l.Duration("TREASURY_SWEEP_TIMEOUT", 24*time.Hour),
			Settlement: l.Duration("TREASURY_SETTLEMENT_TIMEOUT", 2*time.Hour),
			BaseTx:     l.Duration("TREASURY_BASE_TX_TIMEOUT", 10*time.Minute),
		},
		FIX:         l.fix(),
		Maintenance: l.Window("MAINTENANCE_BREAK", defaultMaintenanceBreak),
		Secrets:     l.secrets(),
	}
	// Parse errors are reported on their own: validating values that failed to
	// parse would report a second, misleading error for the same mistake.
	if err := l.err(); err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
}

func loadSimVenue(lookup lookupFunc) (*SimVenue, error) {
	l := newLoader(lookup)
	cfg := &SimVenue{
		Common: l.common("SIM_METRICS_ADDR", ":9103"),
		FIX:    l.fix(),
		Fill: FillModel{
			Latency:          time.Duration(l.PositiveInt("SIM_LATENCY_MS", 20)) * time.Millisecond,
			SlippageBps:      l.Decimal("SIM_SLIPPAGE_BPS", "2"),
			PartialThreshold: l.Decimal("SIM_PARTIAL_THRESHOLD", "10"),
		},
	}
	return cfg, l.err()
}

func loadMigrateCmd(lookup lookupFunc) (*MigrateCmd, error) {
	l := newLoader(lookup)
	cfg := &MigrateCmd{
		DatabaseURL:     l.Required("DATABASE_URL"),
		PerpProductID:   l.Required("PERP_PRODUCT_ID"),
		ContractSizeETH: l.Decimal("CONTRACT_SIZE_ETH", defaultContractSizeETH),
		Log:             l.logConfig(),
	}
	return cfg, l.err()
}

const (
	defaultMaintenanceBreak = "Fri 17:00-18:00 America/New_York"
	defaultContractSizeETH  = "0.10"
	pressureWeightCount     = 3
)

func (l *loader) common(metricsAddrKey, metricsAddrDefault string) Common {
	return Common{
		DatabaseURL:   l.Required("DATABASE_URL"),
		Asset:         l.String("ASSET", "ETH"),
		PerpProductID: l.Required("PERP_PRODUCT_ID"),
		SpotProductID: l.Required("SPOT_PRODUCT_ID"),
		MetricsAddr:   l.String(metricsAddrKey, metricsAddrDefault),
		Log:           l.logConfig(),
	}
}

func (l *loader) coinbase() CoinbaseEndpoints {
	return CoinbaseEndpoints{
		APIURL: l.String("CB_API_URL", "https://api.coinbase.com/api/v3/brokerage/"),
		WSURL:  l.String("CB_WS_URL", "wss://advanced-trade-ws.coinbase.com"),
	}
}

func (l *loader) baseChain() BaseEndpoints {
	return BaseEndpoints{
		RPCURL:     l.String("BASE_RPC_URL", ""),
		AlchemyKey: l.Secret("ALCHEMY_API_KEY"),
	}
}

func (l *loader) fix() FIXSession {
	return FIXSession{
		Sender: l.String("FIX_SENDER", "CARRY"),
		Target: l.String("FIX_TARGET", "SIMV"),
		Host:   l.String("FIX_HOST", "sim-venue"),
		Port:   l.PositiveInt("FIX_PORT", 5001),
	}
}

func (l *loader) secrets() Secrets {
	return Secrets{
		CBAPIKeyName:           l.Secret("CB_API_KEY_NAME"),
		CBAPIPrivateKey:        l.Secret("CB_API_PRIVATE_KEY"),
		CBPortfolioID:          l.Secret("CB_PORTFOLIO_ID"),
		WalletAddress:          l.Secret("WALLET_ADDRESS"),
		SessionKey:             l.Secret("SESSION_KEY"),
		SessionKeyRouter:       l.Secret("SESSION_KEY_ROUTER"),
		SessionKeyExpiry:       l.Time("SESSION_KEY_EXPIRY"),
		SessionKeyAllowanceUSD: l.Decimal("SESSION_KEY_ALLOWANCE_USD", "0"),
		GasPolicyID:            l.Secret("GAS_POLICY_ID"),
	}
}

// maxOvernightLeverage is the v1 ceiling from architecture section 12: leverage
// never exceeds 3x on overnight margin.
var maxOvernightLeverage = decimal.RequireFromString("3")

// two is used to express the "delta tolerance stays under half a contract"
// invariant without a float literal.
var two = decimal.NewFromInt(2)

// validate enforces the invariants that are cheaper to fail at startup than to
// discover at trade time. Each one is a documented rail, not a preference.
func (c *Carry) validate() error {
	var errs []error

	// Spec section 9 / architecture section 12: intraday margin is never opted
	// into in v1, so the intraday-to-overnight margin transition can never
	// trigger a call.
	if c.Risk.IntradayMarginOptIn {
		errs = append(errs, errors.New("INTRADAY_MARGIN_OPT_IN: must be false in v1"))
	}

	if c.Risk.MaxLeverage.LessThanOrEqual(decimal.Zero) || c.Risk.MaxLeverage.GreaterThan(maxOvernightLeverage) {
		errs = append(errs, fmt.Errorf("MAX_LEVERAGE=%s: must be greater than 0 and at most %s",
			c.Risk.MaxLeverage, maxOvernightLeverage))
	}

	// The perp leg only moves in whole contracts, so any residual delta the spot
	// leg has to absorb is under half a contract by construction. A tolerance
	// wider than that would accept delta the system could actually have closed.
	if c.ContractSizeETH.LessThanOrEqual(decimal.Zero) {
		errs = append(errs, fmt.Errorf("CONTRACT_SIZE_ETH=%s: must be greater than 0", c.ContractSizeETH))
	} else if halfContract := c.ContractSizeETH.Div(two); c.Risk.DeltaToleranceETH.GreaterThan(halfContract) {
		errs = append(errs, fmt.Errorf("DELTA_TOLERANCE_ETH=%s: exceeds half a contract (%s)",
			c.Risk.DeltaToleranceETH, halfContract))
	}

	return errors.Join(errs...)
}
