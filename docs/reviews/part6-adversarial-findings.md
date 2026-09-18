# Adversarial review of Part 6 — raw findings

**Run:** 2026-09-04 · six lenses over the Part 6 change set with a refutation stage · **25 raw findings, 8 verified (7 survived, 1 refuted).**

The [build-plan changelog](../build-plan.md#changelog--decision-record) carries the verified findings and what was done about them. This file is the raw output.

It exists because that changelog entry originally pointed at a run transcript living outside the repository, in `~/.claude/` — session-local. Eleven days later half of it had already been collected. A pointer a future reader cannot follow is not a record, so the findings are here instead.

**Two things to read carefully.** Only the eight marked *closed* were adversarially verified; everything else is **one agent's unverified claim**, and may be wrong, unreachable, or already prevented elsewhere. And several lenses found the *same* defect independently — those duplicates are kept rather than merged, because four lenses converging on one line is itself the finding.

| Severity | Area | Finding | File | State |
|---|---|---|---|---|
| high | test-integrity | No test constrains which address either wallet balance is read for; the fake ignores the balanceOf argument | `internal/ingest/base.go` | closed |
| high | secrets | The credential URL leaks into a WARN log line when BASE_RPC_URL does not parse — the one url.Error path that is wrapped | `internal/ingest/ethrpc.go` | closed |
| high | secret-leakage | The Alchemy API key reaches a WARN log line whenever BASE_RPC_URL fails to parse | `internal/ingest/ethrpc.go` | closed |
| high | secret-leak | The Alchemy key reaches a WARN log line whenever BASE_RPC_URL fails to parse | `internal/ingest/ethrpc.go` | closed |
| high | secret-leak / test-integrity | The Alchemy key reaches a log line through the request-build path, which the leak test does not cover | `internal/ingest/ethrpc.go` | closed |
| high | test-coverage | VenueState.SpotMid — the production spot fallback — has no test at all | `internal/ingest/venuestate.go` | closed |
| medium | doc-code-drift | `runOnBoundary` is shared by two callers, not three — the venue-state sampler still carries its own copy of the loop, plus a `lastBoundary` guard the two new callers do not have | `docs/architecture.md` | closed |
| medium | schema-migration | Migration 000006 is not re-appliable: its own down migration leaves rows that its up migration rejects | `internal/db/migrations/000006_base_state_spot_px_source.down.sql` | closed |
| medium | migration-reversibility | 000006's down migration is not re-appliable: `down 1` then `up` fails and leaves the schema marked dirty | `internal/db/migrations/000006_base_state_spot_px_source.down.sql` | closed |
| medium | silent-degradation | spot_px silently drops to NULL forever once the pool failure has been warned about — no log line, no metric movement | `internal/ingest/base.go` | closed |
| medium | observability | spot_px silently goes NULL forever once the pool is already failing: no log, no metric, and the health gauge keeps advancing | `internal/ingest/base.go` | closed |
| medium | secrets | The non-2xx body snippet is logged verbatim, so an upstream that echoes the request path puts the key in the log | `internal/ingest/ethrpc.go` | **open** |
| medium | degradation-semantics | No per-poll deadline: 5 sequential 15s RPC timeouts make one Base poll overrun the 30s interval and skip boundaries | `internal/ingest/ethrpc.go` | closed |
| medium | test-integrity / docs-drift | The two new Prometheus series are outside the catalogue test that exists to make a rename deliberate | `internal/ingest/metrics.go` | closed |
| medium | doc-code-drift | "One function all three callers share" is false — only two components call runOnBoundary; VenueState.Run, Marks.Run and FundingRunner.Run each keep their own copy | `internal/ingest/poller.go` | closed |
| medium | docs-drift | "one function all three callers share" is false — the venue-state sampler still has its own copy of the loop | `internal/ingest/poller.go` | closed |
| medium | test-coverage | The extracted boundary-wait left the venue-state sampler behind: its private copy of the loop survives the exact mutation the new regression test was written to catch | `internal/ingest/poller.go` | closed |
| low | secrets | A revealed Secret's raw value is echoed into the startup error that main prints to stderr | `cmd/ingest/main.go` | **open** |
| low | misleading-diagnostics | The one permanent, unrecoverable failure in the Base poller is logged as retryable=true | `internal/ingest/base.go` | closed |
| low | shutdown-noise | Graceful shutdown emits four spurious "the column is going NULL" warnings | `internal/ingest/base.go` | closed |
| low | lifecycle | BasePoller warns and increments the spot-source counter for reads it then discards on cancellation | `internal/ingest/base.go` | **open** |
| low | input-validation | decodeQuantity accepts a signed hex QUANTITY, so a negative wei balance or gas price is recorded as fact | `internal/ingest/ethrpc.go` | closed |
| low | correctness | rpcTimeout bounds one round trip, not one poll — five sequential 15s calls make a 75s poll on a 30s interval | `internal/ingest/ethrpc.go` | closed |
| low | docs-drift | ethrpc.go's header and ADR-0017 misstate the client's own surface and size | `internal/ingest/ethrpc.go` | **open** |
| low | correctness | untilNextBoundary's "always returns a positive duration" is false for a non-positive interval, and runWaiting then busy-loops | `internal/ingest/venuestate.go` | **open** |

---

## Still open

Each was checked far enough to say what is below, and no further. None is known to be a live defect.

### The non-2xx body snippet is logged verbatim, so an upstream that echoes the request path puts the key in the log
**medium** · `internal/ingest/ethrpc.go`:197

**Where it stands.** Not investigated. Needs an upstream that echoes the request path into its error body.

**Claim.** decodeRPC copies up to 512 bytes of the server's error body into RPCError.Message with no filtering; RPCError.Error() renders it and BasePoller.warn logs it verbatim. Because the request path IS the credential, any upstream that echoes the request path in its error body writes the key to the log. The code has no defence at all here — it never scrubs c.url (or the path segment carrying the key) out of anything it returns — while the documented invariant ("it is never logged") is unconditional. Alchemy's own 401 body does not echo (the acceptance run's `http 401` is consistent with that), so the trigger is a different responder in front of or instead of Alchemy — an Express-style gateway ("Cannot POST /v2/<key>"), a misdirected BASE_RPC_URL, or a corporate proxy — which is exactly the situation the operator is in when this error fires.

**Alleged failure.** BASE_RPC_URL points at a host whose 404/403 handler echoes the path (Express's default handler is the common one). The block read fails and the poller logs: {"level":"WARN","msg":"base read failed; the column is going NULL","what":"block_number","error":"eth_blockNumber: http 404: Cannot POST /v2/s3cret-alchemy-key"} — the key in the container log.

### A revealed Secret's raw value is echoed into the startup error that main prints to stderr
**low** · `cmd/ingest/main.go`:133

**Where it stands.** Not fixed. Only WALLET_ADDRESS takes this path today, and that is a public address by doctrine — but the shape is general.

**Claim.** WALLET_ADDRESS is typed config.Secret, whose whole contract is that its content cannot reach a log line (String/GoString/LogValue/MarshalJSON/MarshalText all redact; Reveal is the sole way out). chainReader reveals it and hands the raw string to ParseAddress, whose error quotes the input with %q (ethrpc.go:71); chainReader wraps that with the variable name and run() → main prints it to stderr with %v. This is the one place in the binary where a Secret's plaintext reaches an output stream, and it is unbounded — the whole value is echoed whatever it is. The neighbouring credential path deliberately does not do this: coinbase.NewSigner describes the malformed key ("looks like a PEM", "decodes to %d bytes") without ever echoing it.

**Alleged failure.** An operator editing .env.private puts a wrong value in WALLET_ADDRESS — pasting SESSION_KEY, a private key, or a base64 secret into the neighbouring line, which is the realistic way this field is ever malformed. Startup fails and stderr/`docker logs` shows: `ingest: WALLET_ADDRESS: address "3MEs9dEfghij0123456789abcdefghijKLMNOPqrstuvwxyz=": want 40 hex digits, got 49` — the whole pasted secret in the log.

### BasePoller warns and increments the spot-source counter for reads it then discards on cancellation
**low** · `internal/ingest/base.go`:172

**Where it stands.** Half fixed: the warning is suppressed on cancellation, the counter increment is not. Confirmed still present at base.go:284.

**Claim.** `Poll` checks `ctx.Err()` only *after* `p.read(...)` has returned (base.go:172-174), but `read` has already had per-column side effects: each of `walletETH`, `walletUSDC`, `gasGwei` and `spotPx` calls `p.warn(...)` on failure (base.go:212, 224, 240, 275) and `spotPx` calls `p.metrics.spot(...)` on success (base.go:270, 282). A context.Canceled from the RPC client is not an `*RPCError`, so `warn` classifies it `retryable=true` and logs it as a real degradation.

**Alleged failure.** SIGTERM lands while a poll is between its block read and its last contract call. All four column reads fail with context.Canceled, so the log emits four `"base read failed; the column is going NULL" retryable=true` WARN lines naming wallet_eth, wallet_usdc, gas_gwei and spot_px at every such shutdown — an outage report for an orderly stop. In the same window `spotPx` can take the Coinbase fallback (`VenueState.SpotMid` is an in-memory read and ignores ctx), incrementing `ingest_base_spot_px_source_total{source="coinbase"}` for a row that is then dropped by the `ctx.Err()` check and never reaches base_state. That counter is documented in api-spec §6 as the signal "for the alert that notices the fallback carrying rows it was never meant to carry", so it and the table disagree by one.

### ethrpc.go's header and ADR-0017 misstate the client's own surface and size
**low** · `internal/ingest/ethrpc.go`:24

**Where it stands.** Confirmed, and made worse by the review fixes: ADR-0017 said 'about 250 lines', the file is 426. Corrected in the ADR; the file header's own claim was not re-checked.

**Claim.** The `EthClient` doc comment says "the read path is three RPC methods and one ERC-20 call, and the ~100 lines below…". The file implements four RPC methods (`eth_blockNumber`, `eth_getBalance`, `eth_gasPrice`, `eth_call`) and the poller makes five contract calls, and the file is 412 lines (246 excluding blanks and comments). base.go:56 in the same package says "the whole Part 6 surface is four RPC methods and these five calls", as do ADR-0017's Context, api-spec §2.1, the build plan, CHANGELOG and README. ADR-0017's Decision separately claims "It is about 250 lines including its comments" — 412 including comments.

**Alleged failure.** A reviewer reading the file's own header to decide whether ADR-0017's hand-roll-versus-go-ethereum trade still holds is given a surface 25% smaller and a size 4x smaller than the code; the two numbers ADR-0017's whole argument rests on (surface, line count) are both wrong at the point of use, and the header contradicts base.go directly, one file away.

### untilNextBoundary's "always returns a positive duration" is false for a non-positive interval, and runWaiting then busy-loops
**low** · `internal/ingest/venuestate.go`:237

**Where it stands.** Confirmed unreachable from config — loader.Seconds rejects <= 0 — so a latent trap for a future caller, not a live defect.

**Claim.** The doc comment claims `untilNextBoundary` "always returns a positive duration, so a wake that lands exactly on a boundary schedules the following one rather than spinning". That holds only for interval > 0. `time.Time.Truncate` documents that for d <= 0 it returns t unchanged, so `untilNextBoundary(t, 0)` returns 0 and `untilNextBoundary(t, -5s)` returns -5s. `runWaiting` (poller.go:100-113) feeds that straight into `wait(...)`; `time.NewTimer` with a non-positive duration fires immediately, so the loop degenerates into an unthrottled poll loop with no delay. Unlike `now`, which both `NewBasePoller` and `Ingest.New` default when nil, no constructor or call site validates the interval.

**Alleged failure.** `ingest.New` with `Options{Chain: someReader, BaseAddresses: …}` and `BaseInterval` left at its zero value — the field is a plain exported field of an exported struct with no default applied at ingest.go:167 — produces a BasePoller that, after its immediate first poll, spins at full CPU issuing ~5 JSON-RPC calls per iteration against the Alchemy endpoint with zero delay until the rate limiter or the bill stops it, and stamps every row `ts = at.Truncate(0) = at`, defeating the (ts) natural key. The same shape applies to `SampleInterval` for the AccountPoller. Not reachable from configuration today: `internal/config/env.go:125-136` rejects a non-positive `POLL_BASE_SECS`/`POLL_REST_SECS`, so this is a latent hazard behind a comment that says the guard exists.

---

## Closed by verification and fix

| Finding | What was done |
|---|---|
| No test constrains which address either wallet balance is read for; the fake ignores the balanceOf argument | fake now records the balanceOf argument; both address swaps fail |
| The credential URL leaks into a WARN log line when BASE_RPC_URL does not parse — the one url.Error path that is wrapped | both *url.Error paths unwrapped; TestAMalformedEndpointDoesNotLeakTheURLEither |
| The Alchemy API key reaches a WARN log line whenever BASE_RPC_URL fails to parse | same defect, second lens — fixed |
| The Alchemy key reaches a WARN log line whenever BASE_RPC_URL fails to parse | fixed |
| The Alchemy key reaches a log line through the request-build path, which the leak test does not cover | same defect, third lens — fixed |
| VenueState.SpotMid — the production spot fallback — has no test at all | TestSpotMidIsTheSpotProductAndOnlyWhileFresh; both mutations caught |
| `runOnBoundary` is shared by two callers, not three — the venue-state sampler still carries its own copy of the loop, plus a `lastBoundary` guard the two new callers do not have | refuted as a defect; its factual half fixed — VenueState.Run now calls runOnBoundary |
| Migration 000006 is not re-appliable: its own down migration leaves rows that its up migration rejects | CHECK is now NOT VALID; seeded round-trip test catches it |
| 000006's down migration is not re-appliable: `down 1` then `up` fails and leaves the schema marked dirty | same defect, second lens — fixed |
| spot_px silently drops to NULL forever once the pool failure has been warned about — no log line, no metric movement | spotSourceNone counter plus a warning on the transition |
| spot_px silently goes NULL forever once the pool is already failing: no log, no metric, and the health gauge keeps advancing | same defect, second lens — fixed |
| No per-poll deadline: 5 sequential 15s RPC timeouts make one Base poll overrun the 30s interval and skip boundaries | a poll is now bounded by its own interval |
| The two new Prometheus series are outside the catalogue test that exists to make a rename deliberate | both series added to TestExportedSeriesMatchTheCatalogue |
| "One function all three callers share" is false — only two components call runOnBoundary; VenueState.Run, Marks.Run and FundingRunner.Run each keep their own copy | fixed by making the claim true |
| "one function all three callers share" is false — the venue-state sampler still has its own copy of the loop | same, second lens — fixed |
| The extracted boundary-wait left the venue-state sampler behind: its private copy of the loop survives the exact mutation the new regression test was written to catch | same, third lens — fixed |
| The one permanent, unrecoverable failure in the Base poller is logged as retryable=true | errPoolMismatch now classified non-retryable |
| Graceful shutdown emits four spurious "the column is going NULL" warnings | warn suppressed on context.Canceled |
| decodeQuantity accepts a signed hex QUANTITY, so a negative wei balance or gas price is recorded as fact | decodeQuantity refuses a leading sign; TestASignedQuantityIsRefused |
| rpcTimeout bounds one round trip, not one poll — five sequential 15s calls make a 75s poll on a 30s interval | same as the per-poll deadline — fixed |
