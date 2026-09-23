package venue

import (
	"context"
	"time"
)

// Run refreshes the cache once immediately and then on every tick until ctx
// is canceled. It returns nothing: a refresh that fails is logged and the
// loop carries on, because the failure has already been turned into ageing
// rows and, in time, stale flags (Refresh). The one thing that ends the loop
// is cancellation. This is the "feed errors degrade to staleness, never
// crash" rule, applied to the read side of the feed.
//
// Each refresh is bounded to one interval. Without that, a database that
// accepts a connection and never answers would hold the loop on a single
// query indefinitely — the flags would freeze at whatever they last read,
// which is the one state a staleness cache must not be able to enter.
//
// The immediate first refresh is what makes the state usable at startup
// rather than one interval later, and what makes a restart's first tick see
// current rows.
func (c *Cache) Run(ctx context.Context, every time.Duration) {
	ticks := c.opts.ticks
	if ticks == nil {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		ticks = ticker.C
	}

	var failing bool
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		err := c.Refresh(rctx)
		if ctx.Err() == nil {
			failing = c.note(err, failing)
		}
		if c.opts.onRefresh != nil {
			c.opts.onRefresh(c.Latest())
		}
	}

	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			refresh()
		}
	}
}

// note logs a refresh outcome on the transition, not on every tick: an outage
// should be one warning and one recovery, not a line every five seconds. The
// metrics carry the per-tick picture. It returns the new failing state.
func (c *Cache) note(err error, failing bool) bool {
	switch {
	case err != nil && !failing:
		c.log.Warn("venue state refresh failing; last known rows kept and ageing", "err", err)
		return true
	case err == nil && failing:
		c.log.Info("venue state refresh recovered")
		return false
	}
	return failing
}
