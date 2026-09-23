# Adversarial review of Part 9 (venue state cache) — raw findings

**Run:** 2026-09-22 · eight lenses over the Part 9 diff, **every** finding attacked by two independent skeptics with different methods (one reads the code and the documented contract, one copies the repo and runs an experiment), with a tiebreaker where they disagreed · **29 distinct findings after merge, all 29 verified: 11 survived, 18 refuted.** One needed a tiebreak.

The PO asked this run to carry two things beyond the diff, and both are answered below: a sweep for a third instance of the "context nobody cancels" hang, and a probe of the one-staleness-model claim at its replica-lag edge.

Nothing was sampled. The Part 7 run left 14 of 30 findings unverified when it hit its own cap and had to warn readers that those were unchecked; that is the bar this run was measured against.

---

## The two the PO sent in

### 1. The context-ownership sweep found no third instance

Two lenses swept independently — every goroutine started in `cmd/carry`, every wait on a channel or context, and every path where a peer can return early — and both found that each parked loop has a named owner that cancels it when its peer exits. The Part 9 fix (an explicit `stopVenue()` before `<-refreshed`, rather than relying on the deferred one) is the second instance, and it is correct.

One candidate was raised and **refuted by experiment**: `dbtest.Seed` starts the batch writer on `context.Background()`, whose only exit is a `Close()` that a `t.Fatalf` would skip. The skeptic forced exactly that path — 6000 rows with a bad first row, to make the size-triggered flush die *while `Submit` was still running* — and `goleak` reported nothing, twice. The reason is in `Writer.Run`: every exit path, including the fatal-flush exit, does `stopOnce.Do(close(w.stop))` before returning, and `ran` is buffered. So `Submit` can only fail *after* the writer goroutine has already finished; the failing state presupposes the thing it was supposed to leak. A control test with a deliberately leaked sleeping goroutine confirmed `goleak` was live and would have spoken.

The rule is now on the standing [review checklist](../engineering-principles.md#code-review-checklist) regardless, because two instances is a pattern.

### 2. Replica lag: the one-staleness-model claim holds

**Confirmed, by contract reading and by experiment.** Freshness keys off the row's own timestamp and never off read success or refresh success. The deciding lines are `staleness(next, now, ...)` in `Refresh`, `freshness(s.X.TS, now, limit)` per source, and `age := now.Sub(at); Stale: age > limit`. The read outcome only chooses *which row* is judged: an error keeps the previous row, a successful empty read zeroes it to `Missing`, a success takes the new one. In no branch is `Stale`, `Age` or `At` set from the read result.

So a lagging replica that successfully serves an old row ages out on exactly the same clock as a stopped feed or a failed read. `TestARowServedSuccessfullyButNeverAdvancingAgesOut` pins it across 25 successful reads.

Two adjacent edges were probed and both refuted. A row stamped *ahead* of the reader's clock would read fresh on a negative age — but both writers derive `ts` from the host clock and truncate, which can only move it backward; the stack shares one kernel clock; and `SELECT count(*) FROM cb_venue_state WHERE ts > now()` returns 0. And a row can be fresh with every price column NULL — but see below, because that one was claimed by two lenses and is **false as claimed**.

---

## The finding that changed the part's acceptance

**The `<50 ms` criterion is false today, and the explanation given for it was wrong.** Measured from Go, through the shipped `db.Connect` / `db.Reader` / `venue.Cache.Refresh`, not from `psql`:

| chunks per table | cold refresh (fresh backend) | steady p50 | steady max |
|---|---|---|---|
| 12 — today | **47–60 ms** | 4.3 ms | 12.0 ms |
| 91 — three months | **215–252 ms** | 11.7 ms | 20.7 ms |
| 366 — one year | **694–1523 ms** | 32.1 ms | 298 ms |

End to end through the real `Refresh`, with only `pool_max_conn_lifetime` shortened so an hour of production passes in five seconds: ticks 0, 5 and 10 cost **81.7 / 84.4 / 56.8 ms** at today's chunk count, every other tick 3.7–13.6 ms.

The lens's *mechanism* was partly wrong and the skeptic corrected it, which is the process working. It is not per-refresh planning: cold cost is mostly catalog and relcache warming paid **once per connection**, and after five executions pgx's cached statement gets a generic plan (0.015 ms). But the lens's *conclusion* was right and understated, because **execution is not O(1) in chunk count either** — executor init over the chunk-append children grows about 0.08 ms per chunk per refresh, so the warm path reaches 32 ms at a year's history.

Why the original evidence missed it: it watched 60 refreshes over five minutes, a window shorter than pgx's one-hour `MaxConnLifetime`, so it saw exactly one cold connection — the startup one — and attributed the three outliers to the compile jobs that were running. At least the first was the cold-connection cost, which reproduces at 57–85 ms on an idle machine.

**Production harm today is small and the review says so plainly:** `Latest()` serves from memory and never blocks on a refresh, `Run` bounds each refresh to one interval, and staleness ages come from row timestamps, so a 250 ms refresh delays nothing that matters. What is broken is the acceptance criterion and the sentence explaining it. The explanation is corrected in code and in the evidence; **what the criterion should say, and whether to time-bound the queries, is an open item for the PO** (Part 9 section).

---

## Survivors, fixed in this session

Each was run against the unfixed code and watched to fail first.

| # | Finding | Fix | Mutation observed red |
|---|---|---|---|
| 1 | A metrics bind failure left the binary running headless — live FIX session, live cache, no `/metrics`, no `/healthz`, until SIGTERM. Compose restart policies fire on exit, not on unhealthy, so the container stayed up indefinitely | `metrics.Listen` binds synchronously at startup in all three binaries, before anything else starts | all three tests hung 30 s, then passed in 18 ms |
| 2 | Nothing asserted the cache refreshes more than **once**: every unit test injects its ticker, so the production ticker branch was exercised by nothing | `recordingStore` in `cmd/carry`, asserting ≥2 refreshes in 300 ms at a 50 ms interval | "cache refreshed 1 times … the ticker never fired" |
| 3 | The perp/spot product ids `cmd/carry` wires into the cache were verified by no layer; swapped, every consumer reads the spot row as the perp and both CI jobs stay green | the same store asserts the ordered pair, because a swap asks for the same two products and a set comparison cannot see it | "read venue state for [ETH-USD ETP-…], want [ETP-… ETH-USD]" |
| 4 | `TestRunBoundsEachRefreshToTheInterval` pinned neither "each" nor "the interval" — a bound of 100 intervals passed | assert each refresh lands within ten intervals, and tick once so the second is bounded too | "the first refresh took 2.0s against a 20ms interval" (and the second) |
| 5 | No test pinned the histogram **observation**: `CollectAndCount` counts series, and a plain histogram is one series whether observed or not | assert the sample count after each refresh | "has 0 observations after one refresh, want 1" |
| 6 | The metrics fixture gave every source the same timestamp, so a transposed source label was invisible — and per-source reporting is the part's whole point | distinct ages per source | golden comparison fails on the transposition |
| 7 | `fakeStore.Product` ignored the product id the real reader filters on, while claiming "it fails where the real one fails" | filter on the id | "Product known=false … want known, 0.1" |
| 8 | The `carry_feed_ok` help string — written so the gate condition travels with the series — was **invalid PromQL** (unquoted label values) | quote them | upstream parser: `unexpected identifier "account" in label matching` |
| 9 | `LatestFundingRate` and `LatestBook` read `rows.Err()` before the deferred `Close`, so a connection dropped after the row reported success. `latest.go`'s new helper does the opposite and its comment says why — the package taught two rules | close before reading `Err`, and wrap | real driver: `Err()` nil before `Close`, `division by zero` after |
| 10 | A new integration test asserted a global maximum over a shared, never-truncated table; when it breaks it blames "the older row" for a value an unrelated test wrote | clear the two dimensionless tables first | a 2027-dated fixture makes it fail with the misleading message |
| 11 | Documentation: `doc.go` claimed live and replay "read through this same cache … one code path" (the backtester is Python and `Store` has no as-of read); architecture §3 said the reader "exists for one caller" when it has three, in the same diff that rewrote §2 to describe the new ones; §6's label registry omitted `source` and `table` while this part added two more `source` series; and the `FeedOK` scope needed an ADR rather than being judged inside ADR-0003 | corrected; [ADR-0021](../decisions/0021-feedok-market-data-tradeok-order-flow.md) added | n/a — doc facts, verified directly |

---

## Notable refutations

Eighteen findings died. Six are worth reading, because in each case the refutation is more informative than the finding.

- **"A fresh perp row can carry no price at all, so `FeedOK` lies."** Claimed by two lenses with the Friday maintenance hour as evidence, and **false on the data**: through 2026-09-18 21:00–22:00Z all 720 perp rows carry `maintenance_window` *and* both marks non-NULL — the REST poller ran straight through the halt. Rows with every price column NULL over seven days: **zero**. The real case (248 rows over six days where marks lag while the quote is fine) still carries a mid. And the per-column question is already assigned: `.Valid` is a documented universal of the whole schema (api-spec §5), per-column freshness lives at the writer where ADR-0014 puts it, and the only consumer of `State` today is the metrics exporter. Carried to Part 10 as a note, not a defect: features must check `.Valid`, because a `NullDecimal` zero-values to 0.
- **"One `STALE_FEED_SECS` across a 5 s and a 30 s cadence makes the wallet flap."** The 35 single 60 s gaps in seven days are real; the **duty cycle is exactly zero**. Simulating the real 5 s sampling over 110,566 samples and attributing each stale sample to its gap: the 60 s gaps produce none, because one missed 30 s poll lands exactly *on* 60 s and the comparison is strictly greater. The strict boundary another finding had questioned is precisely what makes this a non-event. Wallet staleness totalled ~25 s in seven days, from one 90 s gap.
- **"`State`'s copy shares pointers, so a consumer can mutate the cache."** True of the mechanism, refuted as a defect by both skeptics: no production site writes through those pointers, `Refresh` replaces whole rows, and — decisively — `decimal.Decimal` holds a `*big.Int`, so *every* money field in every row already aliases across copies. The codebase's value semantics rest on nobody mutating in place, everywhere, not just here. A counter-test confirmed the documented guarantee holds on the normal path.
- **"The never-freeze bound lives in `Run`, which Part 10 retires."** The contract attributes it to `Run` in those words, and "takes over the call" is replacement, not a second concurrent caller. The overlap regression reproduces in isolation — a slow refresh can commit an older row over a newer one — but `Refresh` has exactly one production caller, invoked synchronously inside a single-goroutine loop. Carried to Part 10, which inherits both the deadline and the no-overlap obligation.
- **"A database outage at startup is a refusal, not the documented degradation."** Correct behaviour: `carry` with no database has nothing decidable, and §8's "on restart reload positions from DB" presupposes a reachable one. No document asserts otherwise — §3.7's degradation sentence is about `Cache.Refresh`, whose contract holds at all times including before the first successful read.
- **"Adding a fifth source is shotgun surgery, and Tier 2/3 have no home."** The four sources are the part's acceptance criteria, and generalising now would mean a row-decoder registry built for a consumer that does not exist. The Part 10 hazard is real and is Part 10's acceptance problem; a one-line note goes in its section instead.

One refuted finding still produced a change, as in the Part 8 runs: the claim that `dbtest` had no isolation guard was refuted (Postgres refuses to drop the database you are connected to, so the protection is in the server), but the review's measurement of *why* it is safe is now what the helper's comment says.

---

## What this run cost, and what it found

Sixteen agents. It found one defect that let a binary trade with no observability and no way for Compose to notice, one acceptance criterion that was false on the day it was written, six tests that could not fail — including the one guarding the part's headline behaviour, that the cache refreshes at all — one help string that could not parse, one error path that reported success on a dropped connection, and four documentation claims the code does not support. In a diff that had already been written carefully, reviewed against its own criteria, and had a green suite twice.

Two of its own lenses were wrong in ways the skeptics caught: the maintenance-hour claim was false on the data, and the planning-cost mechanism was misdiagnosed even though its conclusion held. Both corrections came from the experiment track, which is the argument for running two methods against every finding rather than one.
