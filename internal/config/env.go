package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Parse failures are sentinel errors so tests can assert on the reason rather
// than on message text.
var (
	errRequired    = errors.New("required, not set")
	errNotInteger  = errors.New("not an integer")
	errNotPositive = errors.New("not a positive integer")
	errNotBool     = errors.New("not a boolean (true/false)")
	errNotDuration = errors.New("not a duration (e.g. 30s, 5m, 24h)")
	errNotDecimal  = errors.New("not a decimal number")
	errNotTime     = errors.New("not an RFC3339 timestamp")
)

// lookupFunc resolves one environment variable. Production passes os.LookupEnv;
// tests pass a map, so configuration tests never mutate process-global state and
// can run in parallel.
type lookupFunc func(key string) (value string, ok bool)

// loader reads typed values out of the environment, accumulating every failure
// instead of returning the first one. A misconfigured deployment then reports all
// of its problems in a single startup log line rather than one per restart.
type loader struct {
	lookup lookupFunc
	errs   []error
}

func newLoader(lookup lookupFunc) *loader {
	return &loader{lookup: lookup}
}

// err joins everything that went wrong, or returns nil if the load was clean.
func (l *loader) err() error {
	return errors.Join(l.errs...)
}

func (l *loader) reject(key, raw string, reason error) {
	l.errs = append(l.errs, fmt.Errorf("%s=%q: %w", key, raw, reason))
}

// value returns the raw string for key, falling back to def when the variable is
// unset or empty. Empty is treated as unset because Docker Compose materializes
// absent .env entries as empty strings.
func (l *loader) value(key, def string) string {
	if raw, ok := l.lookup(key); ok && raw != "" {
		return raw
	}
	return def
}

func (l *loader) String(key, def string) string {
	return l.value(key, def)
}

// Required is for values that have no sensible synthetic default — a wrong guess
// would point the system at the wrong database or the wrong product.
func (l *loader) Required(key string) string {
	raw := l.value(key, "")
	if raw == "" {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", key, errRequired))
	}
	return raw
}

// Secret reads a credential. Absent secrets are not an error here: the parts that
// need them (16, 17) assert their presence at the point of use, so the public
// stack still starts with an empty .env.private.
func (l *loader) Secret(key string) Secret {
	return Secret(l.value(key, ""))
}

func (l *loader) Bool(key string, def bool) bool {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.reject(key, raw, errNotBool)
		return def
	}
	return v
}

func (l *loader) Int(key string, def int) int {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.reject(key, raw, errNotInteger)
		return def
	}
	return v
}

// PositiveInt rejects zero and negatives — used for horizons, ports and counts
// where zero is a configuration mistake rather than a meaningful setting.
func (l *loader) PositiveInt(key string, def int) int {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		l.reject(key, raw, errNotPositive)
		return def
	}
	return v
}

// Seconds reads the `*_SECS` variables, which the API spec expresses as bare
// integer seconds rather than Go duration strings.
func (l *loader) Seconds(key string, def time.Duration) time.Duration {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		l.reject(key, raw, errNotPositive)
		return def
	}
	return time.Duration(v) * time.Second
}

// Duration reads the treasury timeouts, which the API spec expresses as Go
// duration strings (`5m`, `24h`).
func (l *loader) Duration(key string, def time.Duration) time.Duration {
	raw := l.value(key, "")
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		l.reject(key, raw, errNotDuration)
		return def
	}
	return v
}

// Decimal is how every price, size, threshold and weight is read: money and the
// numbers compared against money are decimal end to end, never float.
func (l *loader) Decimal(key, def string) decimal.Decimal {
	raw := l.value(key, def)
	v, err := decimal.NewFromString(raw)
	if err != nil {
		l.reject(key, raw, errNotDecimal)
		return decimal.Zero
	}
	return v
}

// DecimalList reads a fixed-length comma-separated list, e.g. PRESSURE_WEIGHTS.
// The length is part of the contract: two weights where three are expected is a
// silent behavior change, so it fails the load.
func (l *loader) DecimalList(key, def string, want int) []decimal.Decimal {
	raw := l.value(key, def)
	parts := strings.Split(raw, ",")
	if len(parts) != want {
		l.reject(key, raw, fmt.Errorf("expected %d comma-separated values, got %d", want, len(parts)))
		return nil
	}
	out := make([]decimal.Decimal, 0, want)
	for _, p := range parts {
		v, err := decimal.NewFromString(strings.TrimSpace(p))
		if err != nil {
			l.reject(key, raw, errNotDecimal)
			return nil
		}
		out = append(out, v)
	}
	return out
}

// Time reads an optional RFC3339 instant. The zero time means "unset"; callers
// that require it (the session-key preflight in Part 17) check for that.
func (l *loader) Time(key string) time.Time {
	raw := l.value(key, "")
	if raw == "" {
		return time.Time{}
	}
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		l.reject(key, raw, errNotTime)
		return time.Time{}
	}
	return v
}

// Window reads the maintenance break, e.g. `Fri 17:00-18:00 America/New_York`.
func (l *loader) Window(key, def string) MaintenanceWindow {
	raw := l.value(key, def)
	w, err := ParseMaintenanceWindow(raw)
	if err != nil {
		l.reject(key, raw, err)
		return MaintenanceWindow{}
	}
	return w
}
