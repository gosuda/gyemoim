# History Query API

The loopback management API reads request history from NDJSON. History is not
stored in SQLite. These endpoints are available under the same local Host and
browser-origin protections as the rest of `/api/`.

## Date-range deletion

`POST /api/storage/delete` accepts a bounded strict JSON object:

```json
{"firstDate":"2026-10-01","lastDate":"2026-10-03"}
```

Both dates are inclusive UTC calendar days. The server applies the effective
half-open range `[firstDate 00:00Z, min(day-after-lastDate 00:00Z, submission-time))`.
Future dates, reversed ranges, and empty effective ranges return HTTP 400. The result
includes the effective `from` and exclusive `to` timestamps, `segmentsProcessed`, and
`recordsRemoved`.

Deletion uses `started_at` and applies to all accounts and Models. If any matching
request is in progress, HTTP 409 is returned before any history file changes. While
deleting, new queries return HTTP 503 with `history_maintenance`. If roll-forward
recovery is pending after a failure or restart, queries remain HTTP 503 and
`GET /api/storage` exposes the deletion range, segment progress, state, and safe error.
The journal is replayed before normal history validation and compression at startup.


## Request list

`GET /api/requests` returns a bounded page of request summaries:

| Query parameter | Meaning |
| --- | --- |
| `limit` | Page size, default `25`, maximum `100`. |
| `cursor` | Opaque `nextCursor` from the preceding response. |
| `from` | Optional UTC RFC 3339 start time, inclusive. |
| `to` | Optional UTC RFC 3339 start time, exclusive. |
| `account_id` | Exact ServiceAccount ID. |
| `model_id` | Exact Model ID. |
| `provider_id` | Exact actual Provider ID used by the request. |
| `upstream_model` | Exact upstream Model identifier used by the request. |
| `outcome` | `completed`, `failed`, `cancelled`, `incomplete`, `interrupted`, or `active`. |

Results are ordered by `startedAt` descending, then `requestId` ascending. Responses
include `hasMore` and, when another page exists, `nextCursor`. The API does not scan
the full archive to compute a total count. The UTC time range is half-open
`[from,to)` and filters use request start time.

Schema 2 end records supply their own account, Model, Provider, outcome, usage, and
timing attribution. During the end-record scan, unfiltered and interrupted queries
build an exact index of schema 2 end IDs. This prevents completed schema 2 starts
from entering the interrupted-start resolver. Schema 1 ends are joined to their
start and transmission records in batches of at most 128 IDs. Starts without an end
are included as `outcome: "interrupted"`; active in-process requests are included as
`outcome: "active"`. Segment filenames do not determine request order. Legacy
resolution and actual unmatched-start rescans remain batched; if those scans or index
I/O exceed the 30 second query budget, the API returns a timeout error and no partial
page.

## Request details and full content

`GET /api/requests/{requestId}` returns the summary, timing offsets, attempt and
upstream-response metadata, event counts, and byte counts for the incoming and
effective request bodies. Each upstream response entry includes `contentType`, the
sanitized upstream `Content-Type` header value; it is absent when the upstream sent
no header. Large bodies are not embedded in this response. Retrieve
their exact recorded bytes with:

`GET /api/requests/{requestId}/body?kind=incoming`

`GET /api/requests/{requestId}/body?kind=effective&attempt=1`

`GET /api/requests/{requestId}/body?kind=http_response&attempt=1`

The `kind` values are `incoming`, `effective`, and `http_response`. Use `offset` and
`limit` to retrieve byte chunks; the default chunk is 256 KiB and the maximum is
1 MiB. The response is `application/octet-stream` and includes `X-Content-Offset`,
`X-Content-Total-Bytes`, `X-Content-Complete`, `X-Content-Truncated`, and
`X-Content-Read-Failed` headers. Each response write has a 30 second deadline. The
upstream response flags report whether the recorded HTTP body itself was truncated
or could not be fully read.

Attempt and upstream-response metadata is capped at 128 entries per detail response.
`attemptsTruncated` and `responsesTruncated` say when those metadata arrays exceed
the cap; content remains addressable by attempt number.

## SSE events

`GET /api/requests/{requestId}/events?limit=25&cursor=0` returns event metadata and
exact raw frame bytes as base64, bounded to 100 events and 1 MiB of decoded raw bytes
per response. Events are ordered by sequence ascending, with elapsed offset as a
tie-breaker. An event name preview is at most 1 KiB; `eventNameBytes` and
`eventNameTruncated` make a shortened preview explicit. `rawOmitted` explains when a
frame does not fit the page's byte budget, and every item has a `rawUrl` to retrieve
the complete frame.

`GET /api/requests/{requestId}/events/{sequence}?offset=0&limit=262144` returns a
raw UTF-8 event-frame chunk. The `limit` is at most 1 MiB. Repeat with the next byte
offset until `X-Content-Complete` is `true`. A single SSE frame may be 64 MiB, so
large frames are never collected into a stream-sized response array.

## Usage aggregates

`GET /api/usage` returns one aggregate. Add `group_by` with a comma-separated subset
of `account`, `model`, `provider`, and `upstream_model` to group results. The same
time, identity, and outcome filters as `/api/requests` apply. At most 1000 groups are
retained; exceeding the cap returns an explicit error instead of dropping groups.

`clientRequests` counts client requests; `upstreamAttempts` counts upstream
transmissions. Each token field has a `total` of known values plus `knownRequests`
and `unknownRequests`. An explicitly reported zero is known; an absent count remains
unknown. Cached input and reasoning output are subsets, not additional totals.
`cacheRatio` is `null` when any request in the group lacks either input or cached
counts, when total input is zero, or when cached input exceeds input.
`cacheRatioUnavailableReason` identifies which condition applied. Otherwise the
ratio is total cached input divided by total input for the group.

## Resource limits and errors

Each query has a 30 second deadline. At most four queries hold history read leases at
once; further queries return HTTP 503 with `history_query_busy`. Queries acquire a
fixed file snapshot and release the recorder mutex before scanning. Closed files
are opened one at a time, and the active file is read only through its captured
length. Recording and rotation continue during a scan.

The exact schema 2 end-ID index uses fixed 128-byte NUL-padded IDs. Up to 8 MiB of
IDs are sorted and deduplicated in memory. Larger indexes spill sorted runs to a
unique owner-only temporary directory, merge at most 31 input runs at a time, then
binary-search a single sorted file before an interrupted-start batch is queued. The
index and its temporary files belong to one snapshot query and are removed on query
exit; no persistent index is kept. A process crash can leave temporary files for the
operating system's temporary-directory cleanup. The remaining legacy and actual
interrupted-start batches can still rescan the archive, and large spilled indexes
also add temporary-disk and random-read I/O within the query deadline. The index
supports at most 4096 initial runs; exceeding that fixed bookkeeping bound returns a
query error. Temporary index build, read, merge, or cleanup failures return HTTP 500
with `history_query_index_unavailable`; the response suggests checking temporary
directory access and free disk space without exposing a scratch path. Context
deadlines continue to return the history query timeout response.

The query lease coordinates compression and date-range deletion. When exclusive
history maintenance is active, new queries return HTTP 503 with `history_maintenance`
rather than waiting. Closed `.ndjson.zst` segments are streamed through an external
`zstd -q -dc` child; queries check the child's exit status after EOF and cancel, close,
and reap it after early termination. If compressed history exists but `zstd` cannot be
started, queries return HTTP 503 with `compressed_history_unavailable` rather than
silently reading only raw segments. Compressed startup validation failures return
HTTP 500 with `compressed_history_invalid`; affected files remain preserved. Context
cancellation, malformed records, missing legacy attribution, read failures, and
timeouts return visible errors; a query does not silently omit unreadable segments.
The implementation can still query intact history when recording is degraded, as
long as the selected files can be read and decoded.


## WebUI history investigation

The Overview combines `/api/usage` across all readable history with a separate
`/api/requests?limit=100` timing sample. Usage known totals include only reported
values; known and unknown request coverage is shown per token field. Outcome rates
use all client requests as their displayed denominator. Performance values describe
the newest bounded sample and are not global latency percentiles.

The Requests page filters by the UTC half-open start-time range and exact
ServiceAccount, requested Model, actual Provider, upstream Model, and outcome values.
Configured IDs are suggested, and deleted historical IDs can be entered directly.
Results are requested in cursor pages of 25; the UI does not auto-fetch every page.

Request details keep incoming, effective-attempt, HTTP response, and SSE event bytes
separate. Body and event chunks replace the prior preview, show their byte position,
total and recording flags, and render only as text. A displayed chunk can split JSON,
SSE, UTF-8, or tool content, so it is not presented as a complete JSON document.
