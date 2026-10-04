# Code Review — 2026-10-04

The review covered configuration and credentials, OAuth, routing, HTTP APIs,
Responses streaming, history recording and queries, compression and deletion,
the embedded WebUI, runtime setup, and release packaging. Luna XHigh reviewed
the source; the coordinating agent evaluated findings and reviewed fixes.

## Accepted findings

### Provider credential removal could overtake durable admission

An inference could write its durable start record before acquiring the
provider's authentication lock. Disconnect could clear credentials in that
gap. Provider deletion also bypassed the lock, so it could remove credentials
after the request had durably started, causing local token preparation to fail.

The fix orders admission and token preparation against disconnect and Provider
deletion using a provider preparation lease. Local readiness is checked before durable admission;
refresh or other provider network I/O still requires successful durable logging.
The lease ends after the token is copied, before inference transmission.

### Error response bodies had no read duration bound

Non-success responses and successful responses with an unexpected content type
were limited by byte count, but could hold an inference slot indefinitely if
the upstream sent headers and stalled its body.

The fix gives those body reads a 30-second budget by cancelling a child request
context. Received bytes and read-failure metadata remain available in history.
Successful SSE responses retain their existing streaming lifetime.

### Compression cleanup could act on a deleted pair

Cleanup captured verified raw/compressed pairs before acquiring a maintenance
lease. Date deletion could finish in between, remove the compressed copy, and
clear the pair's bookkeeping. Stale cleanup then treated the missing copy as
corruption and prevented subsequent history queries.

The fix checks that each pair is still pending cleanup after acquiring the
maintenance lease, before accessing its files.

## Deferred finding

The independent history review confirmed that request-start reconciliation uses
batches of 128 and rescans the captured archive for each batch. This also affects
completed schema 2 requests in unfiltered queries. Large archives can therefore
reach the existing 30-second query budget. This documented, unmeasured scaling
limit remains deferred; fixing it needs a separate query/index design that keeps
history outside SQLite and preserves bounded memory and crash recovery.

## Verification and limits

Fixes passed source review and compilation for Linux and macOS, on amd64 and
arm64. JavaScript and release-script syntax checks passed. A manual read of a
copied compressed-history fixture returned the expected six requests and usage
totals, with recording ready and no failed segments. Manual management calls
created and deleted an unreferenced Provider (201/204); deletion of a referenced
Provider remained blocked (409). No automated tests were
added or run. The timing-sensitive races are source-reviewed;
authenticated OpenAI and pi integration and native macOS execution remain
unverified.
