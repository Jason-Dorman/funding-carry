# Adversarial review of Part 7 — raw findings

**Run:** 2026-09-16/17 · six lenses over the Part 7 change set with a refutation stage · **30 raw findings, 16 verified (10 survived, 6 refuted), 14 left unverified by the run's own cap.**

The [build-plan changelog](../build-plan.md#changelog--decision-record) carries what was done about them. This file is the raw output, kept for the reason the [Part 6 file](part6-adversarial-findings.md) exists: the previous run's findings lived in a session transcript outside the repository and half of it was collected within eleven days.

**Read the state column carefully.** Only the sixteen marked *survived* or *refuted* were adversarially verified. The fourteen *unverified* ones are *one agent's unverified claim* — several turned out to be right and were fixed anyway because they were cheap to check by hand, but nothing here should be taken as established unless its state says so. Duplicates across lenses are kept rather than merged: three lenses independently finding the same gauge is itself the finding.

**The run was interrupted** by a WSL disconnection after all 22 agents had finished but before the results returned, and was recovered with `resumeFromRunId` from the cached agent results.

| Severity | Area | Finding | File | Verified | State |
|---|---|---|---|---|---|
| high | deploy-surface | `carry` receives `FIX_STORE_PATH` from the shared env_file but has no `fix-store` volume, so its sequence store is ephemeral while sim-venue's is not. | `deploy/docker-compose.yml` | refuted | refuted |
| high | metrics-cardinality | `fix_msgs_total`'s `msg_type` label is taken from raw tag 35 with no data dictionary configured, so any client can make the label set unbounded. | `internal/fix/acceptor.go` | survived | fixed |
| high | shutdown-lifecycle | `publish` hands the fill row to the writer on the cancellable root context, so a fill the engine has already committed to is dropped ~50% of the time at SIGTERM and the drop is reported as a fatal writer failure. | `internal/fix/engine.go` | survived | fixed |
| high | shutdown-lifecycle | The engine goroutine publishes reports through an uncancellable quickfix call, so a stalled FIX peer freezes the simulator and makes SIGTERM shutdown impossible. | `internal/fix/engine.go` | refuted | refuted |
| high | test-integrity | TestFillsAreDeterministicUnderASeed cannot fail — it compares output that carries nothing derived from the seed, so the one acceptance criterion that guards determinism is unfalsifiable. | `internal/fix/engine_test.go` | survived | fixed |
| high | test-integrity | TestFillsAreDeterministicUnderASeed passes when the seed is replaced by the wall clock — the jitter the seed drives is invisible in everything the test compares | `internal/fix/engine_test.go` | survived | fixed |
| high | test-integrity | TestSequenceNumbersSurviveARestart passes with the FIX message store thrown away — its only assertion is that a sequence number went up, which a reset session also satisfies | `internal/fix/socket_test.go` | survived | fixed |
| high | test-integrity | TestSequenceNumbersSurviveARestart cannot fail for the reason it names: its only assertion is that the sequence number went up, which a session rebuilt from an empty store also satisfies. | `internal/fix/socket_test.go` | survived | fixed |
| medium | docs-drift | .env.example's new host-mode guidance points FIX_STORE_PATH at ./.fix, a path .gitignore does not cover, while .gitignore's own FIX entry names a directory the implementation no longer uses. | `.env.example` | unverified | fixed |
| medium | docs-drift | api-spec §7 scopes FIX_STORE_PATH to "carry, sim-venue" and asserts it is "A Compose volume", but only the sim-venue service mounts one. | `docs/api-spec.md` | unverified | fixed |
| medium | configuration | `SimVenue.validate()` checks only slippage and the partial threshold, so a book-max-age shorter than the poll interval loads cleanly and then refuses valid orders. | `internal/config/config.go` | unverified | fixed |
| medium | test-coverage | Two documented output paths are never exercised: fixSender.sendCancelReject (0% coverage) and all three simv_* metric updates — no-op versions of both pass the suite | `internal/fix/acceptor.go` | survived | fixed |
| medium | concurrency | The engine goroutine makes a blocking database round trip with the unbuffered `submits` channel as its only inbox, converting database latency into FIX session loss. | `internal/fix/engine.go` | survived | fixed |
| medium | test-integrity | publish's record-before-send guarantee is asserted only in a comment: reversing the order, so a report goes on the wire before (and even when) its fills row fails, passes the whole suite | `internal/fix/engine.go` | survived | fixed |
| medium | correctness | An IOC above SIM_PARTIAL_THRESHOLD fills only one time-slice — roughly a third of the order — because the latency-slicing plan is reused as a stand-in for available depth. | `internal/fix/engine.go` | refuted | refuted |
| medium | correctness | ClOrdID uniqueness is enforced only for orders that were accepted: a rejected order's id and an OrderCancelRequest's own id are both reusable, so two distinct orders can report under one ClOrdID. | `internal/fix/engine.go` | refuted | refuted |
| medium | docs-drift | simv_book_age_seconds reports 0 when no snapshot has ever been read — exactly the case where api-spec §6 and ADR-0019 promise it explains why every order is being rejected. | `internal/fix/engine.go` | unverified | fixed |
| medium | observability | simv_book_age_seconds reports 0 — the freshest possible value — in the one state where the simulator rejects every order. | `internal/fix/engine.go` | unverified | fixed |
| medium | docs-drift | api-spec §4.3 and `publish`'s comment claim the fill row is written before the ExecutionReport is sent, but `Sink.Submit` only enqueues and the report goes out before any flush. | `internal/fix/engine.go` | unverified | fixed |
| medium | observability | `simv_book_age_seconds` reports 0 — the healthiest possible value — in the one state where the venue rejects every order because it has never seen a book. | `internal/fix/engine.go` | unverified | fixed |
| medium | correctness | Fill prices are never quantized to the product's $0.50 tick, so every sim fill prints at a price the venue could not have produced and the modelled slippage is smaller than one tick. | `internal/fix/fill.go` | refuted | refuted |
| medium | docs-drift | The goleak ignore and its build-plan writeup attribute an initiator-only quickfixgo leak to sim-venue, so the operator signal they tell you to watch can never appear. | `internal/fix/main_test.go` | survived | fixed |
| medium | db-contract | The reconnect path of the new fix_sessions upsert writes NULL over last_in_seq/last_out_seq, erasing the only sequence numbers the table ever holds. | `internal/fix/session.go` | unverified | fixed |
| medium | correctness | A logout with no preceding logon permanently adopts the failed-handshake instant as the session's started_at, contrary to the comment directly above it. | `internal/fix/session.go` | unverified | fixed |
| medium | test-determinism | The socket acceptance tests race real timers over real sockets — the deterministic Wait seam built for exactly this is injected by no test — and the mid-partial cancel case was observed failing in an ordinary run | `internal/fix/socket_test.go` | refuted | refuted |
| low | docs-drift | api-spec §4.3 says the jittered latency is applied "before each report"; the code applies it only before fills, so acks, rejections and cancel confirmations go out with no delay at all. | `docs/api-spec.md` | unverified | fixed |
| low | observability | Switching fix_sessions to DO UPDATE re-opens the retry-storm blind spot that rows_written_total/rows_conflicted_total exist to prevent. | `internal/db/rows.go` | unverified | **open** |
| low | shutdown-lifecycle | When the engine stops because the writer died, the FIX logout's `fix_sessions` close-out can never be written, losing the sequence numbers in exactly the case they matter. | `internal/fix/acceptor.go` | unverified | **open** |
| low | correctness | sliceSizes truncates the order quantity through int64, so a large-but-well-formed OrderQty is reported FILLED with a non-zero LeavesQty and fills that do not sum to the order. | `internal/fix/fill.go` | unverified | **open** |
| low | correctness | report.raw() converts a broken decimal-encoding invariant into a silent NULL jsonb, losing the only record of cum_qty, leaves_qty, avg_px and order_qty. | `internal/fix/message.go` | unverified | **open** |

---

## Still open

Four findings were read, judged, and deliberately not acted on. Each was checked far enough to say what is below and no further; none is known to be a live defect.

### `sliceSizes` truncates the order quantity through `int64`
**low** · `internal/fix/fill.go` · unverified

An OrderQty above `math.MaxInt64` reaches `qty.IntPart()`, whose behaviour on an overflowing decimal is undefined, so the slices could sum to something other than the order. Unreachable in any configuration this system has: `MAX_NOTIONAL_USD` is 500, a contract is 0.10 ETH, and the venue's own limits bind long before 9.2×10¹⁸ contracts. Worth a guard the day sim-venue is pointed at something other than this strategy; not worth widening the arithmetic now.

### A logout's `fix_sessions` close-out is lost when the engine stopped because the writer died
**low** · `internal/fix/acceptor.go` · unverified

If the engine returns because `Submit` reported `ErrWriterStopped`, the acceptor still stops and still logs out — and the session recorder's write then goes to the same dead writer. The row keeps the sequence numbers from the previous logon (after the COALESCE fix above, it no longer loses them), and the process is exiting for a reason the operator will see in the fatal write error. Recording an operational row through a writer that has already failed is not a hole worth a second persistence path.

### The upsert re-opens the retry-storm blind spot `rows_conflicted_total` exists to close
**low** · `internal/db/rows.go` · unverified

`ON CONFLICT ... DO UPDATE` reports one row affected whether it inserted or updated, so a re-sent `fix_sessions` batch counts as throughput rather than as a conflict — the exact signal [ADR-0012](../decisions/0012-idempotent-inserts-natural-keys.md) split the two counters to preserve. It is inherent to upserting at all and `cb_products` has had it since Part 5; `fix_sessions` writes a handful of rows per process run, so it cannot produce the storm the counter watches for. Recorded so the next table that wants an upsert weighs it.

### `report.raw()` turns a broken encoding invariant into a silent NULL
**low** · `internal/fix/message.go` · unverified

If `decimal.MarshalJSONWithoutQuotes` were ever flipped, `EncodeSnapshot` refuses to encode and `raw()` returns nil, so the fill row lands with a NULL payload instead of failing. That is the deliberate choice — the fill itself must be recorded, and the columns that matter are all typed `numeric` — but it means the one signal that the global moved is a jsonb column quietly going empty. The global is pinned in `internal/config` and asserted by `TestDecimalGlobalsBehaveAsPinned`, which is the check that would actually fire first.

---

## One defect found while verifying a refuted finding

The socket-test finding was refuted on its mechanism — the fills are seeded, so the schedule is not load-dependent — but checking it turned up a **different, real race in the same file**, which then reproduced during the post-review verification run: `TestScriptedClientRunsAnOrderToFilled` failed with *"timed out waiting for the client to log on."*

`freePort` bound port zero, read the port the kernel assigned, and closed the listener before handing the number to the acceptor. Anything on the machine could take it in between — and with eight sockets churned per suite run across two packages running concurrently, sometimes something did. Confirmed by occupying the port deliberately: `acceptor.Run` returns `bind: address already in use`, which the harness reported on a goroutine while the test failed five steps away at a logon timeout.

The harness now waits until the acceptor is genuinely accepting (dialling it, rather than assuming), reports a bind failure as itself, and retries on a fresh port. It also removed a second of reconnect delay per socket test, since the client no longer dials before the venue is listening.

Worth stating plainly: this was a flaky test, and the project's testing strategy calls those bugs rather than annoyances. It was found by a refuted finding, which is an argument for reading refutations rather than filing them.

## One sub-claim worth carrying forward

The socket-test finding was refuted on its mechanism — the fills are seeded, so the schedule is not load-dependent — but its other observation stands and is not a defect to fix here: **the `Wait` seam that `engineOptions` exposes for deterministic scheduling is injected by no test.** It was built for the run loop and the method-level tests drive the engine directly instead, so it is currently unused machinery. Part 8 wires an initiator against this same engine and is the natural place either to use it or to remove it.
