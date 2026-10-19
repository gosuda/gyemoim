# Routing selection and runtime observations

The harness executes `single` routes by default and `fallback` routes only when
explicitly configured through the management API or Models editor. Both Responses and Chat
Completions, streaming and collected, use the same executor. Each request holds
one admission slot and recorder handle across all candidates; no probes run.

## Execution and commitment

The executor reads the incoming body once (64 MiB maximum), snapshots the route,
and replays the same request through `Prepare` with each selected upstream model.
Chat translation is performed once; history retains the original chat request and
each effective Responses request. Dropped fields are deduplicated per request.
Upstream requests always use `stream=true` and `store=false`.

Single retains its pre-admission local provider-readiness lease, with no token
refresh before durable admission. Fallback acquires each candidate's lease after
durable `StartAttempt`. Both release the lease before `Transmit` and upstream I/O.
The lease rechecks connection changes after the route snapshot.

Each skip, start, attempt end and transmission is a durable fence. Event and HTTP
body records are bound to the current transmission. A failed fence or recording
callback prohibits advancement, though already-transmitted streams may continue.
Each upstream is closed before its attempt ends and before another decision.
Candidate numbers are configured position plus one; transmission numbers count
successful send fences, so pre-send auth failures and skips consume none.

Streaming reads and validates events incrementally, delaying HTTP headers/flush
until the first deliverable frame (or chat chunk). An eligible failure before
that point may advance. Any attempted downstream header/body commitment forbids
fallback, even if delivery then fails. Unknown initial model errors remain
terminal; Responses streaming may deliver the original terminal frame.
Non-streaming requests may advance before their final JSON/error is committed.
Valid incomplete results, including token-limit results, do not cause fallback.
Cancellation or request-context expiry stops without a target cooldown. No total
SSE timeout is imposed.

Delayed commitment also applies to `single`: a stream read/format failure before
the first deliverable event now returns a JSON error with its safe HTTP status,
rather than committing HTTP 200 and then emitting an SSE error. After commitment,
errors remain SSE-framed and never switch targets. Non-streaming read errors retain
their established codes: `upstream_frame_too_large` (502),
`upstream_protocol_error` for invalid UTF-8/EOF (502), `upstream_timeout` (503),
and `upstream_read_error` (502). These client envelopes are independent of the
explicit failure classification used for fallback eligibility and cooldowns.

One gateway request ID spans every attempt. Only the final/delivered attempt's
upstream request ID and validated `Retry-After` are exposed to the client. After
attempt exhaustion its last safe error is preserved. An all-skipped cooling route
returns 503 with the minimum connected eligibility delay in `Retry-After`; an
all-disconnected route returns 503 without that header.

## Authorized snapshot and selection

`Service.ResolveRoute` revalidates the local key, enabled account, and explicit
model grant in the same brief SQLite read transaction as the model revision,
ordered targets, and **all** referenced providers. `ResolvedRoute.Providers` is
indexed by provider ID; legacy `Provider` and model target fields project the
first target. Transactions have ended before selection or provider work. Token
leases and credential resolution remain executor responsibilities, not selector
work. A connection change after the snapshot must still be handled by the
executor's normal authentication boundary.

`Selection.Next` retains single behavior, without consulting cooldowns. Both
strategies implement additive `DecisionSelection.NextDecision(ctx, previous)`:

- A decision contains the zero-based priority, target, `kind` (`attempt` or
  `skip`), and observed state. Fallback returns **one candidate per call**, in
  configured priority order, at most once per request.
- A disconnected or cooling fallback candidate returns a skip. The executor
  must durably write `Request.SkipCandidate` **before** requesting another decision
  or doing any provider work. After a skip pass `nil` as the previous outcome.
  If recording fails, stop; never request another decision or send upstream.
  Map `Position + 1` to the history `Candidate.Number` and use the snapshotted
  provider name/ID plus selected upstream model. `State.Status` supplies a safe
  skip reason (`cooling` or `disconnected`).
- After an attempt, continuation requires `AttemptOutcome.Failure` containing
  an explicit `Failure`. Legacy `Outcome`/`HTTPStatus` alone do not permit retry.
  Commitment, client cancellation, and history failure gates exist both on the
  outcome and failure facts. They always stop continuation. Context cancellation
  also stops selection. Terminal selection cannot be restarted with a nil outcome.
- Unknown failures, request/capability errors, and non-retryable upstream statuses
  stop. Successful/incomplete results are not retryable failures.
- Fallback's legacy `Next` returns `ErrDecisionRequired`: it never silently
  discards skips. Unknown strategies return `ErrUnsupportedStrategy`.
- After only skips, `SelectionUnavailable` wraps `ErrTargetsCooling` when any
  connected candidate is cooling, with the **minimum connected** `EligibleAt`
  and ceiling-rounded `RetryAfterSeconds(now)`. Disconnected candidates do not
  contribute to that delay. If none are connected it wraps
  `ErrTargetsDisconnected` and supplies no retry delay. After actual attempts,
  normal exhaustion is `ErrSelectionExhausted`; preserve the last attempt error.

Single decisions expose observed state but **never skip due to cooldown**. Their
executor retains the existing connection/authentication checks. Direct legacy
`SingleTargetStrategy` construction without runtime reports `unobserved` state.

## Failure policy and observations

`ClassifyFailure(Failure, now)` accepts safe facts, not raw errors. Executors must
classify errors at the boundary where they know their origin. In particular,
do not label every returned error as transport, protocol, or auth, and check
cancellation and recording failures first.

| Explicit failure | Continue before commitment | Shared cooldown |
| --- | --- | --- |
| HTTP 429 | yes | valid `Retry-After` decimal seconds or HTTP date; otherwise 60 s |
| HTTP 408 or 500–599 | yes | 30 s |
| HTTP 401/403 or provider authentication failure | yes | 30 s |
| Known transport failure | yes | 30 s |
| Adapter-confirmed malformed/unexpected response format or premature upstream stream termination (`FailureProtocol`) | yes | 30 s |
| Initial stream error with a known retryable upstream HTTP status | same HTTP policy | same HTTP policy |
| Disconnected | next candidate | none; excluded by provider config until reconnect |
| Other HTTP status, unclassified initial-stream/model error, request/capability error | no | none |
| Cancellation or history failure | no | none |

An unclassified model error event such as `response.failed`, without a known
retryable status, remains terminal (`FailureInitialStream`). In contrast, an
adapter-identified malformed/unexpected upstream response or an initial stream
break (for example, premature EOF before a terminal event) uses the explicit
`FailureProtocol` kind, permitting fallback before downstream commitment with a
30-second cooldown. A known transport read failure can instead use
`FailureTransport`. Do not relabel request/capability errors, unknown model error
codes, or client cancellation as protocol failures. Missing `Content-Type` alone
is not a protocol failure: the adapter supports body sniffing for OpenAI.
A downstream write error is not an upstream protocol/transport failure.
No policy adds a total SSE
timeout. `ResponseCommitted`, `ClientCanceled`, or `RecordingFailed` prevent
continuation even for otherwise retryable upstream facts. A real classified
upstream failure can still be observed after commitment; a purely local failure
must use its local failure kind and must not poison target health.

For each actual candidate, call `service.Runtime().BeginAttempt(target)` just
before authentication/provider work, after durable history admission and
`Request.StartAttempt`. Finish
its one-use token with `ObserveFailure(token, failure, now)` or
`ObserveSuccess(token)`. The former returns the classification policy. Tokens
are runtime-specific and are not retained in a per-request runtime registry.
Record attempt history independently; an observation is not a durable record.

Runtime state is keyed by `(ProviderID, UpstreamModel)`, shared across aliases
and unchanged by reordering. Concurrent failures never shorten an existing
cooldown: the effective deadline and its reason/source remain those of the
longest delay. The latest observed failure and latest observed 429 are retained
separately, including when the latest failure has a shorter delay. A success
clears a cooldown only if its token **began after every observed failure**.
Earlier-started successes cannot clear newer failures. Failures observed after
a success conservatively establish/extend a cooldown, even when their attempts
started earlier. Completion order governs failure observations.

Expiry means **eligible for another request**, not healthy. Success does not
erase failure/429 diagnostics. Valid zero/past `Retry-After` values give immediate
eligibility but still retain the 429 observation. Raw header/error text, tokens,
and secrets are never retained in runtime diagnostics.

All runtime state resets on restart. The shared startup gateway is passed to
both harness and management handlers. Its existing 30-second maintenance worker
calls `PruneRuntime` with a fresh full routing snapshot, removing deleted or
unreferenced destination pairs. Memory scales with configured pairs, not request
count, plus configuration churn since the last successful prune; no observation
log grows per request. A failed config snapshot does not prune valid state and
is retried on the next tick. Late tokens cannot resurrect pruned entries. No
background provider checks, probes, retries, or credential I/O are performed.

## Management API

`GET /api/routing/status` requires the same management session and completed
password change as other management endpoints. Harness bearer credentials do not
authorize it. Other methods return 405. Responses are `Cache-Control: no-store`.

The response has `observedAt` (server snapshot time) and a `models` list. Each model contains `id`,
`name`, `version` (saved configuration revision), `strategy`, `executionEnabled` (single and fallback), priority-ordered
`targets`, and an optional `predictedTarget`. Targets expose:

- `providerId`, `upstreamModel`, and `status`: `disconnected`, `cooling`, or
  `eligible` (not a claim of health);
- optional effective `eligibleAt`, `reason`, and `delaySource`;
- optional `lastFailure` and `last429`, containing safe reason, HTTP status,
  observation time, that observation's deadline, and parsed delay source.

Disconnection takes precedence over cooldown status. A disconnected target may
still show its previous deadline/diagnostics, but that deadline does not imply
that it will reconnect or become usable automatically.

Prediction follows the configured strategy: fallback's first eligible candidate;
single's connected target **even if cooling**. An unavailable model exposes
`unavailableReason` (`all_cooling`, `all_disconnected`, or
`unsupported_strategy`) and, for all-cooling, minimum `retryAfterSeconds`.
A prediction is only a selector preview, not an execution reservation.
Configuration is read in one transaction, then runtime state under one brief
lock. No transaction or runtime lock spans provider I/O. Predictions reserve
nothing and may change before selection; provider connection status is derived
from config, never duplicated as mutable runtime truth.

## WebUI observations

Models shows per-target observations in priority order and an explicitly labeled
predicted first target, never an active/running target claim. Provider cards show
configured upstream model pairs, deduplicated across aliases; a 429 observation
is not treated as an account-wide rate limit. Connection/authentication state,
generic cooldowns and 429 diagnostics remain distinct. Both pages show the last
429 time, its retry time and source (`Retry-After` seconds/date or default 60 s),
alongside any effective cooldown's reason/source. Expiry says **Retry eligible**,
not healthy. Single's observations are diagnostic: it never skips its connected
target due to cooldown, and never falls back. Disconnected targets are not predicted.

Only visible Models/Providers pages poll, approximately every eight seconds after
the preceding request finishes. A ten-second fetch deadline bounds stalled polls.
Leaving the page, hiding the document, or logging out cancels polling and in-flight
requests; generation checks reject late results. One-second local countdowns use
`observedAt` plus monotonic elapsed client time, clamp at zero, and are approximate
(network transit is not measured). Countdown expiry can change the local prediction
without a reload; concurrent runtime/connection changes still require the next poll.
Loading, unavailable and stale observations are explicit, not health claims.

Updates edit status text, not resource cards or editor controls. A model's saved
revision and exact provider/upstream pair must match before status is attached.
External configuration changes prompt a card Refresh; the editor draft, its original
expected version, target order and focus are never replaced by runtime polling.
