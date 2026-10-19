# Routing selection and runtime observations

The harness currently executes **single** routes only. Management model writes
remain single-only. The gateway's `fallback` strategy and observation policy are
available as Go APIs, not activated inference behavior. Runtime snapshots may
therefore be unobserved until an executor reports observations; no probes run.

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

The response has `observedAt` and a `models` list. Each model contains `id`,
`name`, `strategy`, `executionEnabled` (currently only single), priority-ordered
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
A fallback prediction is only a selector preview, not enabled harness execution.
Configuration is read in one transaction, then runtime state under one brief
lock. No transaction or runtime lock spans provider I/O. Predictions reserve
nothing and may change before selection; provider connection status is derived
from config, never duplicated as mutable runtime truth.
