// Package risk is the only component permitted to emit orders. It holds position
// and delta state, runs pre-trade checks and hard stops, quantizes the perp leg
// to whole contracts, and owns the kill switch.
//
// It converts the decision engine's intent into approved order deltas, or blocks
// it with a reason code.
//
// Built in Part 13.
package risk
