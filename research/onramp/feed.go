package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"
)

// tick is one message from the fake feed: the fields a real ticker message
// carries, minus everything Part 4 will have to parse off the wire.
type tick struct {
	product string
	seq     int64
	price   decimal.Decimal
	at      time.Time
}

// feed is one product's fake WebSocket stream. Two of these run concurrently
// and send into the same channel; that fan-in is what the exercise is about.
type feed struct {
	product  string
	interval time.Duration
	base     decimal.Decimal
	step     decimal.Decimal
	m        *feedMetrics
	log      *slog.Logger
}

// walkPeriod is the length of the price sawtooth in ticks: eleven steps up from
// the base, ten back down.
const walkPeriod = 21

// walk is the price path. It is a deterministic sawtooth rather than a random
// walk so two runs of the binary produce the same rows, which is what makes the
// row counts in the README repeatable (testing strategy: determinism).
//
// The arithmetic is decimal end to end. A float here would be the easiest place
// in the repo to break the no-float-money rule, and it would be invisible: the
// column is numeric, so the error would only show up as cents that do not add
// up weeks later.
func (f *feed) walk(seq int64) decimal.Decimal {
	offset := seq%walkPeriod - 10
	return f.base.Add(f.step.Mul(decimal.NewFromInt(offset)))
}

// run emits ticks until ctx is canceled.
//
// It returns nothing on purpose. A fake feed has no failure mode, and a real one
// would degrade to staleness rather than stop the binary — feed errors are the
// one place the project allows a loop to carry on (CLAUDE.md engineering rules).
func (f *feed) run(ctx context.Context, out chan<- tick) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	var sent int64
	defer func() { f.log.Info("feed stopped", "product", f.product, "ticks", sent) }()

	for seq := int64(1); ; seq++ {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			t := tick{product: f.product, seq: seq, price: f.walk(seq), at: now.UTC()}

			// The send gets its own select. The writer may be mid-flush and the
			// queue may be full, and a producer parked on a full queue that
			// cannot observe cancellation is a shutdown that never finishes.
			select {
			case out <- t:
				sent++
				f.m.produced(f.product)
			case <-ctx.Done():
				return
			}
		}
	}
}
