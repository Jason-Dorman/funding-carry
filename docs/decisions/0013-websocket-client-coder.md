# ADR-0013: `github.com/coder/websocket` is the WebSocket client

**Date:** 2026-08-20 · **Status:** accepted · **Build part:** P4

## Context

Part 4 needs a WebSocket client for the Coinbase Advanced Trade market-data socket, and the module had none. The choice is structural because of one project rule it interacts with directly: `context` cancellation flows the whole stack ([architecture §3](../architecture.md#3-concurrency-model-ingest)), and the place that rule is hardest to honour is a goroutine parked on a socket read. Five streams run one such read each, and graceful shutdown depends on all five returning promptly when the root context is canceled.

The two realistic candidates were `github.com/gorilla/websocket`, the most widely deployed Go client, and `github.com/coder/websocket` (the maintained continuation of `nhooyr.io/websocket`).

## Decision

`github.com/coder/websocket`, wrapped behind a three-method `Conn` interface declared in `internal/ingest` ([conn.go](../../internal/ingest/conn.go)), so no code above that file imports the library.

`Read`, `Write` and the handshake all take a `context.Context`, which means the read deadline and the root context are the *same* mechanism: `session` derives a per-read timeout from the root context, and a `SIGTERM` reaches a parked read directly rather than through a second cancellation path.

## Alternatives considered

- **`gorilla/websocket`** — cancellation is expressed as `SetReadDeadline` plus a goroutine watching `ctx.Done()` to close the connection out from under the read. That is a second cancellation mechanism running beside the root context, in the one place where the project's rule is load-bearing, and it is the kind of construct that works until a shutdown happens to land between the deadline being set and the read starting. It is also in long-term maintenance mode.
- **`golang.org/x/net/websocket`** — documented by its own authors as lacking features and not recommended.
- **Hand-rolled over `net/http` hijacking** — no. Framing, masking, close handshakes and continuation frames are exactly the kind of code that is cheap to write and expensive to get right, and none of it is this project's subject.

## Consequences

- The dependency surface is three methods wide. Replacing the library is an edit to `conn.go` and nothing else, and the reconnect loop, gap detection and resubscription are all tested against a scripted `Conn` with no network involved.
- One thing the wrapper must set explicitly: the library defaults to a 32 KB read limit, and the live level2 snapshot for the perp was **74 KB**. Left at the default, every snapshot would fail its read and the level2 stream would reconnect forever without ever building a book. `readLimit` is 4 MB, and `TestARealConnectionAcceptsAFullBookSnapshot` sends an oversized frame through a real handshake so the limit cannot be lowered silently.
- `CloseNow` rather than a close handshake: every call site is a connection already being abandoned, so waiting for the peer to answer would only delay the reconnect.
- Revisit if the library stops being maintained, or if the FIX-side work in Parts 7–8 turns out to want a different transport stack (it does not — quickfixgo owns its own sockets).
