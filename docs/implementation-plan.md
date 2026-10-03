# Gyemoim Implementation Plan

This plan tracks implementation in small, reviewable tasks. T01 is complete. T02 through T17 are pending.

## Tasks

| ID | Scope | Status |
| --- | --- | --- |
| T01 | Runtime, embedded WebUI, automatic data-directory creation, and process lock | Complete (`c40b2b5`) |
| T02 | SQLite configuration and persistence | Complete |
| T03 | ServiceAccounts, local keys, Model grants and configuration, single-target routing strategy, and model-list API | Pending |
| T04 | Management UI for Providers, ServiceAccounts, Models, and status | Pending |
| T05 | Versioned NDJSON request recorder | Pending |
| T06 | Log rotation and crash-safe recovery | Pending |
| T07 | OpenAI OAuth login and credential registration | Pending |
| T08 | Token refresh and provider model catalog | Pending |
| T09 | OpenAI Responses provider contract and capability validation | Pending |
| T10 | Streaming gateway, cancellation, request IDs, and timing | Pending |
| T11 | Non-streaming responses, errors, and resource limits | Pending |
| T12 | pi agent 1.0.0 model metadata and connection configuration | Pending |
| T13 | History query and usage aggregation | Pending |
| T14 | Request investigation UI and timing details | Pending |
| T15 | External zstd compression and storage visibility | Pending |
| T16 | Date-range request-record deletion | Pending |
| T17 | Packaging and user documentation | Pending |

## Confirmed Implementation Decisions

- T02 uses SQLite through `modernc.org/sqlite` v1.59.0.
- Keep routing behind a Go strategy interface while T03 implements only one configured target per Model.
- Keep request history in NDJSON files outside SQLite. T05 introduces the recorder; T06 handles 64 MiB or one-hour rotation and crash-safe recovery.
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

### T02

- Implemented on top of T01 base `c40b2b5`; pending parent review.
- Added the versioned SQLite configuration store, typed repository methods, stable host UUID, credential separation, live SQLite readiness in status, and manual schema inspection instructions.
- `CGO_ENABLED=0 go build ./...` passed. Manual startup/status, owner-only database and directory permissions, WAL/schema version, table set, restart-stable host ID, and newer-schema rejection checks passed; no automated tests were added or run.

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
