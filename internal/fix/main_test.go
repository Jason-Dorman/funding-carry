package fix

import (
	"testing"

	"go.uber.org/goleak"
)

// The acceptor runs an engine goroutine and quickfixgo runs several more per
// session, so goroutine leaks are checked for the package as a whole (testing
// strategy: goleak on anything with goroutines).
//
// Two of quickfixgo's own goroutines are ignored, and it is worth being exact
// about what they are rather than waving at them. Both are timers the library
// arms while waiting for a peer, and each fires into the session's event channel
// with a bare send:
//
//	time.AfterFunc(s.LogoutTimeout, func() { s.sessionEvent <- internal.LogoutTimeout })
//
// Once the session has stopped, nothing reads that channel again, so the timer's
// goroutine blocks on the send for the life of the process. The library knows
// the shape of this — two neighbouring sends in the same file carry a comment
// about the identical deadlock and are guarded with a select — these two are
// not. Both are in quickfixgo v0.9.11, on the far side of an API this package
// cannot reach.
//
// Which of the two applies to which side matters, and the first version of this
// comment got it wrong (corrected after the Part 7 adversarial review):
//
//   - initiateLogoutInReplyTo's timer is armed by whichever side sends a Logout,
//     so it is **sim-venue's**: one goroutine per session it logs out, which in
//     practice means one per shutdown and one per client disconnect.
//   - stateMachine.Connect's logon timer is armed only for initiators —
//     session_state.go returns early on `!session.InitiateLogon` — so an
//     acceptor never arms it. It appears in this package's test runs because the
//     scripted client IS an initiator, and it is what carry will leak once Part
//     8 gives it a FIX session.
//
// Ignored here rather than papered over: both are real, both are upstream, and
// both are visible in production through go_goroutines, which every binary
// already exports. A sim-venue whose client flaps would show it as a slow climb.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/quickfixgo/quickfix.(*session).initiateLogoutInReplyTo.func1"),
		goleak.IgnoreTopFunction("github.com/quickfixgo/quickfix.(*stateMachine).Connect.func1"),
	)
}
