package ingest

import (
	"math/rand/v2"
	"time"
)

// Backoff is the reconnect schedule: exponential growth to a ceiling, with
// jitter so that streams which drop together — every socket on this binary goes
// down when the network does — do not come back in lockstep and re-create the
// thundering herd the backoff exists to avoid.
type Backoff struct {
	Base time.Duration // delay after the first failure, before jitter
	Max  time.Duration // ceiling on the exponential term

	// Jitter spreads a computed delay. It is injected so the schedule is
	// deterministic under test; production uses equalJitter.
	Jitter func(d time.Duration) time.Duration
}

const (
	defaultBackoffBase = 500 * time.Millisecond
	defaultBackoffMax  = 30 * time.Second

	// maxShift bounds the exponent so 1 << attempt cannot overflow on a stream
	// that has been failing for a very long time. At the default base this
	// exceeds the ceiling many times over, so it changes no observable delay.
	maxShift = 32
)

func (b Backoff) withDefaults() Backoff {
	if b.Base <= 0 {
		b.Base = defaultBackoffBase
	}
	if b.Max <= 0 {
		b.Max = defaultBackoffMax
	}
	if b.Jitter == nil {
		b.Jitter = equalJitter
	}
	return b
}

// delay is the wait before reconnect attempt n (n >= 1).
func (b Backoff) delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > maxShift {
		shift = maxShift
	}
	d := b.Base << shift
	if d <= 0 || d > b.Max {
		d = b.Max
	}
	return b.Jitter(d)
}

// equalJitter returns a delay uniformly distributed over the top half of the
// window: half of d, plus a random share of the other half.
//
// The alternative — a delay uniform over the whole window — is the more commonly
// quoted "full jitter", and it is wrong here. It draws delays arbitrarily close
// to zero, so a venue refusing connections would be retried in a tight loop
// roughly as often as it was retried politely, which is precisely the behaviour
// the schedule exists to prevent. Keeping a floor costs nothing: the point of
// jitter is to decorrelate the streams from each other, not to make some of them
// fast.
//
// math/rand rather than crypto/rand: this is a scheduling nudge, not a secret.
func equalJitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half))) //nolint:gosec // scheduling jitter, not a security decision
}
