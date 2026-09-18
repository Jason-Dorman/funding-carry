// Package exec holds the order-state machine, the execution router, and the
// non-FIX Venue implementations: the paper engine and, from week 5, the live
// Coinbase and Base adapters.
//
// Every venue feeds the same ExecReport stream and the same state machine, so
// paper, sim and live accounting are identical by construction.
//
// Part 8 builds the state machine (statemachine.go): one Tracker per venue,
// fed every ExecReport that venue emits, sequencing them by CumQty and
// recognising the duplicates a FIX resend produces. Parts 14 to 17 add the
// router and the other venues.
package exec
