# Implementation Handoff

## Implementation status

All T01–T17 tasks are implemented, reviewed, and committed. There are no remaining tasks in the accepted implementation plan. Read [README.md](../README.md) for running and packaging, and [implementation-plan.md](implementation-plan.md) for task-by-task verification. Live authenticated OpenAI and Pi use, native macOS execution, and the active-request deletion conflict at runtime remain unverified; these are verification limits, not claims of successful checks.

The user requested sequential fresh Luna XHigh implementation tasks, root review and commit, and no automated tests.

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
- For interrupted historical starts, use bounded batches and a second scan for matching ends rather than an unbounded request-ID map. Provide a bounded scan deadline and return an explicit timeout rather than partial results.
- Detail and event responses need explicit count/byte bounds. One event can be 64 MiB; avoid full stream arrays. Preserve exact incoming/effective JSON, raw events, upstream HTTP response bytes, IDs and timings.
- Aggregate client request counts separately from upstream attempt counts. Cached input and reasoning output are subsets; unknown counts remain distinct from zero. Cache ratio is unavailable if counts are unknown or input is zero.
- Segment filenames are **not chronological event order**. Recovery segments may use old request-start times. Fold records by elapsed offset/sequence and do not prune solely by segment filename.
- Context cancellation, error reporting, malformed records, and decompression failures must be visible.

## T14: implemented investigation UI

The implementation adds an all-history Overview usage/outcomes view, a latest-100
request timing sample, and a bounded Requests page with filters, cursor pagination,
request details, safe content previews, and observed timing offsets. The page uses
plain-text preview rendering and replaceable 256 KiB chunks; no automated tests were
added or run. Parent source review, four-target builds, JavaScript syntax checks and
manual fixture browser inspection passed. The fixture is artificial and does not
verify live OpenAI behavior.

Use the existing `history.QueryService` and management routes documented in `history-query.md`. Summaries expose `durationNs`, snake-case timing offsets, usage and historical identity snapshots. Event pages have base64 raw data, explicit omission/name-preview flags and raw chunk URLs. Body chunks return octet-stream with X-Content headers. Preserve byte offsets and decode UTF-8 carefully at chunk boundaries; do not load an entire long stream into an unbounded browser array.

Connect overview statistics and request filters/details to T13. Show active/completed/failed/cancelled/incomplete/interrupted requests, incoming/effective content, tools/events, usage and cache ratio, separate upstream/local IDs, and the observable timing stages. Unknown values need explicit presentation. Raw content must use safe text rendering. Request comparisons, cache-miss explanations, exports and automated analysis are deferred.

## T15: implemented external zstd and storage

Local CLI research confirmed `/usr/bin/zstd` v1.5.7 supports the `-q -c` compression and `-q -dc` decompression flags used by T15. Runtime resolves `zstd` from `PATH` during startup; no zstd library is used. Read [history-format.md](history-format.md) for startup pair recovery, source retention, temporary cleanup, and missing-executable behavior. The Storage page and `GET /api/storage` expose counts, bytes, compression availability/work/errors, recorder state and safe loss counters. After installing zstd into an initially unavailable environment, restart Gyemoim to validate and query existing compressed segments.

## T16: implemented date deletion

The accepted UX is **409 conflict with no changes when any in-progress request overlaps the selected range**. Do not wait for requests, cancel them, or suppress their logs. Delete all accounts and Models by each record's `started_at`, across all segments.

Use a UTC half-open interval; cap today's end at submission time. Under a brief admission/rotation lock, check overlap and capture a fixed file set, rotate the active prefix, and publish a durable roll-forward journal. Then release admission so new recording continues during rewrites. Move `Begin` timestamp capture under its admission lock to make the cutoff atomic. Serialize compression/deletion. New history queries during maintenance return clear 503 rather than wait. Rewrite raw or decompressed records with bounded memory, sync and atomically publish replacements; replay the idempotent journal after crashes. Remove the journal only after all selected files and directory changes are durable. Requests admitted after the snapshot must not be deleted.

The implementation adds the date form and `POST /api/storage/delete`; see [history-format.md](history-format.md) and [history-query.md](history-query.md) for journal recovery, request cutoffs, and API behavior. Parent source review, four-target builds, JavaScript syntax, selective deletion and replay API checks, and browser controls inspection passed. The active-request conflict path remains source-reviewed, not runtime-verified.

## T17: implemented packaging and documentation

The README and release script are present. README covers the local first-run workflow, Pi agent 1.0.0 configuration, generic local API calls, backup/restore and recovery, privacy, and optional external zstd. The script builds the four CGO-disabled targets with `-trimpath`, stages in the output directory, and updates only the four named output files. See `docs/implementation-plan.md` for review status and evidence.

A complete four-target release-script build, Bash syntax check, and diff whitespace check passed during implementation. Root reported native Linux startup/status, owner-only mode, process-lock and port-conflict, graceful Ctrl-C, and custom-port restart checks against the final T16 runtime. Root ran the final script to a path containing spaces, confirmed the four binary formats, and started the packaged Linux binary outside the repository; see the plan for details.

No automated tests were added or run. The cross-built macOS binaries have not been run on macOS. Live authenticated OpenAI sign-in, refresh, catalog, inference, and a complete Pi-to-Gyemoim session remain unverified. Parent source review accepted the final changes.

## Runtime and local references

- Linux data: absolute `$XDG_DATA_HOME/gyemoim`, else `~/.local/share/gyemoim`; macOS: `~/Library/Application Support/Gyemoim`. Owner-only directory/files and one process per data directory.
- SQLite uses `modernc.org/sqlite`; configuration and credentials only. History is separate.
- Single-target routing only, explicit grants only, no automatic retries or fallback. Revocation/disconnect affects new requests without administrative cancellation of admitted streams.
- Recording failure blocks new inference before Provider authentication/network. Already-admitted inference continues; lost records are reported.
- Maximum eight inferences, 64 MiB incoming body and individual SSE frame. Body-read and downstream-write deadlines are 30 seconds; no total SSE timeout.
- Installed pi 1.0.0 primary source: `/home/deploy/.nvm/versions/node/v24.19.0/lib/node_modules/@earendil-works/pi-coding-agent`, with nested `@earendil-works/pi-ai`. Exact field is per-model `thinkingLevelMap`, seven string/null keys; `off:null` suppresses default `none`. Both Responses compatibility flags in generated config are false.
- OpenAI docs skill: `/home/deploy/.codex/skills/.system/openai-docs/SKILL.md`. Browser skill: `/home/deploy/.agents/skills/agent-browser-core/SKILL.md`. Both were used in the previous session; follow skill activation instructions in a new session.
- Browser CLI: `/home/deploy/.nvm/versions/node/v24.19.0/bin/agent-browser`; use an isolated session and avoid displaying issued keys in snapshots/output.
