# OpenAI Responses provider contract

T09 implements the SIWC request boundary in `internal/provider`. The adapter has no
credential-store dependency: the gateway executor passes the current managed access
token to `Send`. `Adapter` is a Go interface so another provider can implement the
same request preparation and streaming response contract without a plugin system.

## Request preparation

`OpenAIResponsesAdapter.Prepare(upstreamModel, incoming)` accepts one JSON object and
returns `PreparedRequest`, which contains the effective upstream JSON and the
client's streaming preference. It returns `*CapabilityError` with a stable `Param`,
`Code`, and safe `Message` when the request is not supported. The gateway should
map that error to HTTP 400 in an OpenAI-style `error` object.

The adapter keeps unknown JSON fields and nested values, flat `function` and `custom`
tools, `prompt_cache_key`, `prompt_cache_options`, and `service_tier`. It changes
only `model`, `stream`, and `store`: the configured upstream model replaces the
requested model, `stream` is always `true`, and `store` is always `false`. The
incoming `stream` value is optional and defaults to false; when present it must be a
JSON boolean. A supplied `store` value must also be a JSON boolean, including when
it is `false`. `store:true` is accepted and normalized to false. The incoming
stream value is retained in `PreparedRequest.ClientStream` for downstream delivery.

`input` must be an array containing the complete conversation. Input strings are
rejected instead of being rewritten. Message items with `role:"system"` are
rejected; `instructions` and developer messages are allowed. The adapter rejects
these fields whenever they are present: `background`, `conversation`,
`max_output_tokens`, `max_tool_calls`, `metadata`, `moderation`, `multi_agent`,
`prompt`, `prompt_cache_retention`, `previous_response_id`, `safety_identifier`,
`temperature`, `top_logprobs`, `top_p`, `truncation`, `user`, and
`programmatic_tool_calling`.

Known unsupported tool types are rejected in `tools`, `additional_tools`, and
nested namespace definitions: `image_generation`, `file_search`, `code_interpreter`,
`computer`, `computer_use`, `computer_use_preview`, MCP/hosted-MCP, connector, and
`tool_search` tools. Audio and video input item types are rejected. File and image
inputs, including references to existing files, are not rejected here; this adapter
does not call the Files API. Other tool types and unknown request fields are left to
the upstream API to validate.

## HTTP and response lifecycle

Production requests use the fixed `https://api.openai.com/v1/responses` endpoint.
The adapter does not accept a provider URL or forward harness headers. It constructs
only `Authorization: Bearer <managed token>`, `Content-Type: application/json`,
`Accept: text/event-stream`, and the gateway-owned `X-Client-Request-Id`. Redirects
are returned as responses and are not followed. The shared transport disables
proxies and compression, reuses connections, bounds dialing/TLS/response headers,
and has no whole-response timeout so an active SSE response can run until the
request context is canceled.

`Send` returns an `UpstreamResponse` for every received HTTP status. Non-2xx results
preserve the actual status and raw body up to 64 MiB, including nonstandard error shapes.
A 2xx response whose media type is not `text/event-stream` also returns the bounded
body and `ErrNoEventStream`; the executor can return an explicit gateway error while
retaining that upstream response. `ErrorBodyTruncated` and `ErrorBodyReadFailed`
disclose a body limit or read failure.
Only a validated content type, a safe upstream request ID, and a valid `Retry-After`
value are exposed as response metadata. Transport errors are reduced to safe
sentinels; the URL, token, request, and raw transport error are not included.

For a 2xx response, call `NextEvent` repeatedly and call `Close` when finished. The
reader holds at most one SSE frame and enforces a 64 MiB limit per frame, not per
stream. It recognizes LF and CRLF line endings, and preserves each frame byte-for-byte,
including comments, unknown events, and original line framing. CR-only line endings
are not recognized as delimiters. Parsed event data joins multiple `data:` lines with a
newline. Invalid UTF-8 is an error; it is never replaced. The final unterminated
frame is returned with `Complete:false` for history/debugging, then the reader
returns EOF. A partial frame can never establish completion.

Only a complete `response.completed` event with a response object reports
`Outcome:"completed"`. `response.failed` and an explicit `error` event report
`failed`; `response.incomplete` reports `incomplete`. Other and future events are
preserved. EOF without one of those terminal events remains interrupted/incomplete
for the caller to decide; the adapter never manufactures a completed response.
Terminal events retain the full raw event JSON, the nested raw `response` object
(including provider error or incomplete details), and usage when reported. Cached
input and reasoning output counts are exposed as subsets, never added to totals.
Missing counts remain nil; a reported zero remains a pointer to zero.

## Timing data for T10

Create `Trace` with the executor's monotonic `time.Now()` admission origin and pass
it to `Send`. `GetConn` and `GotConn` capture connection acquisition and whether the
connection was reused; `WroteRequest` captures successful request transmission. The
SSE reader records the first complete data event, first nonempty text/refusal/
function-argument/custom-tool-input delta, and a provider terminal event. Comments
and heartbeat frames do not count as events. Reasoning deltas and `response.created`
do not count as first output. `NewTraceWithElapsed` accepts the recorder handle's
`ElapsedNS` method so all offsets can share the recorder's request clock.
`Trace.Snapshot` returns offsets from that clock; the executor maps them into history
timing fields and adds authentication preparation and downstream delivery timings.

## T10 gateway lifecycle

`POST /v1/responses` authenticates the local bearer key, admits at most eight inferences,
then reads a request body capped at 64 MiB (with a 30-second body-read deadline). It
resolves one authorized immutable Model target and durably begins history before adapter
capability checks or SIWC access-token preparation. It durably records the effective
upstream request before `Send`. There are no retries or fallback attempts.

The gateway synchronously reads, records, and forwards each complete SSE frame so a slow
downstream applies backpressure to upstream reading. Raw upstream frame bytes are
preserved, including the provider response's model name. Every downstream frame write and
flush has a 30-second deadline; an active SSE stream has no overall deadline. The local
`X-Request-ID` matches the history ID sent upstream as `X-Client-Request-Id`. A validated
provider request ID is recorded separately and returned as `X-Upstream-Request-ID`.

A terminal event is accepted only when its event type and nested response status agree
(for example, `response.completed` with `response.status: completed`). Complete raw events are
forwarded unchanged. A final unterminated frame is retained in history and withheld from
the client so it cannot corrupt SSE framing. Clean EOF without a terminal event sends a
safe SSE error event and ends as `incomplete`; provider stream read errors send the same
kind of safe SSE error and end as `failed`. A downstream write failure ends as `cancelled`. Terminal usage is kept
even if its event cannot be delivered.

HTTP error bodies and successful non-SSE response bodies are retained as exact bounded
bytes in schema 2 `upstream_response` records. Their bodies are not echoed in gateway
HTTP errors. Upstream 429 remains 429 with a validated `Retry-After` when available. Provider
401/403 becomes 502 with a Provider authorization error, without a local-key challenge.
Upstream 400/404/422 retains its status with a safe request-rejection message; 503/504
becomes 503 and other error statuses become 502. Raw upstream error bodies are retained
in history and excluded from local error messages.

For `stream:false` or an omitted `stream`, the gateway consumes and records frames one
at a time and returns the nested final Response JSON. Completed and incomplete responses
return 200 with their original status and usage. Failed/error terminals and malformed
terminal events return a safe 502. EOF without a terminal returns 502 and records an
incomplete outcome. The gateway does not accumulate an event array. Final JSON delivery
uses the same 30-second write deadline, and a failed delivery records cancellation.

If a history write fails after admission, the admitted inference continues and later
history writes may be lost; the recorder's counters report that loss. A recorder already
degraded at `Begin` rejects inference with 503 before SIWC token refresh or provider I/O.

## Verification status

The adapter has been built and source-inspected without a live OpenAI account. The
HTTP contract, real SIWC inference availability, and live error/header behavior
remain unverified against an authenticated account.
