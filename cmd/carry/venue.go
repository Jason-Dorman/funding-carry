package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/exec"
	"github.com/Jason-Dorman/funding-carry/internal/fix"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
)

// trade wires the order-entry stack and blocks until it has stopped. Read it
// backwards and it is the shutdown order from architecture section 8 as far
// as Part 8 builds it: cancel, the report drain ends, the FIX session logs
// out, the metrics endpoint stops.
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

	var probeErr error
	if probe != nil {
		probeErr = probe.run(ctx, venue, tracker, log)
		// The probe is the whole job: once it has its answer the process ends,
		// whichever way the answer went.
		stopVenue()
	}

	runErr := <-venueErr
	<-drained
	stopMetrics()

	// Every failure is reported rather than the first one found.
	return errors.Join(probeErr, runErr, <-metricsErr)
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
