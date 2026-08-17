package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// minimalEnv is the smallest environment that loads cleanly: the three required
// variables and nothing else, so every other value under test comes from a
// documented default.
func minimalEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":    "postgres://carry:carry@timescaledb:5432/carry?sslmode=disable",
		"PERP_PRODUCT_ID": "ETP-20DEC30-CDE",
		"SPOT_PRODUCT_ID": "ETH-USD",
	}
}

func lookupFrom(env map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func withEnv(overrides map[string]string) lookupFunc {
	env := minimalEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return lookupFrom(env)
}

func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()

	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

// loadErr adapts the three per-binary loaders to one signature so a table can
// exercise a variable through whichever binary actually reads it.
func loadErr[T any](load func(lookupFunc) (*T, error)) func(lookupFunc) error {
	return func(lookup lookupFunc) error {
		_, err := load(lookup)
		return err
	}
}

func TestLoadIngestDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadIngest(withEnv(nil))
	if err != nil {
		t.Fatalf("loadIngest: %v", err)
	}

	if got, want := cfg.Asset, "ETH"; got != want {
		t.Errorf("Asset = %q, want %q", got, want)
	}
	if got, want := cfg.MetricsAddr, ":9101"; got != want {
		t.Errorf("MetricsAddr = %q, want %q", got, want)
	}
	if got, want := cfg.Poll.REST, 5*time.Second; got != want {
		t.Errorf("Poll.REST = %v, want %v", got, want)
	}
	if got, want := cfg.Poll.Base, 30*time.Second; got != want {
		t.Errorf("Poll.Base = %v, want %v", got, want)
	}
	if got, want := cfg.Poll.BookSnap, 10*time.Second; got != want {
		t.Errorf("Poll.BookSnap = %v, want %v", got, want)
	}
	if got, want := cfg.Maintenance.Weekday, time.Friday; got != want {
		t.Errorf("Maintenance.Weekday = %v, want %v", got, want)
	}
	if got, want := cfg.Log.Format, FormatJSON; got != want {
		t.Errorf("Log.Format = %q, want %q", got, want)
	}
}

func TestLoadCarryDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadCarry(withEnv(nil))
	if err != nil {
		t.Fatalf("loadCarry: %v", err)
	}

	// Money and thresholds are decimal end to end, so expectations are written as
	// strings and compared with Equal — never as float literals.
	assertDecimal(t, "ContractSizeETH", cfg.ContractSizeETH, "0.10")
	assertDecimal(t, "Signals.ZEnter", cfg.Signals.ZEnter, "1.5")
	assertDecimal(t, "Risk.MarginRatioFloor", cfg.Risk.MarginRatioFloor, "1.5")
	assertDecimal(t, "Risk.MaxNotionalUSD", cfg.Risk.MaxNotionalUSD, "500")
	if got, want := cfg.Signals.HorizonHours, 168; got != want {
		t.Errorf("Signals.HorizonHours = %d, want %d", got, want)
	}
	if got, want := len(cfg.Signals.PressureWeights), pressureWeightCount; got != want {
		t.Fatalf("len(PressureWeights) = %d, want %d", got, want)
	}
	for i, want := range []string{"0.5", "0.3", "0.2"} {
		assertDecimal(t, fmt.Sprintf("PressureWeights[%d]", i), cfg.Signals.PressureWeights[i], want)
	}
	if cfg.Risk.KillSwitch {
		t.Error("Risk.KillSwitch = true, want false by default")
	}
	if cfg.Risk.IntradayMarginOptIn {
		t.Error("Risk.IntradayMarginOptIn = true, want false in v1")
	}
	if got, want := cfg.Treasury.Sweep, 24*time.Hour; got != want {
		t.Errorf("Treasury.Sweep = %v, want %v", got, want)
	}
	if got, want := cfg.Risk.StaleFeed, 60*time.Second; got != want {
		t.Errorf("Risk.StaleFeed = %v, want %v", got, want)
	}
}

func TestLoadSimVenueDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadSimVenue(withEnv(nil))
	if err != nil {
		t.Fatalf("loadSimVenue: %v", err)
	}

	if got, want := cfg.MetricsAddr, ":9103"; got != want {
		t.Errorf("MetricsAddr = %q, want %q", got, want)
	}
	if got, want := cfg.Fill.Latency, 20*time.Millisecond; got != want {
		t.Errorf("Fill.Latency = %v, want %v", got, want)
	}
	if got, want := cfg.FIX.Port, 5001; got != want {
		t.Errorf("FIX.Port = %d, want %d", got, want)
	}
}

func TestLoadReportsEveryMissingRequiredVariable(t *testing.T) {
	t.Parallel()

	_, err := loadCarry(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("loadCarry with an empty environment: want error, got nil")
	}

	// All three failures are reported together: a deployment should learn
	// everything that is wrong in one restart, not one variable per restart.
	for _, key := range []string{"DATABASE_URL", "PERP_PRODUCT_ID", "SPOT_PRODUCT_ID"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
	if !errors.Is(err, errRequired) {
		t.Errorf("error does not wrap errRequired:\n%v", err)
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	// Each case is exercised through the binary that actually reads the variable:
	// the poll intervals belong to ingest, the thresholds to carry, the fill model
	// to sim-venue.
	loadIngestErr := loadErr(loadIngest)
	loadCarryErr := loadErr(loadCarry)
	loadSimVenueErr := loadErr(loadSimVenue)

	tests := []struct {
		name    string
		load    func(lookupFunc) error
		env     map[string]string
		wantErr error
	}{
		{"non-integer seconds", loadIngestErr, map[string]string{"POLL_REST_SECS": "5s"}, errNotPositive},
		{"zero seconds", loadIngestErr, map[string]string{"POLL_REST_SECS": "0"}, errNotPositive},
		{"negative seconds", loadIngestErr, map[string]string{"POLL_BASE_SECS": "-1"}, errNotPositive},
		{"non-integer milliseconds", loadSimVenueErr, map[string]string{"SIM_LATENCY_MS": "20ms"}, errNotPositive},
		{"non-integer port", loadSimVenueErr, map[string]string{"FIX_PORT": "five thousand"}, errNotPositive},
		{"non-integer horizon", loadCarryErr, map[string]string{"CARRY_HORIZON_HOURS": "1 week"}, errNotPositive},
		{"non-decimal threshold", loadCarryErr, map[string]string{"CONTRACT_SIZE_ETH": "ten"}, errNotDecimal},
		{"non-decimal weight", loadCarryErr, map[string]string{"PRESSURE_WEIGHTS": "0.5,high,0.2"}, errNotDecimal},
		{"non-boolean flag", loadCarryErr, map[string]string{"KILL_SWITCH": "yes-please"}, errNotBool},
		{"non-duration timeout", loadCarryErr, map[string]string{"TREASURY_SWEEP_TIMEOUT": "24"}, errNotDuration},
		{"unknown log level", loadIngestErr, map[string]string{"LOG_LEVEL": "chatty"}, errUnknownLogLevel},
		{"unknown log format", loadIngestErr, map[string]string{"LOG_FORMAT": "xml"}, errUnknownLogFormat},
		{"session key expiry not RFC3339", loadCarryErr, map[string]string{"SESSION_KEY_EXPIRY": "2026-08-16"}, errNotTime},
		{"malformed maintenance window", loadIngestErr, map[string]string{"MAINTENANCE_BREAK": "Fri 17:00"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.load(withEnv(tt.env))
			if err == nil {
				t.Fatalf("want the load to fail, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want one wrapping %v", err, tt.wantErr)
			}
		})
	}
}

func TestPressureWeightsMustHaveThreeValues(t *testing.T) {
	t.Parallel()

	_, err := loadCarry(withEnv(map[string]string{"PRESSURE_WEIGHTS": "0.5,0.5"}))
	if err == nil {
		t.Fatal("two weights where three are expected: want error, got nil")
	}
	if !strings.Contains(err.Error(), "PRESSURE_WEIGHTS") {
		t.Errorf("error does not mention PRESSURE_WEIGHTS:\n%v", err)
	}
}

// The safety rails below are enforced at load rather than at trade time: a
// configuration that violates one of them must never start.
func TestCarryValidationRejectsUnsafeConfigurations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		wantText string
	}{
		{
			name:     "intraday margin opted in",
			env:      map[string]string{"INTRADAY_MARGIN_OPT_IN": "true"},
			wantText: "INTRADAY_MARGIN_OPT_IN",
		},
		{
			name:     "leverage above the 3x overnight cap",
			env:      map[string]string{"MAX_LEVERAGE": "5"},
			wantText: "MAX_LEVERAGE",
		},
		{
			name:     "leverage of zero",
			env:      map[string]string{"MAX_LEVERAGE": "0"},
			wantText: "MAX_LEVERAGE",
		},
		{
			name:     "delta tolerance wider than half a contract",
			env:      map[string]string{"DELTA_TOLERANCE_ETH": "0.06"},
			wantText: "DELTA_TOLERANCE_ETH",
		},
		{
			name:     "contract size of zero",
			env:      map[string]string{"CONTRACT_SIZE_ETH": "0"},
			wantText: "CONTRACT_SIZE_ETH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadCarry(withEnv(tt.env))
			if err == nil {
				t.Fatalf("want the load to fail, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error does not mention %s:\n%v", tt.wantText, err)
			}
		})
	}
}

func TestDeltaToleranceAtExactlyHalfAContractIsAccepted(t *testing.T) {
	t.Parallel()

	if _, err := loadCarry(withEnv(map[string]string{"DELTA_TOLERANCE_ETH": "0.05"})); err != nil {
		t.Fatalf("half a contract should be acceptable: %v", err)
	}
}

// A secret must not be readable through any of the paths a value normally leaks
// by: fmt verbs, error strings, or a structured log attribute.
func TestSecretsAreRedacted(t *testing.T) {
	t.Parallel()

	const real = "super-secret-cdp-key"
	cfg, err := loadCarry(withEnv(map[string]string{"CB_API_PRIVATE_KEY": real}))
	if err != nil {
		t.Fatalf("loadCarry: %v", err)
	}

	secret := cfg.Secrets.CBAPIPrivateKey
	if got := secret.Reveal(); got != real {
		t.Fatalf("Reveal() = %q, want %q", got, real)
	}
	if !secret.IsSet() {
		t.Error("IsSet() = false, want true")
	}

	// %v, %s and Sprint all route through String; the struct case is the one that
	// matters most, since that is how a whole config accidentally reaches a log.
	for name, rendered := range map[string]string{
		"%v":       fmt.Sprintf("%v", secret),
		"Sprint":   fmt.Sprint(secret),
		"String()": secret.String(),
		"struct":   fmt.Sprintf("%v", cfg.Secrets),
	} {
		if strings.Contains(rendered, real) {
			t.Errorf("%s leaked the secret: %s", name, rendered)
		}
		if !strings.Contains(rendered, redacted) {
			t.Errorf("%s = %s, want it to contain %s", name, rendered, redacted)
		}
	}

	var buf strings.Builder
	NewLogger(LogConfig{Level: slog.LevelInfo, Format: FormatJSON}, &buf).
		Info("starting", "key", secret)
	if strings.Contains(buf.String(), real) {
		t.Errorf("slog attribute leaked the secret: %s", buf.String())
	}
}

func TestSecretsAreOptional(t *testing.T) {
	t.Parallel()

	// The public stack must start with no .env.private at all; Parts 16 and 17
	// assert presence at the point of use.
	cfg, err := loadCarry(withEnv(nil))
	if err != nil {
		t.Fatalf("loadCarry without secrets: %v", err)
	}
	if cfg.Secrets.SessionKey.IsSet() {
		t.Error("SessionKey.IsSet() = true, want false when unset")
	}
	if !cfg.Secrets.SessionKeyExpiry.IsZero() {
		t.Error("SessionKeyExpiry should be the zero time when unset")
	}
}

func TestEmptyValueFallsBackToDefault(t *testing.T) {
	t.Parallel()

	// Compose materializes absent .env entries as empty strings, so empty has to
	// mean "unset" rather than "override with nothing".
	cfg, err := loadIngest(withEnv(map[string]string{"ASSET": ""}))
	if err != nil {
		t.Fatalf("loadIngest: %v", err)
	}
	if got, want := cfg.Asset, "ETH"; got != want {
		t.Errorf("Asset = %q, want the default %q", got, want)
	}
}

func TestLogLevelAndFormatAreConfigurable(t *testing.T) {
	t.Parallel()

	cfg, err := loadIngest(withEnv(map[string]string{"LOG_LEVEL": "debug", "LOG_FORMAT": "TEXT"}))
	if err != nil {
		t.Fatalf("loadIngest: %v", err)
	}
	if got, want := cfg.Log.Level, slog.LevelDebug; got != want {
		t.Errorf("Log.Level = %v, want %v", got, want)
	}
	if got, want := cfg.Log.Format, FormatText; got != want {
		t.Errorf("Log.Format = %q, want %q", got, want)
	}
}
