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

## Resolved and deferred history-query scaling finding

The original finding was that request-start reconciliation used batches of 128 and
rescanned the captured archive for each batch, including completed schema 2 requests
in unfiltered queries. The schema 2 completed-start problem is resolved: while
reading end records, unfiltered and interrupted queries collect exact request IDs in
128-byte NUL-padded slots. Up to 8 MiB stays in memory; larger sets spill into a
query-owned mode-0700 temporary directory as sorted runs, then merge with no more
than 31 input files plus one output. The run-path bookkeeping is capped at 4096
initial runs; exceeding that bound returns a query error. The resulting sorted index
is binary-searched before a start enters the interrupted batch. For an all-completed
schema 2 archive this leaves two full history scans (ends and starts), independent
of the number of completed requests, instead of rescanning once per 128 completed
starts. Actual interrupted requests add their existing resolver scans.

The actual interrupted starts still go through 128-ID resolver batches, and schema 1
end attribution still uses its existing 128-ID batches. Those cases can still rescan
the archive and reach the 30-second query deadline. Spilled-index membership also
uses random file reads proportional to `log2(index size)` per filter-eligible start;
large indexes add temporary-disk and random-read I/O. Scratch files are removed when
the query exits, but an abrupt process crash can leave files for normal temporary
directory cleanup. History remains in NDJSON, and this index exists only for one
query. Index build, read, merge, and cleanup failures have a dedicated safe API error;
context deadlines still map to the query timeout response, and temporary paths are
not exposed.

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

### Query-scaling follow-up verification

Two Luna XHigh agents implemented and independently reviewed the exact-ID index.
The coordinating agent reviewed the code and manually compared an artificial raw
archive containing 70,000 completed schema 2 requests and two interrupted starts.
The previous binary returned HTTP 504 at 30.00 seconds for a one-row unfiltered
request page. The new binary returned HTTP 200 in approximately 6.3 seconds on the
same archive. This is one local fixture measurement, not a general latency bound.

Usage included 70,002 client requests, 70,000 completed requests, and two interrupted
requests. The interrupted filter returned the two expected IDs. Observed scratch
directory/file modes were 0700/0600; scratch was removed after successful queries
and client cancellation. An unavailable scratch directory returned HTTP 500 with
`history_query_index_unavailable`, no partial page or scratch path, while recording
remained ready. Restoring the directory allowed the next query to succeed.

A separate compressed fixture preserved all six schema 1/schema 2 request totals,
its interrupted request, and exact binary upstream error bytes. Four-target builds,
JavaScript/Bash syntax checks, and diff checks passed. No automated tests ran.
Multi-level merges beyond 31 input runs were source-reviewed, not runtime-verified;
live-provider and native macOS verification limits remain.
