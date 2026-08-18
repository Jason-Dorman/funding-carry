# onramp — the Go tour

Build plan [Part 3](../../docs/build-plan.md#part-3--go-onramp-toy). This is
`cmd/ingest` shrunk to four files: **two fake feeds fan into one channel, one
writer goroutine batches into TimescaleDB over pgx, and `/metrics` is served
throughout.** Nothing here is production code — it writes to a throwaway table
and takes flags instead of `internal/config` — but every structural rule the real
services follow is in it, which is the point.

## Run it

```bash
make up                                    # TimescaleDB on :15432
export DATABASE_URL='postgres://carry:carry@localhost:15432/carry?sslmode=disable'

go run ./research/onramp -run-for 12s -interval 100ms
curl -s localhost:9109/metrics | grep '^onramp_'
```

Then look at what it wrote:

```sql
SELECT product, count(*), min(price), max(price) FROM onramp_ticks GROUP BY product;
```

`-run-for 0` (the default) runs until Ctrl-C. Drop the table whenever you like;
startup recreates it.

## The shape

```mermaid
graph LR
    F1[feed: perp<br/>goroutine] -->|tick| C{{ticks chan<br/>buffered}}
    F2[feed: spot<br/>goroutine] -->|tick| C
    C --> W[writer<br/>goroutine]
    W -->|pgx batch| DB[(onramp_ticks)]
    M[metrics server<br/>goroutine] -.->|:9109/metrics| P[curl / Prometheus]
    W -.->|counters| M
    F1 -.->|counters| M
    F2 -.->|counters| M
```

Four goroutines plus `main`. The arrows that carry data are channels; the dotted
ones are just method calls on Prometheus collectors.

## Read it in this order

### 1. [`feed.go`](feed.go) — goroutines, channels, `select`

A **goroutine** is a function running concurrently, started by putting `go` in
front of a call. A **channel** is a typed pipe between them. `feed.run` is the
producer side of both.

Three things in that file are the whole concurrency lesson:

**Directional channel types.** The parameter is `out chan<- tick`, not
`chan tick`. The arrow points into the channel, so the compiler will reject a
receive inside this function. Narrowing direction at the boundary is how Go
documents who produces and who consumes.

**`select` picks whichever case is ready.** The outer select waits on either the
ticker or cancellation, and if both are ready it chooses at random. That
randomness matters: it is why nothing can starve, and why the writer's `Submit`
in `internal/db` has to check its stop channel in a *separate* select first.

**The inner select around the send.** A send on a full channel blocks. If the
writer were wedged and the queue full, a producer waiting only on `out <- t`
could never see the context being canceled and shutdown would hang forever. So
the send itself is a `select` with a `<-ctx.Done()` escape. There is a test for
exactly this — `TestFeedStopsWhenChannelIsFullAndContextIsCanceled` deadlocks if
you delete the escape.

Also here: prices are built with `decimal.New(345000, -2)` — an integer
coefficient and a power of ten, exact. `decimal.NewFromFloat` is banned module-
wide and [`internal/guard`](../../internal/guard/decimal_test.go) fails the build
if it appears anywhere.

### 2. [`writer.go`](writer.go) — the one-writer rule and consumer-defined interfaces

**One goroutine touches the database.** That is spec §11, and the reason is that
it makes ordering, batching, and failure handling single-threaded problems.
`pending` is an ordinary slice with no mutex, because exactly one goroutine ever
sees it. Ownership, not locking, is the Go answer to shared state.

**The interface is declared here, at the consumer.** `batchSender` is one method
wide and lives in the file that *uses* it, not in a package that provides it.
That is the project's interface rule (architecture §12), and the payoff is
immediate: [`writer_test.go`](writer_test.go) drives batching, the final flush and
the failure path against a 20-line fake, with no container and no migration.

**The loop has no `ctx.Done()` case, on purpose.** Read `run` and ask what stops
it: the channel closing. Cancellation stops the *feeds*; the feeds finishing
closes the channel; the closed channel stops the writer. The writer is therefore
the last thing in the binary to stop, which is precisely the shutdown order
architecture §8 requires. A writer that stopped on cancellation like everything
else would drop rows its producers already believe are safe.

**`t, ok := <-in`.** The comma-ok form of a receive: `ok` is false only once the
channel is closed *and* drained. Reaching that branch is proof every tick ever
sent has been taken.

**`context.WithoutCancel` on every write, not just the last one.** `flush` builds
a context that inherits values but not cancellation, bounded by its own timeout.
Cancellation is meant to stop *producers*; a round trip already in progress has
to be allowed to land or to fail on its own clock. This one was learned the hard
way — the first version protected only the final flush, and the backpressure
experiment below promptly hit the gap: a `-run-for` deadline arriving mid-batch
killed the write and exited 1 with `insert 1 ticks: timeout: context deadline
exceeded`. `TestWriterNeverWritesOnACanceledContext` is the regression. The production
writer had the identical bug, found by this toy and fixed in the same session —
[architecture §8](../../docs/architecture.md#8-reliability-design) now states the
rule: cancellation stops producers, never I/O already in flight.

### 3. [`main.go`](main.go) — lifecycle

Read `pipeline` bottom to top and you have the shutdown sequence:

| Step | What happens |
|---|---|
| Ctrl-C, SIGTERM, or `-run-for` elapsing | root context is canceled |
| feeds observe `ctx.Done()` | `run` returns, `defer producers.Done()` fires |
| `producers.Wait()` returns | the closer goroutine calls `close(ticks)` |
| writer sees `!ok` | drains, final flush on an uncancelable context |
| `pipeline` returns | metrics context canceled, HTTP server drains |

The log from a real run shows it happening in that order:

```
msg="feed stopped"    product=ETH-USD          ticks=119
msg="feed stopped"    product=ETP-20DEC30-CDE  ticks=119
msg="writer stopped"  final_flush_rows=19
msg="metrics server stopped"
```

Two details worth pausing on. **Only a sender may close a channel, and only once
every sender is done** — hence the `sync.WaitGroup` and a dedicated closing
goroutine; closing from the receiving side, closing twice, or sending after close
all panic. And **the metrics endpoint deliberately outlives the root context**,
because the final flush happens after cancellation and an endpoint that died with
everything else would hide the one scrape that proves the shutdown worked.

### 4. [`metrics.go`](metrics.go) — instrumentation

Collectors are registered on *this binary's* registry (from
[`internal/metrics`](../../internal/metrics/server.go)), never the client_golang
default, so what a binary exports is a property of its wiring rather than a side
effect of its imports. The `onramp_` namespace is deliberately outside the
catalogue in [api-spec §6](../../docs/api-spec.md#6-prometheus-metrics): nothing
scrapes this but you.

Note that the counters are wrapped in small methods. Prometheus takes `float64`
and prices are `decimal` — keeping every conversion in one file is how those two
facts stay separated.

### 5. The tests

`go test -race ./research/onramp`

- [`writer_test.go`](writer_test.go) — batching, the partial final flush, the
  flush that must still run on a canceled context, argument order, and a failing
  insert. The interval flush is driven by injecting a `<-chan time.Time` instead
  of sleeping, the same trick `internal/db.Writer` uses: a test that sleeps is a
  test that is slow and flaky at once.
- [`feed_test.go`](feed_test.go) — the price walk is table-driven, and decimals
  are compared with `.Equal`, never `==`. The operator compares internal
  representation, so `3450.00` and `3450` come out unequal; the guard catches
  that too.
- [`main_test.go`](main_test.go) — `goleak` fails the package if any goroutine
  outlives the tests.

## Break it on purpose

The fastest way to understand this is to make it fail:

| Change | What you should see |
|---|---|
| Delete `close(ticks)` in the closer goroutine | the writer waits forever; `-run-for` never exits |
| Delete the inner `select` in `feed.run` | `TestFeedStopsWhenChannelIsFullAndContextIsCanceled` hangs and `goleak` fails; in the binary it is the failed-flush path that strands a producer mid-send |
| Add a `case <-ctx.Done(): return nil` to the writer loop | the final rows disappear on Ctrl-C |
| Swap `decimal.New(345000, -2)` for `decimal.NewFromFloat(3450.00)` | `go test ./internal/guard` fails the whole module |
| Compare two prices with `==` | same guard, other rule |
| Run with `-interval 1ms -batch-size 1 -queue 4 -flush-every 1m` | one round trip per row makes the writer the bottleneck: `onramp_write_queue_depth` sits pinned at 4 and the feeds slow from 1000 ticks/s to ~270 — backpressure, visible |

## Where each idea reappears

| Here | There |
|---|---|
| `feed.run` | `internal/ingest/ws.go` — Part 4, same loop with a real socket and reconnects |
| `writer` | `internal/db.Writer` — same shape plus per-table batches, retries, backpressure metrics |
| `batchSender` | the identical interface in [`internal/db/writer.go`](../../internal/db/writer.go) |
| `pipeline` | `cmd/ingest`, `cmd/carry` — same context, same shutdown order |
