# Request History Format (writer schema version 2)

Gyemoim stores request history outside SQLite as newline-delimited JSON in
`<data-directory>/history/active.ndjson`. Each line is one compact JSON object
followed by `\n`. The history directory is mode `0700`; active and closed files are
mode `0600`.

The active file rotates at record boundaries when it reaches 64 MiB or one hour old.
A record that crosses 64 MiB stays whole, so one JSON line may overshoot the target.
A one-minute ticker rotates an idle file after the age limit. Every nonempty active
file found at startup is validated, then published as a closed segment before a fresh
active file is created. This keeps closed files immutable for later history readers
and compression.

Closed raw files are named
`segment-<UTC-start-time>-<random-suffix>.ndjson`, for example
`segment-20261003T120000.123456789Z-<32 lowercase hex digits>.ndjson`. The timestamp
identifies segment creation time (or the first recovered record's `started_at`
when publishing an active file after restart); the random 128-bit suffix prevents filename collisions.
Names sort lexically by segment start time. Publication syncs the active file, closes
it, renames it, syncs the history directory, creates a new owner-only active file, and
syncs the directory again. The directory is also synced when first created.

Closed segments may be stored as either their raw `.ndjson` file or as a same-basename
`.ndjson.zst` file. Once per minute, after startup recovery is complete, the recorder
checks for closed raw segments and invokes the external `zstd` program with `-q -c`.
It writes stdout to an owner-only, uniquely named temporary file in the history
directory, syncs that file, renames it to the final `.zst` name, syncs the directory,
then removes the raw source and syncs the directory again. Failures before publication,
and failures after publication but before source removal, leave the raw source in
place. Published pairs are recorded as verified so history queries do not count them
twice; a later minute check retries source cleanup. If removal succeeds but the last
directory sync fails, a later minute check syncs the compressed copy and directory
again. If a crash restores the raw directory entry, startup verifies the pair again.

On startup, every compressed segment is streamed through `zstd -q -dc` and validated
with the same bounded record decoder as raw history. A raw/compressed pair is compared
by decompressed byte length and SHA-256 before the source can be removed or omitted
from a query snapshot. Mismatches and malformed compressed files are preserved and
degrade recorder startup. If `zstd` is unavailable, raw recording still opens, but
queries that encounter compressed segments return an explicit unavailable error and
status marks compressed validation as pending. Installing `zstd` after startup
requires a restart before compressed history is validated and queried.

Known crash-left compression temporary files use
`<segment>.ndjson.zst.tmp-<32 lowercase hex digits>` names. Startup cleanup removes
only matching regular files and never follows or removes links. Unknown files are
left untouched.

Startup scans each closed segment and the active file one bounded line at a time.
Records are limited to 512 MiB to allow JSON escaping of the largest 64 MiB UTF-8 SSE
frame (up to roughly 384 MiB before record overhead). A complete line with malformed
JSON, an unsupported schema version/type, or invalid schema fields degrades history
and prevents append; the source file is preserved. A final active-file fragment with
no newline is treated as an incomplete crash tail: only that fragment is truncated,
and the file is synced. Incomplete tails in closed segments are errors and are never
truncated. `recoveredBytes` reports the number of active-tail bytes removed this run.
The reader validates individual records without rebuilding request lifecycle state;
a request lacking `request_end` remains an interrupted request for later readers to
infer. No synthetic end records are written at startup.

## Fields shared by every record

| Field | Type | Meaning |
| --- | --- | --- |
| `schema_version` | integer | New records use `2`. The reader accepts legacy schema `1` and schema `2`. |
| `type` | string | `request_start`, `upstream_transmission`, `upstream_response`, `response_event`, or `request_end`. |
| `request_id` | string | Gateway request ID correlating all records for one request. |
| `started_at` | UTC timestamp | Wall-clock timestamp captured at recorder admission and repeated unchanged on every line. |
| `timestamp_utc` | UTC timestamp | Wall-clock time when this record was written. |
| `elapsed_ns` | integer | Monotonic nanoseconds since recorder admission; never derived from wall-clock subtraction. |

Timestamps use RFC 3339 with nanosecond precision. Records are serialized under one
mutex, so each JSON object stays on one line even when requests run concurrently.

## Record types

### `request_start`

- `service_account`: `{ "id", "name" }`, admission-time snapshot.
- `model`: `{ "id", "name", "version", "strategy" }`, admission-time snapshot.
- `incoming_request`: the complete incoming request JSON body.

The request ID can be supplied by the caller or generated by the recorder. The caller
must use `Request.ID()` for `X-Request-ID`. `Begin` writes and syncs this record before
returning an admitted request handle; the caller must not invoke a provider without a
successful handle.

### `upstream_transmission`

- `attempt`: positive, increasing attempt number.
- `provider_id`, `provider_name`, `upstream_model`: actual target for this attempt.
- `effective_request`: the complete effective upstream request JSON body.

`Transmit` writes and syncs this record before the caller sends the upstream request.

### `upstream_response` (schema 2)

- `attempt`: attempt whose HTTP response was received.
- `http_status`: actual upstream HTTP status.
- `content_type`: sanitized upstream `Content-Type` header value, empty when the
  upstream sent no header. An empty value is meaningful: the live Responses endpoint
  currently omits the header on streaming and error responses alike.
- `upstream_request_id`: validated provider request ID, if supplied.
- `body`: exact raw response bytes encoded by JSON as base64. It is bounded to 64 MiB.
- `body_truncated` and `body_read_failed`: indicate that the adapter could not retain a
  complete body because of the size cap or a read failure. Both can be true if the body
  exceeded the cap and reading also failed.

This record preserves non-2xx bodies and successful responses with an unexpected
content type. It contains no managed authorization token and no upstream response
headers other than the sanitized `content_type` and request-ID values above.
The following `request_end` record provides the durability fence.

### `response_event`

- `attempt`: most recently recorded upstream attempt.
- `event_name`: parsed SSE event name (empty when absent).
- `sequence`: per-request sequence starting at one.
- `wire_text`: the complete UTF-8 SSE frame, including its original framing bytes.

Each frame is written immediately. A single frame may be at most 64 MiB; the limit is
per frame, not per stream. Event records are synced by the next durable fence or close.

### `request_end`

- `attempt`: last upstream attempt, or `0` if none was transmitted.
- `outcome`: `completed`, `failed`, `cancelled`, or `incomplete`.
- `http_status`: response status, or `0` if none was available.
- `safe_error`: a short pre-sanitized explanation, empty when none applies.
- `usage`: `null` when unavailable, or an object with `input_tokens`, `output_tokens`,
  `cached_input_tokens`, and `reasoning_output_tokens`.
- `service_account`, `model`, and `provider`: small admission and actual-target snapshots
  written by schema 2 so attribution scans can read end records without joining request
  starts or transmissions. `provider` is absent only when no attempt was transmitted.
- `upstream_request_id`: safe provider request ID, separate from the gateway `request_id`.
- `timings`: offsets `authentication_preparation_ns`, `connection_requested_ns`,
  `connection_established_ns`, `request_transmission_ns`, `first_event_ns`,
  `first_output_ns`, `stream_completion_ns`, `downstream_delivery_ns`, and the optional
  `connection_reused` boolean. `connection_requested_ns` and `connection_reused` are
  written by schema 2. `downstream_delivery_ns` is the last successfully flushed frame.

A missing usage count or timing is JSON `null`; a reported count of zero is `0`.
Cached input and reasoning output are subsets of their respective totals. `End` writes
and syncs the final record, then unregisters the request even if recording has failed.
A repeated `End` is harmless. Closing the recorder ends remaining handles as
`incomplete` when it can write them.

## Durability, errors, and sensitive fields

`Begin`, `Transmit`, and `End` each sync the active file before returning. Any write or
sync failure permanently degrades this process's recorder. Existing requests continue
upstream; their later history writes fail and increment the safe loss counter. Future
`Begin` calls return `ErrRecordingUnavailable`, allowing the gateway to reject new
requests before provider invocation. `Status` exposes only state, potentially-lost-record count, recovered-tail byte
count, active-request count, and bytes in the current active file.

The schema has no HTTP authorization header, local API key, OAuth credential object,
authentication URL, or generic URL field. New records use schema 2; readers continue to
accept schema 1 without requiring schema 2 attribution fields. Callers pass JSON request bodies only;
arbitrary user-provided body content is preserved. Callers must keep gateway-managed
credentials out of body payloads and must sanitize `safe_error` before recording it.

`ReadRecords` is the shared bounded-memory record decoder for startup validation and
later history readers. It passes one decoded record to its callback at a time and
honors context cancellation between records. The exported closed-segment snapshot
returns sorted filenames and metadata, not absolute paths; active data is excluded.
Any later maintenance operation that rewrites or deletes closed files must serialize
with that operation's own snapshot and rotation coordination.

## Date-range deletion and recovery

`POST /api/storage/delete` accepts `firstDate` and `lastDate` as strict `YYYY-MM-DD`
UTC dates, inclusive. The effective interval is `[firstDate 00:00Z,
min(day-after-lastDate 00:00Z, request-submission-time))`. Dates after the current UTC
date, reversed dates, and empty effective ranges are rejected. This keeps requests
started later on the current UTC day outside a deletion that includes today.

The recorder compares the effective interval against all in-progress requests using
`started_at`. If any overlap, the API returns HTTP 409 before rotating or changing
history files. `Recorder.Begin` captures its timestamp under the same recorder mutex
used for the deletion cutoff and snapshot. Deletion applies across all accounts and
Models; no identity-specific deletion is available.

After acquiring the exclusive history-maintenance lease and checking active requests,
the recorder briefly holds its writer mutex to rotate the current active prefix,
capture the fixed closed-segment basenames, and durably publish the owner-only
`.history-delete.json` roll-forward journal. It releases that mutex before reading
segment contents. New queries receive HTTP 503 during maintenance; new inference
recording can continue in the fresh active file.

Each fixed segment is streamed one bounded NDJSON record at a time. The record is
decoded to validate it and inspect `started_at`; retained record lines are written
with their original bytes, without JSON reserialization. A raw replacement temporary
file is synced and atomically renamed, then its directory is synced. If a raw and
compressed pair exists, deletion uses the compressed representation as the source,
streams it through external `zstd -q -dc`, publishes the filtered raw file, durably
checkpoints that segment in the journal, and only then removes the `.zst` source and
syncs the directory. The resulting raw segment is eligible for the normal compression
worker again.

The journal stores only validated closed-segment filenames and the effective range.
Per-file completion checkpoints make replay idempotent across crashes before or after
replacement, checkpoint publication, and compressed-source removal. A missing file is
accepted only where the durable checkpoint establishes that its replacement was
published; otherwise replay stays pending. Journal and rewrite temporaries use unique
owner-only names that compression cleanup does not consume.

Startup replays the journal before closed-file validation, compressed-pair recovery,
or the compression worker starts. If replay cannot finish (including when zstd is
unavailable for a captured compressed segment), Gyemoim keeps the journal and the
maintenance lease, exposes `pending_recovery` and safe progress/error status through
`GET /api/storage`, and returns HTTP 503 to new history queries. Unrelated closed
segments are still validated. Raw inference recording stays available if the writer
itself is healthy. Installing zstd after a pending compressed deletion requires a
restart to retry replay. A shutdown cancels and joins the bounded deletion service
context; any unfinished roll-forward is completed at the next startup.

The deletion result reports the effective `from` and exclusive `to` timestamps,
processed segment count, and removed history-record count. The record count is stored
in the per-segment checkpoint before replacement, so startup replay retains it across
a crash after the replacement rename.

A read-only deletion preview (`GET /api/storage/delete-preview`, documented in
`history-query.md`) reuses the deletion's interval computation and record-reading
machinery without changing files. It skips closed segments — raw or compressed — whose
modification time plus a 2-second margin precedes the range start: closed segments are
immutable, so the mtime of a raw segment bounds its last write and the mtime of a
`.zst` segment bounds the later compression time, and a record's `started_at` never
exceeds its write time. Skipped segments are neither opened nor decompressed. There is
no symmetric skip on the upper side: a request started before the range end may keep
writing continuation records into later segments, and those records are part of what
the deletion removes.
