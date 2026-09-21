package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/exec"
	"github.com/Jason-Dorman/funding-carry/internal/fix"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
)

// trade wires the order-entry stack and blocks until it has stopped. Read it
// backwards and it is the shutdown order from architecture section 8 as far
// as Part 8 builds it: the context is cancelled, the FIX session logs out,
// the report drain ends when the initiator closes the channel behind it, the
// gauge watcher takes its last reading, and the metrics endpoint stops last.
//
// The drain OUTLIVES the logout rather than preceding it — the initiator
// closes the report channel in a defer that runs after Stop returns — and
// that ordering is exactly what the Part 8 report-loss defect turned on, so
// this list is kept in the order the code actually does it. An earlier
// version had these two the other way round; found by the Part 8 follow-up
// adversarial review.
//
// What is deliberately not here yet: cancelling open orders on shutdown. That
// step belongs to the router and the risk engine (Part 15), and an order left
// working across a restart is exactly the case the file-backed sequence store
// exists for — the fill that prints while carry is down is recovered by resend
// when it comes back, and the tracker adopts it.
func trade(ctx context.Context, cfg *config.Carry, probe *probeOrder, log *slog.Logger) error {
	srv := metrics.NewServer(cfg.MetricsAddr, log)

	// The metrics endpoint outlives the root context, the same way ingest's
	// and sim-venue's do: the last reports arrive during the logout, and an
	// endpoint that stopped with everything else would make them unobservable
	// in the one scrape where it matters.
	metricsCtx, stopMetrics := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMetrics()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- srv.Serve(metricsCtx) }()

	venue, err := fix.NewInitiator(fix.InitiatorOptions{
		Product: cfg.PerpProductID,
		Session: cfg.FIX,
	}, fix.NewSessionCatalogue(srv.Registry()), log)
	if err != nil {
		stopMetrics()
		return errors.Join(err, <-metricsErr)
	}
	tracker := exec.NewTracker(exec.Options{
		Venue:   db.VenueSim,
		Timeout: cfg.Execution.OrderTimeout,
	}, exec.NewMetrics(srv.Registry()), log)

	venueCtx, stopVenue := context.WithCancel(ctx)
	defer stopVenue()
	venueErr := make(chan error, 1)
	go func() { venueErr <- venue.Run(venueCtx) }()

	// The drain is the consumer the venue's contract requires: every report is
	// taken off the channel and sequenced by the tracker until the venue
	// closes it, which it does only after logging out. Nothing else reads the
	// channel.
	var observe func(exec.Status, exec.Outcome)
	if probe != nil {
		observe = probe.observe
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drain(venue, tracker, observe)
	}()

	// The open/overdue gauges need a clock the tracker deliberately lacks, so
	// the binary supplies one: every second, on a context of its own.
	//
	// Not the venue's context, because the drain outlives the venue. Reports
	// arriving during the logout move carry_orders_open through Apply, while
	// only this watcher moves carry_orders_overdue — so a watcher stopped with
	// the venue would leave the final scrape, the one the metrics endpoint is
	// deliberately kept alive for, reading carry_orders_overdue=1 beside
	// carry_orders_open=0. Overdue orders are a subset of open ones; that pair
	// cannot happen in the tracker and would fire the alert in architecture
	// section 8 with no order behind it. Found by the Part 8 follow-up
	// adversarial review.
	watchCtx, stopWatch := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWatch()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watch(watchCtx, tracker)
	}()

	var probeErr error
	if probe != nil {
		probeErr = probe.run(ctx, venue, tracker, log)
		// The probe is the whole job: once it has its answer the process ends,
		// whichever way the answer went.
		stopVenue()
	}

	runErr := <-venueErr
	<-drained
	// The drain has finished, so the tracker cannot change again and the
	// watcher's last reading is the one the final scrape sees. Stopping it any
	// earlier is what produced the impossible gauge pair above — and stopping
	// it takes a cancel of its own, because venueCtx is not necessarily
	// cancelled when Run returns by itself (a store that could not be
	// created). Waiting on a watcher whose only stop signal was that context
	// is how the first version of this hung forever on exactly that path.
	stopWatch()
	<-watched
	stopMetrics()

	// Every failure is reported rather than the first one found.
	return errors.Join(probeErr, runErr, <-metricsErr)
}

// observeEvery is how often the open/overdue gauges are refreshed. A second
// is well inside ORDER_TIMEOUT, so an order going overdue is visible within a
// scrape or two of the deadline it missed.
const observeEvery = time.Second

// watch refreshes the tracker's gauges every observeEvery, and once more on
// the way out. That last refresh is what makes the final scrape coherent: the
// caller stops the watcher only after the report drain has finished, so the
// reading it leaves behind is the tracker's true final state rather than
// whatever the last tick happened to catch.
func watch(ctx context.Context, tracker *exec.Tracker) {
	tick := time.NewTicker(observeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			tracker.Observe(time.Now())
			return
		case now := <-tick.C:
			tracker.Observe(now)
		}
	}
}

// drain applies every report to the tracker and tells an observer, if there
// is one, what became of it.
func drain(venue carry.Venue, tracker *exec.Tracker, observe func(exec.Status, exec.Outcome)) {
	for r := range venue.ExecReports() {
		status, outcome := tracker.Apply(r)
		if observe != nil {
			observe(status, outcome)
		}
	}
}
