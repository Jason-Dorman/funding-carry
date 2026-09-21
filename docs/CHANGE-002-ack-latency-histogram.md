# CHANGE-002: Add `carry_order_ack_seconds` histogram, close the open timeout item

**Applies to:** `internal/exec` (metrics and the order state machine), plus every document that described the acknowledgement timeout as un-measurable: [api-spec](api-spec.md) §3.2/§6/§7, [build-plan](build-plan.md) Part 8 and Part 15, the [Part 8 follow-up review](reviews/part8-followup-adversarial-findings.md), `.env.example`, and [CHANGELOG](../CHANGELOG.md).
**Type:** additive observability. No control-flow change to the acknowledgement or timeout logic, and the 30 s timeout value is unchanged.
**Status:** applied 2026-09-21 on `feature/fix-initiator`, in the same commit as the documents it updates. **Revised the same day** after PO review of the first implementation — see §5.

---

## 1. The decision

The 30 s order-acknowledgement timeout was chosen without measurement. The [Part 8 follow-up adversarial review](reviews/part8-followup-adversarial-findings.md) established that the two gauges introduced to settle it — `carry_orders_open` and `carry_orders_overdue` — **cannot**: Prometheus scrapes every 15 s, a marginally late acknowledgement raises the gauge for a second or two and is usually invisible, and a gauge carries a count and never a latency, so an ack at 31 s and an ack at 45 s are the same observation.

Rather than carry "histogram vs. accept 30 s" as an open question into Part 9, it is resolved here: **instrument the acknowledgement latency with a histogram, and keep 30 s as the timeout pending data.** The paper-trading run then produces the empirical distribution needed to defend or tune the value. A timeout without a companion metric is a magic number; a timeout with a histogram is a measured assertion.

No ADR: this adds an instrument inside a structure [ADR-0004](decisions/0004-consumer-defined-venue-interface.md) and [ADR-0020](decisions/0020-initiator-delivery-guarantees.md) already fix, and changes no decision either of them records.

## 2. What was added

| | |
|---|---|
| `carry_order_ack_seconds` | histogram, labels `venue` + `leg`, buckets `0.05 0.1 0.25 0.5 1 2 5 10 30 60`. Time from submission to the venue's **first response**, measured on our clock at both ends |
| `carry_order_ack_timeouts_total` | counter, labels `venue` + `leg`. Orders that passed `ORDER_TIMEOUT` unacknowledged, counted once each |

The two **overlap rather than partition**, and the overlap is the informative part. The histogram holds every order that *eventually* got a response, at its true latency; the counter holds every order whose response *missed the deadline*, including those that later arrived. A late answer is in both; an order never answered is in the counter only. A **synthetic observation is never written for a timeout** — that would corrupt the one distribution the change exists to collect. Consequence worth stating because it is the obvious mistake: **dividing the counter into the histogram's count double-counts the late answers.** The timeout rate is the counter against orders *submitted*.

The metric's name says "ack" and the thing measured is the venue's **first response** — a `NEW`, a fill, or a rejection. The name is kept because it is the name the deadline goes by; the help string and [api-spec §6](api-spec.md#6-metrics-and-alerts) both say "first response" so no reader concludes rejections are excluded.

## 3. Judgement calls made in applying the directive

Each of these departs from the directive's letter and is recorded here rather than in the code.

**The observation site is `Tracker.apply`, not `acknowledged()`.** `acknowledged()` is one arm of the rule table `transition`, which is a pure function of its arguments — that purity is what makes the state machine replayable, and a metric call inside it would end that. The observation sits beside the existing round-trip observation in `apply`, on the edge the deadline actually judges: **the report that takes an order out of `PENDING`**. That is deliberately wider than a `NEW`. An order whose first report is already a `FILLED`, or a `REJECTED`, has been answered by the venue and stops being overdue at that instant; measuring only `NEW` would drop the orders the venue answered *fastest* out of the distribution and bias it slow.

**The interval is measured between two instants on our own clock, and neither of them is `time.Since`.** See §5: the first implementation used the venue's report time and the PO sent it back. `ExecReport` now carries a `ReceivedAt`, stamped once at the venue implementation's receipt boundary, and the interval is `ReceivedAt - Ack.At`. The tracker stays **clockless** — the instant is data on the report, not a clock call inside `apply` — so a replayed report replays its original receipt time and the state machine remains deterministic.

**The labels are `venue` + `leg`, not `leg` alone.** The directive's rule was "add `leg` only if both legs flow through the same path, otherwise no labels". `carry.Order` carries a `Leg` and both legs flow through this tracker by design (Part 15 routes the spot leg through the paper venue), so `leg` applies. `venue` is kept because every other series in `internal/exec` carries it, Part 15 adds a second venue, and dropping it would merge sim, paper and live latencies into one series. Both labels are already sanctioned as low-cardinality in [api-spec §6](api-spec.md#6-metrics-and-alerts).

**Adopted orders contribute nothing.** An order adopted off the wire has no acknowledgement: its `SubmittedAt` is its own first report's time, so the interval would be a fabricated zero in the fastest bucket. That is the defect the Part 8 review found in the round-trip histogram, and it is kept out of this one by the same rule.

**The timeout counter is an edge detected in `Observe`.** Nothing in this system *fires* a timeout — the tracker answers `Overdue(now)` as a question and the binary asks it every second — so "the timeout fired" is the first observation at which an order is overdue. A flag on the order makes it one count per order rather than one per tick.

**The two leg-labelled series are not pre-created at zero**, unlike the outcome counters beside them. A visible zero is worth having for something that can happen and has not; `leg="spot"` is something that *cannot* happen in v1, and a series sitting at zero would advertise a path the system does not have. They appear with the first order of a leg.

## 4. Acceptance

- [x] Explicit buckets; `prometheus.DefBuckets` appears nowhere in the diff — and the bucket list is asserted against the catalogue by `TestAcknowledgementBucketsMatchTheCatalogue`, so it cannot drift from api-spec §6 silently.
- [x] Observations only on genuine acknowledgements; timeouts go to the counter, never into the histogram.
- [x] `leg` decision made per the rule in the directive and reflected identically in code and api-spec §6.
- [x] All documents updated in the same commit; no document describes the item as open.
- [x] Tests cover ack-recorded and timeout-not-recorded, plus: only the first response counts, an adopted order contributes nothing, a late response is counted *and* measured, the counter counts once per order rather than once per tick, the interval is our clock and not the venue's, a report with no `ReceivedAt` contributes nothing, and the boundary stamps `ReceivedAt` distinctly from `At`. **Eleven behaviours mutation-checked** — each removed or inverted in turn and the suite observed to fail.
- [x] `ORDER_TIMEOUT` unchanged at 30 s, with a `TODO(revisit)` on the constant pointing at the paper-trading review.

## 5. The revision: measuring the wait the timeout actually races

The first implementation measured from `Ack.At` to the report's `At` — the venue's `TransactTime`. The PO rejected it on review, and the objection was not primarily skew:

> The timeout fires on *your* clock, waiting for the report to arrive *at your process*. So the distribution meant to validate that timeout should be submit-on-your-clock → report-received-on-your-clock. The venue's embedded timestamp excludes inbound network and ingest latency, which is precisely the tail the timeout exists to catch. As built, the histogram could show a comfortable p99 while the timeout still fires, and you'd have no data explaining why.

That is correct, and reading the code for the fix turned up a second, sharper failure of the same choice. `At` is stamped locally on arrival and then **overwritten by `TransactTime` whenever the report carries one** (`optionalReportFields`), deliberately, because a **resent** report's `At` must be the original event's time for the `fills` row. So on the restart-and-resend path — the one Part 8 exists to handle — `At` can be hours older than the moment the report arrived, and the first implementation would have recorded a long recovery as a near-instant answer. The regression test pins exactly that: a report whose `TransactTime` is the instant of submission but which reaches the process forty-five seconds later is observed at **45 s**, and the pre-revision code observes it at zero.

The fix is the PO's: `ExecReport.ReceivedAt`, stamped once at the boundary (`parseExecReport`), never taken off the wire, never overwritten by tag 60. Both instants are then one host's clock, the skew caveat disappears, and the Part 15 re-check item disappears with it. A venue that leaves it zero contributes no observation rather than a wrong one, and that is tested too — as is the boundary itself, because the first version of the revision left the stamp unguarded and removing it kept the suite green.

**One thing this does not change:** `carry_order_roundtrip_seconds` still measures to the venue's clock. That is a different question — submit to terminal, not a race against a deadline — and a resting order's terminal report legitimately carries the venue's event time. Worth a decision of its own rather than a silent change here.

## 6. Out of scope

Changing the 30 s value; instrumenting fill, cancel or WebSocket latency; dashboards and alert rules over the new series (deferred until paper trading produces data).
