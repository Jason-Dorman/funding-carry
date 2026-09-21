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
// Both this and the JSON pin below are set to the values shopspring already
// defaults to, which is worth saying plainly: neither assignment changes how the
// library behaves today. They are locks, not settings. The globals are mutable
// and package-scoped, so any dependency can move them at any time, and the two
// things they control — how a ratio rounds, and whether money serializes as a
// string — are both silent when they change. Pinning them here, in the package
// every binary loads first, is what makes the default a decision rather than an
// accident. TestDecimalGlobalsBehaveAsPinned checks the behaviour rather than
// the variables, because reading a global back and comparing it to the constant
// just assigned to it proves nothing.
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
// message, or a rendering of the struct that holds it (spec section 9).
//
// Redaction has to cover every verb a credential can escape through, because the
// gaps are not obvious and each one is silent. String and LogValue alone are not
// enough: slog resolves LogValuer only on the attribute value itself, so a Secret
// nested inside a struct falls through to encoding/json, and JSON is the logging
// format containers run with. GoString covers %#v, and MarshalText covers the
// encoders that reach for it before MarshalJSON. Every one of these is asserted
// in config_test.go — the guarantee is only worth what its test covers.
//
// Reveal is the sole way out.
type Secret string

const redacted = "[REDACTED]"

func (s Secret) String() string { return redacted }

// GoString redacts under %#v, which prints the underlying string rather than
// calling String.
func (s Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer, which is what keeps a Secret redacted when
// it is logged as an attribute value in its own right.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON is what keeps a Secret redacted when it is *not* the attribute
// value — nested in a config struct handed to slog's JSON handler, or passed to
// encoding/json directly.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText covers encoders that prefer TextMarshaler, and makes a Secret used
// as a JSON map key redact too.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

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

// BaseEndpoints is the Base L2 read path (Alchemy JSON-RPC) and the contracts
// it reads.
//
// The three addresses are configuration rather than constants because all three
// are chain-scoped: pointed at Base Sepolia, where the Base leg starts (spec
// section 1), the system needs a different USDC, a different WETH and a
// different pool. They are validated where they are parsed, in cmd/ingest, so a
// typo fails the startup rather than reading a wallet that holds nothing.
type BaseEndpoints struct {
	RPCURL     string
	AlchemyKey Secret
	USDC       string
	WETH       string
	SpotPool   string
}

// PollIntervals are the ingest tickers.
type PollIntervals struct {
	REST     time.Duration
	Base     time.Duration
	BookSnap time.Duration
}

// SimVenueCompID is the simulator's comp id, and in v1 the only FIX
// counterparty either binary will accept. FIX is sim-only by spec (section 12,
// item 3: FIX beyond sim-venue is a Part 21 question) and the live perp venue is
// REST, so a FIX session whose target calls itself anything else is a session
// pointed somewhere this system has no business sending an order. Both loads
// refuse it (Carry.validate, SimVenue.validate) — the PO's decision after the
// Part 8 review, so that the sim-only claim in the safety rails is enforced in
// code rather than resting on a configuration value.
const SimVenueCompID = "SIMV"

// FIXSession is the FIX 4.4 session identity shared by the initiator (carry) and
// the acceptor (sim-venue).
//
// StorePath is the directory quickfixgo keeps its sequence-number store and its
// message log under (one subdirectory each). It is configuration rather than a
// constant because it must outlive the process: ResetOnLogon is off, so a
// session that cannot find yesterday's sequence numbers is a session that
// silently starts again from one — which is the failure the file store exists to
// prevent, and it looks like a working system right up until the resend never
// comes. In Compose it points at a volume; on a developer machine it is a local
// directory; in tests it is a t.TempDir.
type FIXSession struct {
	Sender    string
	Target    string
	Host      string
	Port      int
	StorePath string
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

// Execution is carry's order-entry configuration.
//
// OrderTimeout is how long the venue has to acknowledge an order, measured
// from Submit. An unacknowledged order past it is overdue; an acknowledged one
// never is, however long it rests. On the second leg of an entry an overdue
// order is the trigger for unwinding the first (architecture section 6.4).
type Execution struct {
	OrderTimeout time.Duration
}

// FillModel is sim-venue's fill behavior (API spec section 4.3).
//
// Latency is the delay between an order becoming fillable and the fill printing,
// jittered; SlippageBps is how far through the touch a fill prints, always
// adverse to the order; an order larger than PartialThreshold prints in
// PartialSlices reports rather than one.
//
// BookPoll and BookMaxAge are the two that describe the market rather than the
// order. The simulator prices against the last top-of-book ingest recorded, so
// it re-reads that row every BookPoll, and refuses to accept an order when the
// newest one it can see is older than BookMaxAge. Without the second, a venue
// whose data feed had stopped would keep filling at yesterday's price and look
// entirely healthy doing it.
type FillModel struct {
	Latency          time.Duration
	SlippageBps      decimal.Decimal
	PartialThreshold decimal.Decimal
	PartialSlices    int
	BookPoll         time.Duration
	BookMaxAge       time.Duration
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
	Coinbase CoinbaseEndpoints
	Base     BaseEndpoints
	Poll     PollIntervals
	// Backfill is how far back to reconstruct history on startup. Zero disables
	// it. The default covers the thirty days a funding z-score needs plus a
	// margin, rather than the thirteen months the venue will serve: pulling a
	// year on every restart is a lot of requests for history that is already in
	// the database after the first run.
	Backfill    time.Duration
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
	Execution       Execution
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
		Backfill:    l.Duration("BACKFILL_WINDOW", defaultBackfillWindow),
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
		Execution: Execution{
			OrderTimeout: l.Duration("ORDER_TIMEOUT", defaultOrderTimeout),
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
			PartialSlices:    l.PositiveInt("SIM_PARTIAL_SLICES", 3),
			BookPoll:         l.Seconds("SIM_BOOK_POLL_SECS", time.Second),
			BookMaxAge:       l.Seconds("SIM_BOOK_MAX_AGE_SECS", 60*time.Second),
		},
	}
	if err := l.err(); err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
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

// Base mainnet defaults. Native USDC (not the bridged USDbC beside it), the
// canonical WETH, and the Uniswap v3 WETH/USDC 0.05% pool the spot price is read
// from. The pool is verified against the two token addresses on the first poll
// rather than trusted, so a wrong value here cannot become a price.
const (
	defaultBaseUSDC     = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	defaultBaseWETH     = "0x4200000000000000000000000000000000000006"
	defaultBaseSpotPool = "0xd0b53D9277642d899DF5C87A3966A349A798F224"
)

const (
	// defaultFIXStorePath is an absolute path because the containers run with no
	// working directory of their own: a relative default would resolve to / and
	// fail to create on an image whose user is not root. Compose mounts a volume
	// here, which is what carries the sequence numbers across a restart.
	defaultFIXStorePath = "/var/lib/carry/fix"

	// defaultOrderTimeout bounds how long the venue has to ACKNOWLEDGE an order
	// (PO decision after the Part 8 review: the timeout is about the
	// acknowledgement, never about the fill — a resting order is the order
	// working). Thirty seconds is a starting value, not a measured one — and
	// it is now measurable: carry_order_ack_seconds records the distribution
	// this number bounds (api-spec section 3.2, CHANGE-002).
	//
	// TODO(revisit): set this from the Part 15 soak's observed
	// carry_order_ack_seconds distribution, and record the value and its
	// evidence in the build plan. Until then it is a chosen number that is
	// finally checkable rather than a guess nobody can test.
	defaultOrderTimeout = 30 * time.Second

	defaultMaintenanceBreak = "Fri 17:00-18:00 America/New_York"
	defaultContractSizeETH  = "0.10"
	defaultBackfillWindow   = 45 * 24 * time.Hour
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
		USDC:       l.String("BASE_USDC_CONTRACT", defaultBaseUSDC),
		WETH:       l.String("BASE_WETH_CONTRACT", defaultBaseWETH),
		SpotPool:   l.String("BASE_SPOT_POOL", defaultBaseSpotPool),
	}
}

func (l *loader) fix() FIXSession {
	return FIXSession{
		Sender:    l.String("FIX_SENDER", "CARRY"),
		Target:    l.String("FIX_TARGET", "SIMV"),
		Host:      l.String("FIX_HOST", "sim-venue"),
		Port:      l.PositiveInt("FIX_PORT", 5001),
		StorePath: l.String("FIX_STORE_PATH", defaultFIXStorePath),
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

	// The order path is sim-only in v1, and this is where that stops being a
	// sentence in the safety rails and becomes a startup failure.
	if err := c.FIX.simOnly("FIX_TARGET", c.FIX.Target); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// simOnly refuses a FIX counterparty that is not the simulator.
func (FIXSession) simOnly(key, compID string) error {
	if compID != SimVenueCompID {
		return fmt.Errorf("%s=%q: the FIX session is sim-only in v1 and the simulator's comp id is %q "+
			"(spec section 12, FIX beyond sim-venue is a Part 21 question)", key, compID, SimVenueCompID)
	}
	return nil
}

// validate enforces the two fill-model invariants a zero or a negative would
// turn into silently wrong behavior rather than an error.
func (c *SimVenue) validate() error {
	var errs []error

	// Slippage is applied away from the order, so a negative value would print
	// fills *better* than the touch: a simulator that flatters every execution
	// is worse than no simulator, because the number it produces looks like
	// evidence. Zero is legitimate — it is the frictionless baseline a test
	// wants.
	if c.Fill.SlippageBps.IsNegative() {
		errs = append(errs, fmt.Errorf("SIM_SLIPPAGE_BPS=%s: must not be negative", c.Fill.SlippageBps))
	}

	// The threshold is the size above which an order prints in slices. At zero
	// or below, every order is oversized and the partial path becomes the only
	// path, which is not what any caller reading "orders larger than X" expects.
	if c.Fill.PartialThreshold.LessThanOrEqual(decimal.Zero) {
		errs = append(errs, fmt.Errorf("SIM_PARTIAL_THRESHOLD=%s: must be greater than 0",
			c.Fill.PartialThreshold))
	}

	// A book may not be declared stale sooner than it can be refreshed. The
	// simulator re-reads the recorded top-of-book every BookPoll and refuses
	// orders once the newest snapshot is older than BookMaxAge, so a maximum age
	// below the poll interval is a venue that spends part of every cycle
	// rejecting everything, for no reason an operator could see from either
	// value alone. Equal is allowed: that is a venue with no slack, which is a
	// choice rather than a mistake.
	if c.Fill.BookMaxAge < c.Fill.BookPoll {
		errs = append(errs, fmt.Errorf(
			"SIM_BOOK_MAX_AGE_SECS=%s: must not be shorter than SIM_BOOK_POLL_SECS=%s, "+
				"or the book is stale before it can be refreshed",
			c.Fill.BookMaxAge, c.Fill.BookPoll))
	}

	// The simulator's own comp id is FIX_TARGET (section 4.1: the variables are
	// named from the initiator's side). It answers to SIMV and nothing else, so
	// the two binaries agree on what a simulator is called by construction.
	if err := c.FIX.simOnly("FIX_TARGET", c.FIX.Target); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
