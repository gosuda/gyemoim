# Implementation Handoff

## Resume point

Finish T14 through T17 in [implementation-plan.md](implementation-plan.md). T01–T13 are implemented; the plan records review and verification details. Read [design.md](design.md), [history-format.md](history-format.md), [responses-contract.md](responses-contract.md), and [pi.md](pi.md) before making changes.

The previous session reached the agent thread limit. The user explicitly chose to continue the remaining implementation in a **new session with fresh Luna XHigh agents**, rather than have the root implement the remaining tasks directly.

## Required workflow

- Use a fresh `gpt-6-luna` agent with `xhigh` reasoning and `fork_turns: none` for each implementation task. Pass the exact worktree path and task constraints.
- Implement tasks sequentially. Root reviews every change, requests revisions or directly fixes small issues, commits the accepted task, fast-forwards `master`, then starts the next task. Reset only a task's own changes if its design needs replacement.
- Delegate missing-information research. Read-only research can overlap implementation.
- **Do not add or run automated tests.** The user selected code review, builds, syntax checks, and manual API/browser inspection.
- Worktrees are under `/home/deploy/git/.gyemoim-worktrees/tXX`, with branches `implement/tXX`. Main repository is `/home/deploy/git/gyemoim`.
- Use shell tools with an explicit worktree working directory. Relative `apply_patch` previously wrote into the main repository; do not use it for worktree edits.
- Provide concise Korean progress updates. Documents and UI are in English.

## T13: implemented query foundations

T13 is complete; read [history-query.md](history-query.md) for the API. It implements request list/filter, request details/event pagination, and aggregates over NDJSON without retaining all requests or events in memory. Schema 2 ends contain self-contained account/Model/actual Provider attribution and usage/timings. Read schema 1 too; do not silently exclude legacy records or interrupted starts without ends.

Implemented boundaries and review context:

- Obtain a fixed file snapshot with a **brief** recorder lock: open the active file and capture its byte length plus closed-file names; read only that prefix. Rotation can rename the open file without invalidating its descriptor. Do not hold the writer mutex during a scan.
- Introduce a separate file lease/maintenance gate for query lifetime, later compression and deletion. Keep lock order consistent; the writer must not acquire a file lease while holding its mutex.
- Scan ends once with a bounded page heap, ordering by request start descending then stable request ID. Filter by start range, ServiceAccount, Model, actual Provider/upstream model, and outcome. Use small snapshots for active requests.
- For interrupted historical starts, use bounded batches and a second scan for matching ends rather than an unbounded request-ID map. Provide a bounded scan deadline and an honest partial/cursor result if needed.
- Detail and event responses need explicit count/byte bounds. One event can be 64 MiB; avoid full stream arrays. Preserve exact incoming/effective JSON, raw events, upstream HTTP response bytes, IDs and timings.
- Aggregate client request counts separately from upstream attempt counts. Cached input and reasoning output are subsets; unknown counts remain distinct from zero. Cache ratio is unavailable if counts are unknown or input is zero.
- Segment filenames are **not chronological event order**. Recovery segments may use old request-start times. Fold records by elapsed offset/sequence and do not prune solely by segment filename.
- Context cancellation, error reporting, malformed records, and decompression failures must be visible.

## T14: investigation UI

Use the existing `history.QueryService` and management routes documented in `history-query.md`. Summaries expose `durationNs`, snake-case timing offsets, usage and historical identity snapshots. Event pages have base64 raw data, explicit omission/name-preview flags and raw chunk URLs. Body chunks return octet-stream with X-Content headers. Preserve byte offsets and decode UTF-8 carefully at chunk boundaries; do not load an entire long stream into an unbounded browser array.

Connect overview statistics and request filters/details to T13. Show active/completed/failed/cancelled/incomplete/interrupted requests, incoming/effective content, tools/events, usage and cache ratio, separate upstream/local IDs, and the observable timing stages. Unknown values need explicit presentation. Raw content must use safe text rendering. Request comparisons, cache-miss explanations, exports and automated analysis are deferred.

## T15: external zstd and storage

Use an external process only; do not research or add a zstd library. Missing `zstd` keeps raw recording functional and status explains compression unavailable/pending. Check closed files once per minute. Compress to owner-only same-directory temporary output, sync, rename, sync directory, then remove source and sync directory. Keep source on failure. Verify a crash-window compressed/source pair represents identical data before removing or deduplicating either copy. Use bounded streaming verification.

Read compressed records through `zstd -dc`; close pipes, cancel and reap children on early query termination. Serialize compression with deletion and coordinate file queries using T13's lease. Show raw/compressed bytes, pending/failed work and availability. Startup validation must cover compressed history as well as raw history; avoid duplicate request counts when both representations exist. Exact CLI flags and process handling still require delegated research.

## T16: date deletion

The accepted UX is **409 conflict with no changes when any in-progress request overlaps the selected range**. Do not wait for requests, cancel them, or suppress their logs. Delete all accounts and Models by each record's `started_at`, across all segments.

Use a UTC half-open interval; cap today's end at submission time. Under a brief admission/rotation lock, check overlap and capture a fixed file set, rotate the active prefix, and publish a durable roll-forward journal. Then release admission so new recording continues during rewrites. Move `Begin` timestamp capture under its admission lock to make the cutoff atomic. Serialize compression/deletion. New history queries during maintenance return clear 503 rather than wait. Rewrite raw or decompressed records with bounded memory, sync and atomically publish replacements; replay the idempotent journal after crashes. Remove the journal only after all selected files and directory changes are durable. Requests admitted after the snapshot must not be deleted.

## T17: packaging and documentation

Add a beginner README and small build/release script for CGO-disabled Linux/macOS amd64/arm64 binaries. Assets are already embedded. No installers/service managers needed. Source requires Go 1.25 or newer. Document default `127.0.0.1:9092`, `--port`, first-run UI workflow and exact T12 pi configuration, stop/restart, data paths, complete stopped-service backup/restore and recovery behavior, external optional zstd, full-body recording and SQLite credentials.

Outgoing OAuth/catalog/Responses transports explicitly use `Proxy=nil`; HTTP(S)_PROXY is not supported for outbound OpenAI calls. This is distinct from harness proxy bypass for loopback. No license has been selected; do not invent one.

Build all four targets and manually inspect the native Linux startup, status, permissions, port/process conflicts and graceful shutdown. No automated tests. Live authenticated OpenAI sign-in/refresh/catalog/inference and live pi usage have **not** been verified. No native macOS runtime check is available; cross-compilation is not a runtime check. Keep these limitations explicit.

## Runtime and local references

- Linux data: absolute `$XDG_DATA_HOME/gyemoim`, else `~/.local/share/gyemoim`; macOS: `~/Library/Application Support/Gyemoim`. Owner-only directory/files and one process per data directory.
- SQLite uses `modernc.org/sqlite`; configuration and credentials only. History is separate.
- Single-target routing only, explicit grants only, no automatic retries or fallback. Revocation/disconnect affects new requests without administrative cancellation of admitted streams.
- Recording failure blocks new inference before Provider authentication/network. Already-admitted inference continues; lost records are reported.
- Maximum eight inferences, 64 MiB incoming body and individual SSE frame. Body-read and downstream-write deadlines are 30 seconds; no total SSE timeout.
- Installed pi 1.0.0 primary source: `/home/deploy/.nvm/versions/node/v24.19.0/lib/node_modules/@earendil-works/pi-coding-agent`, with nested `@earendil-works/pi-ai`. Exact field is per-model `thinkingLevelMap`, seven string/null keys; `off:null` suppresses default `none`. Both Responses compatibility flags in generated config are false.
- OpenAI docs skill: `/home/deploy/.codex/skills/.system/openai-docs/SKILL.md`. Browser skill: `/home/deploy/.agents/skills/agent-browser-core/SKILL.md`. Both were used in the previous session; follow skill activation instructions in a new session.
- Browser CLI: `/home/deploy/.nvm/versions/node/v24.19.0/bin/agent-browser`; use an isolated session and avoid displaying issued keys in snapshots/output.
