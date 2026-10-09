# AGENTS.md

## Verification — there is no test suite, by explicit project decision

Do not add or run `*_test.go` files. The accepted verification method is source review, builds, syntax checks, and manual API/browser inspection. For changes, run:

```sh
go vet ./...
CGO_ENABLED=0 go build -trimpath -o gyemoim ./cmd/gyemoim   # current platform
./scripts/build-release.sh                                   # all four targets: linux/darwin × amd64/arm64
node --check internal/httpui/assets/site.js                  # when UI JS changed
```

Then exercise changes by hand: run the binary (listens on http://127.0.0.1:9092/ by default, data directory auto-created) and call the API with curl. `--listen HOST:PORT` sets the bind address and `--port` remains a port-only alias (`--listen` wins when both are given); a second process on the same data directory fails on the process lock.

## Build constraints

- `CGO_ENABLED=0` is required for releases. SQLite is pure-Go `modernc.org/sqlite`; never swap in a CGO driver (e.g. `mattn/go-sqlite3`) — it would break all four cross-compiled targets. Requires Go 1.25+.
- The WebUI is embedded via `//go:embed assets/*` in `internal/httpui/ui.go`: plain HTML/CSS/JS in `internal/httpui/assets/`, no frontend toolchain. Rebuild the binary to see asset changes.

## Architecture

One Go module, one executable (`cmd/gyemoim/main.go`); all code lives under `internal/`:

- `httpapi` — routes: `/api/` management (CSRF-guarded), `/v1/` harness-facing Responses API, `/auth/callback` OAuth
- `gateway` — service-account authentication and single-target model routing
- `provider` — OpenAI Responses adapter; `siwc` — Sign in with ChatGPT OAuth
- `config` — SQLite `config.db` holds providers, OAuth credentials, key hashes, model routes, grants — the only SQLite data
- `history` — request history as NDJSON under `history/` (not SQLite): recorder, rotation, external zstd compression, date deletion, queries
- `httpui` (embedded UI), `websecurity` (CSRF/host guards), `processlock`, `datadir`

## Behavioral invariants — do not weaken these

- Model aliases route to exactly one provider + upstream model; service accounts need explicit grants (never auto-grant); no automatic retries or fallback.
- Upstream OpenAI requests always go out with `stream=true, store=false`; non-streaming client requests are collected. Unsupported request capabilities produce explicit errors — never silently drop them.
- If request recording fails, new inference is blocked (no unlogged bypass); already-admitted requests continue. Admission (with the bounded 8-inference semaphore) happens before body reads.
- Limits: 8 concurrent inferences, 64 MiB incoming body and per-SSE-frame, 30 s body-read/downstream-write deadlines, no total SSE timeout. History queries use a 30 s deadline and return an explicit timeout, not partial results.

## History domain gotchas

- Segment filenames are **not** chronological event order; fold records by elapsed offset/sequence. Both schema 1 and schema 2 records exist — never silently exclude legacy records or interrupted starts (starts without ends).
- Date-range deletion: UTC half-open interval; **409 conflict with no changes** if any in-progress request overlaps the range. Deletion is crash-safe via a roll-forward journal; queries return 503 while recovery/maintenance is pending.
- Compression shells out to the external `zstd` CLI from `PATH` — no zstd library.
- Concurrency: take a fixed file snapshot with a *brief* recorder lock; never hold the writer mutex while acquiring a file lease (or vice versa).

## Docs

`docs/` is authoritative: `design.md` (confirmed product decisions), `responses-contract.md`, `history-format.md`, `history-query.md`, `oauth.md`, `web-deployment.md` (remote web deployment decisions and deployment notes), `pi.md` (Pi agent config export), `implementation-plan.md` (task-by-task verification evidence). `next-session.md` and `code-review.md` are historical handoff/review records — their machine-specific paths (e.g. `/home/deploy/...`, worktrees, skill paths) refer to a previous environment, not this one.

Docs and UI text are in English.

## Live-upstream quirks (verified 2026-10-04)

- `api.openai.com/v1/responses` currently sends **no `Content-Type` header at all** — on SSE streams and JSON error responses alike. Provider code must not require the header; `Send` sniffs the body when it is absent. The request ID arrives as `x-oai-request-id` (canonical `X-Oai-Request-Id`), not `X-Request-Id`.

