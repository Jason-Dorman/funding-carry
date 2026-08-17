// Package fix holds both ends of the FIX 4.4 session: the initiator used by carry
// and the acceptor used by sim-venue, over quickfixgo.
//
// Sequence numbers are file-backed and ResetOnLogon is off, because surviving a
// restart with resend recovery intact is the point of having a real FIX session
// rather than a REST client.
//
// Built in Parts 7 and 8.
package fix
