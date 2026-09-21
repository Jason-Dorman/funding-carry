package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/exec"
	"github.com/Jason-Dorman/funding-carry/internal/fix"
)

// TEMPORARY — Part 8's command-line trigger, removed when Part 15's router
// becomes the thing that submits orders.
//
// It exists so the acceptance criterion "carry sends NOS -> sim-venue ->
// ExecReports arrive on the channel, state machine terminal" can be
// demonstrated against the running stack rather than only in a test. It is an
// operator pressing a button, not code deciding to trade: only the risk engine
// emits orders (spec section 11), and the only venue this can reach is the
// FIX session to sim-venue.

// logonWait bounds how long the probe waits for the session to come up before
// giving up. The initiator reconnects every five seconds, so this is a few
// attempts.
const logonWait = 30 * time.Second

// probeOrder is one order to send, and the channel its reports come back on.
type probeOrder struct {
	order   carry.Order
	updates chan exec.Status
}

// parseProbe reads "side=sell,qty=1,px=2400.00,tif=GTC". tif defaults to GTC;
// everything else is required. The order is always a limit on the perp leg.
func parseProbe(spec string) (probeOrder, error) {
	o := carry.Order{
		ClOrdID: carry.NewOrderID(),
		Asset:   "ETH",
		Leg:     carry.LegPerp,
		Type:    carry.Limit,
		TIF:     carry.GTC,
	}
	for _, kv := range strings.Split(spec, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return probeOrder{}, fmt.Errorf("%q is not key=value", kv)
		}
		if err := setProbeField(&o, key, value); err != nil {
			return probeOrder{}, fmt.Errorf("%s: %w", key, err)
		}
	}
	if o.Side != carry.Buy && o.Side != carry.Sell {
		return probeOrder{}, fmt.Errorf("side must be buy or sell, got %q", o.Side)
	}
	if o.Qty.IsZero() || o.LimitPx.IsZero() {
		return probeOrder{}, errors.New("qty and px are required")
	}
	// Buffered so the drain never blocks on the probe: the probe reads only
	// while it is waiting, and a report that arrives after it has its answer
	// still has to be taken off the venue's channel.
	return probeOrder{order: o, updates: make(chan exec.Status, 64)}, nil
}

func setProbeField(o *carry.Order, key, value string) error {
	var err error
	switch key {
	case "side":
		o.Side = carry.Side(value)
	case "qty":
		o.Qty, err = decimal.NewFromString(value)
	case "px":
		o.LimitPx, err = decimal.NewFromString(value)
	case "tif":
		o.TIF = carry.TIF(value)
	default:
		return errors.New("unknown key")
	}
	return err
}

// observe is the drain's hook: the probe hears about every report.
func (p *probeOrder) observe(s exec.Status, _ exec.Outcome) {
	select {
	case p.updates <- s:
	default:
	}
}

// run sends the order and waits for it to end. If it is still working when
// the wait runs out, it is cancelled and the cancel is waited for. If the
// process is told to stop first, the order is left working at the venue on
// purpose: that is the restart case, and the report it produces while carry
// is down is what the next start recovers.
//
// The wait is ORDER_TIMEOUT, and that reuse is the probe's alone. ORDER_TIMEOUT
// bounds the ACKNOWLEDGEMENT for everything else in the system — an
// acknowledged order resting past it is the order working, and `Tracker.Overdue`
// says so (api-spec section 3.2). The probe is a one-shot CLI trigger that has
// to terminate, so it needs a bound on the FILL as well and has no other clock
// to take one from; it deliberately stops short of calling that "overdue",
// because the router will not behave this way. The Part 8 follow-up adversarial
// review found this file still using the word for the deleted meaning.
func (p *probeOrder) run(ctx context.Context, venue carry.Venue, tracker *exec.Tracker, log *slog.Logger) error {
	log = log.With("component", "probe", "cl_ord_id", p.order.ClOrdID)

	ack, err := p.submit(ctx, venue, log)
	if err != nil {
		return err
	}
	if err = tracker.Track(p.order, ack); err != nil {
		return err
	}
	log.Info("probe order sent", "side", string(p.order.Side), "qty", p.order.Qty.String(),
		"limit_px", p.order.LimitPx.String(), "tif", string(p.order.TIF))

	status, _ := tracker.Status(p.order.ClOrdID)
	timeout := status.Deadline.Sub(status.SubmittedAt)
	status, outcome := p.await(ctx, tracker, log, time.Until(status.Deadline))
	if outcome != waitUnfilled {
		return nil
	}

	// Still working: cancel, and give the venue's answer a window of its own,
	// the same length again.
	log.Warn("probe order still working when its wait ran out; cancelling",
		"state", string(status.State), "cum_qty", status.CumQty.String())
	if err = venue.Cancel(ctx, p.order.ClOrdID); err != nil {
		return fmt.Errorf("cancel probe order: %w", err)
	}
	if status, outcome = p.await(ctx, tracker, log, timeout); outcome == waitUnfilled {
		return fmt.Errorf("probe order %s still %s after the cancel timed out", p.order.ClOrdID, status.State)
	}
	return nil
}

// waitOutcome is how a wait on the order ended.
type waitOutcome int

const (
	// waitTerminal: the order ended.
	waitTerminal waitOutcome = iota
	// waitUnfilled: the window ran out with the order still working. NOT the
	// tracker's "overdue", which is an unacknowledged order (api-spec section
	// 3.2); this is the probe's own bound on the fill, and only the probe has
	// one.
	waitUnfilled
	// waitStopped: the process was told to stop. The order is left working
	// at the venue on purpose — the first live run of this probe went on to
	// cancel it on a context that was already dead, which is how this became
	// its own outcome rather than a flavour of the wait running out.
	waitStopped
)

// submit waits for the session to be up, then sends. The initiator refuses
// while it is down, so the wait is a retry loop over that refusal.
func (p *probeOrder) submit(ctx context.Context, venue carry.Venue, log *slog.Logger) (carry.Ack, error) {
	deadline := time.After(logonWait)
	retry := time.NewTicker(200 * time.Millisecond)
	defer retry.Stop()
	for {
		ack, err := venue.Submit(ctx, p.order)
		if err == nil {
			return ack, nil
		}
		if !errors.Is(err, fix.ErrSessionDown) {
			return carry.Ack{}, fmt.Errorf("submit probe order: %w", err)
		}
		select {
		case <-ctx.Done():
			return carry.Ack{}, ctx.Err()
		case <-deadline:
			return carry.Ack{}, fmt.Errorf("submit probe order: session not up after %s", logonWait)
		case <-retry.C:
			log.Debug("waiting for the fix session")
		}
	}
}

// await returns when the order is terminal, when the wait runs out, or when
// the process is told to stop.
func (p *probeOrder) await(ctx context.Context, tracker *exec.Tracker, log *slog.Logger, wait time.Duration) (exec.Status, waitOutcome) {
	status, _ := tracker.Status(p.order.ClOrdID)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if status.Terminal() {
			log.Info("probe order terminal", "state", string(status.State),
				"cum_qty", status.CumQty.String(), "avg_px", status.AvgPx.String(), "reason", status.Reason,
				"roundtrip", status.UpdatedAt.Sub(status.SubmittedAt).String())
			return status, waitTerminal
		}
		select {
		case <-ctx.Done():
			log.Warn("stopping with the probe order still working at the venue; a restart recovers its reports",
				"state", string(status.State))
			return status, waitStopped
		case <-timer.C:
			return status, waitUnfilled
		case s := <-p.updates:
			if s.ClOrdID == p.order.ClOrdID {
				status = s
			}
		}
	}
}
