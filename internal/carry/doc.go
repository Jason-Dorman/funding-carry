// Package carry owns the decision engine and the Venue interface.
//
// The interface lives here, at the consumer, not next to any implementation
// (spec section 11) — carry is what needs a venue, so carry defines what a venue
// is, and fix, paper, cbVenue and baseVenue all conform to it.
//
// The decision engine emits intent (TargetPosition) and nothing else: only the
// risk engine may emit orders.
//
// Built in Parts 8 and 12.
package carry
