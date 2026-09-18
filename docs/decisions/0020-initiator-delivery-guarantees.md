# ADR-0020: The FIX initiator refuses orders while the session is down, never resends one, and acknowledges a report only once it is on the channel

**Date:** 2026-09-17 · **Status:** accepted, amended 2026-09-18 after the [Part 8 adversarial review](../reviews/part8-adversarial-findings.md) · **Build part:** P8 (binds P15, P16)

## Context

`fix.Initiator` is the first `carry.Venue`. quickfixgo's `SendToTarget` accepts a message whether or not the session is logged on: it assigns the next sequence number, writes the message to the file store, and — the send queue having been dropped on disconnect — leaves it for the *acceptor's* ResendRequest to recover on the next logon. Read from the venue's side, that is a NewOrderSingle arriving whenever the link comes back, at a price the decision engine chose before it went down. The same store mechanics work in carry's favour in the other direction: quickfix increments the inbound sequence number only after `FromApp` returns, so a report the application has not finished with is one the venue must resend.

Three questions had to be answered before the first order crossed the socket, and none of them is in the plan: what `Submit` does when the session is down, when a report counts as delivered, and what an OrderCancelReject means to the state machine.

## Decision

1. **`Submit` and `Cancel` refuse while the session is not logged on**, with `ErrSessionDown` and no message sent. The refusal is checked at the boundary and the window between the check and the send is real and small; the per-order timeout and the risk engine's leg rule are the backstop for an order that did go out into a dying link, not this check. An error from either method means nothing was sent.
2. **A report is handed to the channel before it is acknowledged to the venue, and the only way out is a timeout.** `FromApp` returns once the handoff is done; the consumer drains the channel until the venue closes it, which happens only after the Logout. The escape is a **wait that runs out** (30 s), not a signal that shutdown has begun — the distinction is the whole decision, and the first version of this ADR got it wrong. A signal that is already closed makes both cases of the select ready at once and Go then chooses at random, so the escape fires when the handoff would have succeeded immediately. A timeout cannot fire while the handoff would succeed. Abandonment therefore means one thing only: a consumer that has stopped draining, which is a broken caller rather than an ordinary shutdown.

   The guarantee reaches the channel, not the consumer: a report in the buffer has been acknowledged and will not be resent, so the buffer size is what a `kill -9` can destroy. 256 and 30 s are both chosen numbers, not measured ones, and are recorded as such.

3. **An order is never resent.** quickfix replays a message by calling `ToApp` again with `PossDupFlag` set; the initiator returns `ErrDoNotSend` there, so the message is gap-filled. A venue that has asked for a resend has lost its side of the session, and sim-venue keeps its book in memory with no ClOrdID history across a restart — a replayed `NewOrderSingle` is taken as a brand-new order and filled a second time. A gap fill says the sequence number was used and nothing more, which is exactly true: whatever that order was, it is no longer an instruction this process stands behind.

4. **A BusinessMessageReject is never answered with another.** Both ends refuse an unrouted application message with a 35=j, and a 35=j is itself an application message; rejecting one has the two engines rejecting each other's rejects until the connection dies. Both ends log it and accept it.
5. **An OrderCancelReject is logged and counted and does not touch the order.** The order's state is whatever the venue's ExecutionReports say; a cancel that arrived after the fill is answered by the `FILLED` that beat it, and a cancel for an order the venue never had is an operator's mistake, not an order event.
6. **The state machine adopts reports for orders it was never told about,** and *completes* them if their submitter catches up. After a restart the tracker is empty and the venue's book is not; a report for an order a previous process placed is a fact about a real order, and is applied under the outcome `adopted` rather than dropped. The same path covers a much more ordinary race: the venue's acknowledgement can cross the socket before `Submit` has returned to its caller, and the caller has no way to register the order sooner, because `Track` needs the `Ack` only `Submit` can produce. So `Track` for an already-adopted ClOrdID fills in what adoption could not know rather than refusing it. An adopted order has no ordered quantity, so the quantity guards do not apply to it, and no acknowledgement, so it contributes no round-trip sample.

## Alternatives considered

- **Let quickfix queue orders while down** — the library's default. Rejected: an order-entry gateway that is down refuses orders; the router decides what to do about the leg it could not place ([architecture §6.4](../architecture.md#64-live-carry-entry-week-5)).
- **Return an error from `FromApp` when the consumer is slow** — quickfix would send a Reject to the venue and still increment the sequence number, so the report would be lost with a message saying so. Blocking keeps it recoverable.
- **Close a shutdown channel and select on it** — what this ADR originally specified, and it was wrong for a reason worth keeping: an always-ready case in a `select` is not a fallback, it is a coin flip. Measured at 515 dropped out of 1000 handoffs that would all have succeeded.
- **Let quickfix replay orders on a resend, as the protocol's default does** — correct for a venue that keeps durable order state and wrong for one that does not. The initiator cannot tell which it is talking to, and the failure is a duplicate live position, so it declines for both.
- **Put a cancel reject on the report channel as a pseudo-state** — every consumer would then have to know a state that is not one of the five the schema checks, to learn something the next ExecutionReport already tells it.
- **Drop reports for unknown orders** — the safe-looking choice, and the one that would make a restart lose the fill the sequence store exists to preserve.

## Consequences

`cbVenue` (Part 16) inherits the same contract: kill switch or no session, `Submit` returns an error without network I/O — and inherits decisions 2 and 3, which is why they are worth getting right here against a simulator rather than there against real money. The router (Part 15) must drain the channel until it closes and must reload resting orders from the database, because `Cancel` can only address what this process submitted. `carry_exec_reports_total{outcome="adopted"}` rising after a restart is the recovery working; `stale`, `late` or `invalid` rising at any time is a venue contradicting itself.
