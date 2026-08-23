package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// StatusHandler watches the perp's trading status and hands it to the
// venue-state sampler as the maintenance flag.
//
// It is subscribed for the perp only. The spot market does not close, so a
// status subscription for it would be a stream that never says anything and a
// staleness signal that could never be interpreted.
type StatusHandler struct {
	product string
	state   *VenueState
	log     *slog.Logger

	// last is the status most recently logged, so a channel that repeats itself
	// produces one line per change rather than one per message.
	last string
}

// NewStatusHandler builds the status handler for the perp product.
func NewStatusHandler(product string, state *VenueState, log *slog.Logger) *StatusHandler {
	return &StatusHandler{product: product, state: state, log: log.With("component", "status")}
}

// Subscribe names the channel and the product this handler wants.
func (h *StatusHandler) Subscribe() (string, []string) { return channelStatus, []string{h.product} }

// DataChannel is where status data arrives; here it matches the subscription.
func (h *StatusHandler) DataChannel() string { return channelStatus }

// Reset clears the last logged value so the status is logged again on the first
// message of a new connection. The sampler's copy is deliberately left alone:
// the market's state does not change because this process lost its socket, and
// clearing it would make every reconnect briefly un-assert the maintenance flag.
func (h *StatusHandler) Reset() { h.last = "" }

// Handle records whether the perp is tradable and logs each change.
func (h *StatusHandler) Handle(_ context.Context, msg Message) error {
	var events []statusEvent
	if err := json.Unmarshal(msg.Events, &events); err != nil {
		return fmt.Errorf("decode status events: %w", err)
	}

	for _, e := range events {
		for _, p := range e.Products {
			if p.ID != h.product {
				continue
			}
			if p.Status != h.last {
				h.log.Info("product status", "product", p.ID, "status", p.Status, "message", p.StatusMessage)
				h.last = p.Status
			}
			h.state.ObserveStatus(p.ID, p.Status == statusOnline)
		}
	}
	return nil
}
