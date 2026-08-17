// Package exec holds the order-state machine, the execution router, and the
// non-FIX Venue implementations: the paper engine and, from week 5, the live
// Coinbase and Base adapters.
//
// Every venue feeds the same ExecReport stream and the same state machine, so
// paper, sim and live accounting are identical by construction.
//
// Built in Parts 8, 14, 15, 16 and 17.
package exec
