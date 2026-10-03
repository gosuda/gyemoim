# Gyemoim Implementation Plan

This plan tracks implementation in small, reviewable tasks. T01 through T11 are complete. T12 through T17 are pending.

## Tasks

| ID | Scope | Status |
| --- | --- | --- |
| T01 | Runtime, embedded WebUI, automatic data-directory creation, and process lock | Complete (`c40b2b5`) |
| T02 | SQLite configuration and persistence | Complete |
| T03 | ServiceAccounts, local keys, Model grants and configuration, single-target routing strategy, and model-list API | Complete |
| T04 | Management UI for Providers, ServiceAccounts, Models, and status | Complete |
| T05 | Versioned NDJSON request recorder | Complete |
| T06 | Log rotation and crash-safe recovery | Complete |
| T07 | OpenAI OAuth login and credential registration | Complete |
| T08 | Token refresh and provider model catalog | Complete |
| T09 | OpenAI Responses provider contract and capability validation | Complete |
| T10 | Streaming gateway, cancellation, request IDs, and timing | Complete |
| T11 | Non-streaming responses, errors, and resource limits | Complete |
| T12 | pi agent 1.0.0 model metadata and connection configuration | Pending |
| T13 | History query and usage aggregation | Pending |
| T14 | Request investigation UI and timing details | Pending |
| T15 | External zstd compression and storage visibility | Pending |
| T16 | Date-range request-record deletion | Pending |
| T17 | Packaging and user documentation | Pending |

## Confirmed Implementation Decisions

- T02 uses SQLite through `modernc.org/sqlite` v1.59.0.
- Keep routing behind a Go strategy interface while T03 implements only one configured target per Model.
- Keep request history in NDJSON files outside SQLite. T05 introduces the recorder; T06 handles 64 MiB or one-hour rotation and crash-safe recovery. T15 checks for compression work once per minute.
- T15 invokes the external `zstd` executable, with no bundled zstd library, and checks for compression work once per minute.
- T11 caps request input and upstream SSE data at 64 MiB and limits concurrent inference to eight requests.
- T12 targets pi agent 1.0.0. Its initial integration supports reasoning models only through `models.json`.
- Optional advanced Model metadata is `contextWindow`, `maxTokens`, `input`, `reasoning`, and `supportedReasoningEfforts`. Populate it only from exact available metadata; do not guess values. Generate `models.json` only when the metadata is complete.
- The gateway implements the provider-supported Responses API contract, including for non-reasoning models; pi's initial metadata limitation does not narrow the gateway contract.

## Work and Review Workflow

1. Start each task with a fresh Luna X High implementation agent working only on that task.
2. Have the root agent review the task result. If changes are needed, reset or fix only that task's work before review is complete.
3. Commit a task after its review, then begin the next task.
4. If implementation details are missing, assign targeted research to agents and bring the findings back into the task review.
5. Per the user's request, do not add or run automated tests. Verify through code review, Go builds, and manual checks as appropriate to the task.

## Review Results

### T01

- Parent reviewed every source file and corrected relative XDG path handling and mobile navigation.
- CGO-disabled builds passed for linux/amd64, linux/arm64, darwin/amd64, and darwin/arm64.
- Manual Linux checks passed: startup/status, port validation, duplicate-process rejection, owner-only permissions, Host/Origin/CSRF rejection, and graceful SIGTERM shutdown.
- macOS execution has not been checked on a macOS host. No automated tests were added or run.

### T02

- Parent reviewed the full schema and repository, including credential transactions, foreign keys, key revocation lookup, and atomic Model revisions.
- CGO-disabled builds passed for Linux/macOS on amd64/arm64.
- Manual checks passed for startup/status, WAL/schema version, owner-only DB/WAL/SHM files, stable host ID after restart, and rejection of schema version 99.
- No automated tests were added or run.

### T03

- Reviewed management endpoints, hashed local keys, explicit grants, transactional authorization snapshots, and the single-target strategy interface.
- Corrected concurrent Provider rename handling, referenced Provider deletion, JSON errors, and default HTTP-port origin validation.
- CGO-disabled builds passed for Linux/macOS on amd64/arm64. Manual checks passed for key issuance/revocation, permitted model listing, grant rollback, Model revision increments, conflict/error responses, and secret exclusion.
- No automated tests were added or run.

### T04

- Implemented embedded management pages for runtime status, Providers, ServiceAccounts, and Models; reviewed and accepted.
- Connected Provider, account, key, grant, and Model forms to the same-origin management API with CSRF headers. OAuth sign-in remains unavailable until T07; the UI explains this and keeps its connection action disabled.
- Key plaintext is shown only in the issuance response panel, with copy and dismiss-and-clear actions. Model edits carry existing metadata through the update request for later T12 fields.
- No automated tests added or run.

- Parent inspected all embedded assets and manually exercised browser creation, grants, key issuance/revocation, navigation clearing, and the mobile layout. Native CGO-disabled build and JavaScript syntax check passed.


### T05

- Implemented the version 1 NDJSON recorder, append-only active file, admission/transmission/end durability fences, response-event framing, request usage/timing metadata, sticky storage degradation, and safe status counters.
- Connected recorder startup and close to the HTTP lifecycle; startup remains available when history storage cannot open, while new recorder admissions fail. The runtime status page reports history health and counters.
- Added the exact schema and lifecycle notes in [history-format.md](history-format.md). Parent reviewed recorder lifecycle, durability fences, loss accounting, and integration. CGO-disabled Linux/macOS amd64/arm64 builds passed; manual healthy/degraded startup and owner-only history permissions passed. No automated tests were added or run.

### T06

- Added 64 MiB / one-hour active-file rotation at record boundaries and startup rotation of recovered nonempty active files. Closed segments use sortable UTC timestamps plus unique random suffixes; publication syncs files and the history directory around rename/create operations.
- Startup validates every closed segment and active record with a 512 MiB bounded line reader, truncates and syncs only an incomplete active tail, and exposes recovered-tail bytes in safe status. Malformed complete records and closed tails degrade the recorder without rewriting them.
- Added `ReadRecords` for bounded callback-based decoding and `ClosedSegments` for sorted safe metadata snapshots. No lifecycle map reconstruction, synthetic end records, compression, deletion, or automated tests are included. Parent manual checks passed for partial-tail recovery, complete malformed-line preservation, and startup availability. CGO-disabled Linux/macOS amd64/arm64 builds passed. Parent accepted T06 (`eceb3e6`).


### T07

- Added lazy Sign in with ChatGPT discovery and a bounded, expiring, single-use state map with per-Provider latest-flow behavior. Initial registration and returning sign-in use their respective client IDs and hints; authorization uses nonce and PKCE S256.
- Added callback token exchange, OIDC signature/issuer/audience/expiry/issue-time checks, nonce and subject continuity validation, verified-email retention, authoritative granted-scope status, and atomic credential/status persistence. OAuth secrets remain outside Provider API responses.
- Added CSRF-protected same-origin OAuth start, an origin-exempt callback behind the global exact Host guard, and full-tab Provider UI sign-in with fixed callback-result notices. Documented the exact flow and unverified live-account checks in [oauth.md](oauth.md).
- No live account was available. A live sign-in and confirmation of the provider-issued callback fields and real token response remain unverified. T08 refresh, disconnection, and catalog behavior are not included. No automated tests were added or run.

- Parent reviewed OAuth manager, persistence, guards, and UI changes. CGO-disabled Linux/macOS amd64/arm64 builds and JavaScript syntax passed. Live-discovery authorization configuration, denied callback redirect, and invalid-state/error-reflection checks passed. Authenticated sign-in remains unverified.


### T08

- Added a cancellable per-provider lock shared by OAuth start/callback, token refresh, and disconnect. Internal callers receive only a currently valid direct-use bearer token; refresh runs on demand within 60 seconds of expiry and atomically replaces the rotating token set.
- Terminal refresh-token errors and malformed successful rotations clear unusable credentials and mark the saved registration for reauthentication. Transient transport/server errors preserve local credentials; invalid-client errors remain configuration errors.
- Added same-origin provider disconnect and account-specific model catalog endpoints. Disconnect invalidates pending flow state, clears local tokens before best-effort remote revocation, and keeps the issued client registration. The catalog exposes only visible model slugs and display names from the SIWC response.
- Updated the Provider and Models UI plus OAuth docs. No live OpenAI account was available to verify refresh rotation, remote revocation, or model availability. No automated tests were added or run.

- Parent reviewed credential rotation, cancellation, disconnect serialization, safe catalog errors, transport reuse, and UI request generations. All four CGO-disabled Linux/macOS builds passed. Manual disconnected-catalog and pending-flow-disconnect checks passed; live refresh/revocation/catalog remain unverified.


### T09 implementation (pending parent review)

- Added a provider-layer Go adapter interface and fixed-host OpenAI Responses implementation. The adapter validates the researched SIWC capability boundary, preserves unknown JSON fields and supported flat tools, normalizes only model/stream/store, returns typed 400-capability errors, and does not read tokens from storage.
- Added a reusable HTTP transport with endpoint restriction, redirect refusal, dial/TLS/header bounds, no overall SSE timeout, explicit gateway-owned headers, bounded raw non-2xx bodies, selected safe response metadata, and sanitized transport errors.
- Added bounded incremental SSE frame reading, raw-frame preservation, terminal response JSON and usage extraction, UTF-8 checks, and monotonic connection/transmission/event/output/completion trace offsets. Added [responses-contract.md](responses-contract.md).
- No automated tests were added or run. No live authenticated account is available, so wire behavior remains unverified.

- Parent reviewed the complete adapter and corrected capability traversal, transport error handling, terminal event retention, and recorder-aligned timing. CGO-disabled Linux/macOS amd64/arm64 builds passed. Live authenticated inference remains unverified; no automated tests were added or run.


### T10

- Added `POST /v1/responses` with local bearer authentication, one immutable Model target, a maximum of eight active inference requests, and a 64 MiB bounded request body. The gateway records an authorized valid JSON request before capability checks or SIWC token preparation, then syncs the effective upstream request before sending it.
- Streams complete upstream SSE frames synchronously for backpressure, preserving original frame bytes. Each downstream write has a 30-second write deadline; incoming request bodies have a 30-second read deadline. Active SSE streams have no overall timeout. Client cancellation closes the upstream request context.
- Records terminal outcomes, usage, auth-preparation and provider timing offsets, downstream delivery, connection acquisition/reuse, a separate upstream request ID, and full bounded raw HTTP error bodies. Schema 2 end records contain small self-contained account, Model, and actual Provider attribution snapshots; the reader accepts schema 1 and 2. A recording loss after admission does not stop the live stream, while new admissions fail before provider authentication when storage is already degraded.
- Until T11, `stream:false` returns a clear 400 unsupported response. Upstream HTTP errors currently map to a safe 502; T11 can add provider-status-specific error mapping. No retries or fallback attempts are made. No automated tests were added or run.

- Parent reviewed all changes and corrected selection-interface use, typed authentication errors, incomplete EOF handling, partial-frame forwarding, and error-path timing. CGO-disabled Linux/macOS amd64/arm64 builds passed. Manual local API checks passed for capability rejection, nonstream rejection, disconnected Provider errors, request IDs, and schema 2 attribution. Live authenticated streaming remains unverified.


### T11

- Added bounded non-streaming Responses collection and terminal JSON delivery, retaining complete/incomplete response semantics and terminal usage. Preserved raw frames and errors in history.
- Added safe status-specific upstream error mapping and validated Retry-After forwarding. Provider authorization errors are distinct from local-key errors. Both delivery modes reject mismatched terminal events and record cancellation when downstream delivery fails.
- The implementation agent stopped after source changes because of a usage limit. Parent completed small review fixes and documentation; no automated tests were added or run.
- CGO-disabled builds passed for Linux/macOS amd64/arm64. Manual local API inspection confirmed both delivery preferences reach Provider authentication and history remains ready after restart. Live authenticated nonstreaming and upstream error mapping remain unverified.
