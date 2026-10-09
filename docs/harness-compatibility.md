# Harness compatibility: tolerant request handling and Chat Completions

This document specifies how Gyemoim accepts requests from harnesses beyond the
Responses-native pi agent. All other invariants are unchanged:
recording-before-inference, single-target routing, no retries or fallback,
`stream=true` / `store=false` upstream, bounded sizes and deadlines.

## Rationale

Harnesses other than pi were effectively locked out: strict field rejection
broke clients that always send `temperature` (Roo, Cline, LangChain, generic
OpenAI SDK apps), and most coding harnesses outside pi speak the Chat
Completions API only. Research findings that shaped this design:

- Sign in with ChatGPT (SIWC) access tokens work only on
  `POST /v1/responses`; Chat Completions cannot be forwarded natively. OpenAI's
  own Codex `responses-api-proxy` solves the same problem with a translation
  layer, which is the approach adopted here.
- The rejected-field list mirrors the SIWC backend's real 400s, so "dropping"
  means removing the field from the effective upstream request, not passing it
  through.
- Industry practice is default-tolerant with opt-in strictness (OpenRouter
  `require_parameters`, vLLM accept-and-warn, LiteLLM `drop_params`). The one
  universally documented catastrophic failure is silently dropping
  stateful references: the conversation appears to work but loses context with
  no diagnostics. Hence the explicit error for `previous_response_id` in
  section 2.

## 1. Unsupported request fields and tools — classified drop policy

Applies to every harness-facing inference path, including Chat Completions
after translation.

- **Dropped silently**: `temperature`, `top_p`, `top_logprobs`,
  `max_output_tokens`, `max_tool_calls`, `metadata`, `moderation`, `user`,
  `safety_identifier`, `background`, `prompt`, `prompt_cache_retention`,
  `multi_agent`, `programmatic_tool_calling`, `truncation`, and the
  `connectors` top-level field. These affect quality, attribution, or
  server-side bookkeeping only; the request still completes correctly without
  them.
- **Unsupported tool types**: entries of type `image_generation`,
  `file_search`, `code_interpreter`, `computer`, `computer_use`,
  `computer_use_preview`, `mcp`, `hosted_mcp`, `hostedmcp`, `connector`,
  `connectors`, and `tool_search` are removed from `tools`,
  `additional_tools`, and nested namespaces; remaining tools are forwarded.
  If every entry is removed the request is forwarded without tools (the model
  answers in text and the harness fails visibly, which is preferable to a
  400 the harness cannot recover from).
- **`role:"system"` message items** are rewritten to `developer` message
  items, order preserved (the SIWC backend rejects explicit system items; this
  is the standard lossless rewrite used by the Codex ecosystem).
- **Explicit errors kept**: `previous_response_id` (section 2), `conversation`
  (section 2), audio and video input items, `n > 1` on the Chat Completions
  path, and structurally invalid requests (non-array `input`, invalid JSON,
  unknown model, missing grants).
- **Unknown fields not on any list still pass through**; the upstream API
  remains the validator for future fields. Only known-unsupported fields are
  dropped, so a typo cannot silently disable a real parameter.

### Drop evidence

`request_start` already stores the raw incoming body and
`upstream_transmission` stores the effective upstream request, so every drop
is reconstructible from history. `request_end` additionally records a
`dropped_fields` list (field names and, for tools, the removed tool types) so
drops are queryable without diffing bodies. No response headers are emitted.

## 2. `previous_response_id` and `conversation` — explicit errors

The SIWC backend forbids server-side response storage (`store: false` is
forced upstream), so a `previous_response_id` reference can never resolve
against OpenAI's server: the referenced response was never stored. A silent
drop would make turn 1 succeed and later turns lose all context with no
diagnostics. A gateway-side response-state store is deliberately not provided:
pi and Codex-class harnesses never send this reference, so the storage and
replay complexity is not justified. Therefore:

- A request carrying `previous_response_id` produces an explicit 400
  (`previous_response_id` parameter, message directing the client to include
  the complete conversation in `input` instead) — the same outcome OpenAI
  itself returns for an unresolvable reference.
- The `conversation` field produces an explicit 400 for the same reason;
  proxying the `/v1/conversations` CRUD API is out of scope.
- `store: true` from clients stays normalized to `store: false` upstream
  (existing behavior); no gateway-side state substitutes for the server-side
  store.

## 3. Chat Completions endpoint (translated)

`POST /v1/chat/completions` is served by translating to the Responses
upstream. `/v1/models` is unchanged and shared by both formats. Upstream
remains the single Responses + SIWC path: single-target routing, explicit
grants, the admission semaphore, body/frame limits, deadlines, and
recording-before-inference all apply unchanged. The translation layer lives in
the provider layer (`internal/provider`).

History needs no schema change: `request_start.incoming_request` stores the
chat JSON as received, `upstream_transmission.effective_request` stores the
translated Responses JSON, and `response_event` records stay upstream-native
(the downstream chat chunks are synthesized, not recorded).

### Request mapping (chat → Responses)

| Chat Completions | Responses | Note |
| --- | --- | --- |
| `messages[]` string content | message items with `input_text` content parts | |
| `role:"system"` | `developer` message item | order preserved |
| `role:"developer"` | `developer` message item | pass-through |
| assistant `tool_calls[]` | one `function_call` item each (`call_id`, `name`, `arguments`) | |
| `role:"tool"` | `function_call_output` items | linked by `call_id` |
| `tools[]` `{type:"function", function:{…}}` | flat `{type:"function", name, description, parameters, strict:false}` | `strict` set explicitly: Responses defaults toward strict mode when omitted |
| `tool_choice` | flattened (`{type:"function", name}`) | |
| `reasoning_effort` | `reasoning.effort` | |
| `verbosity` | `text.verbosity` | |
| `response_format: json_schema` | `text.format` (`name` moves to top level) | |
| `response_format: json_object` | `text.format: {type:"json_object"}` | |
| `parallel_tool_calls`, `prompt_cache_key`, `service_tier` | pass-through | |
| `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `seed`, frequency/presence penalties, `logprobs`, `top_logprobs`, `user`, `metadata` | dropped | same classes as section 1; the SIWC backend rejects them |
| `stream_options.include_usage` | consumed (final usage chunk) | never forwarded |
| `n` | `n: 1` only | `n > 1` is an explicit 400 |
| image content parts | `input_image` items | supported |
| audio content parts | — | explicit 400 |

### Streaming translation (Responses SSE → chat SSE)

- `response.created` → first `chat.completion.chunk` with
  `delta: {role: "assistant"}`.
- `response.output_text.delta` → `delta: {content: …}` chunks.
- Tool calls are accumulated from `response.output_item.added` (which carries
  `call_id` and `name`, assigned a stable index) plus
  `response.function_call_arguments.delta`; later chunks omit id/name. This
  avoids the known failure of building tool calls only from
  `response.completed`, which drops streamed tool calls.
- `finish_reason`: `tool_calls` when any function-call items were emitted,
  otherwise `stop`; `response.incomplete` with
  `incomplete_details.reason: "max_output_tokens"` → `length`, other
  incomplete reasons → `stop`.
- `response.failed` / explicit `error` events end the stream with a
  chat-format error body. Clean EOF without a terminal event ends the stream
  after a safe error chunk, mirroring the Responses path.
- When the client sent `stream_options.include_usage`, one final usage-only
  chunk (empty `choices`) is emitted before `data: [DONE]`.
- Reasoning summary deltas have no Chat Completions representation and are not
  emitted (documented loss; usage still reports reasoning tokens).

### Non-streaming translation

The collected terminal Response object is translated to one
`chat.completion` JSON: `content` concatenated from `output_text` parts,
`message.tool_calls` from `function_call` items in order, `usage`
`prompt_tokens`/`completion_tokens`/`total_tokens` from the Responses counts
with `completion_tokens_details.reasoning_tokens` from
`output_tokens_details`, `finish_reason` per the table above, `id` =
`chatcmpl-` + the gateway request ID, `model` echoing the client-requested
Model alias, `created` = completion time as a Unix timestamp.

### Errors

Chat-path errors use the same OpenAI error envelope (`error.message/type/
code/param`) as the Responses path; authentication, limits, model-not-found,
and grant semantics are identical.

### Known losses (documented, accepted)

- Chat clients cannot replay reasoning items across turns — chat has no
  reasoning item representation, so reasoning context is per-request.
- Dropped `max_tokens`/`max_completion_tokens` means no output cap (the SIWC
  backend rejects `max_output_tokens`).
- `previous_response_id` is Responses-format only; Chat Completions clients
  resend history by design and need no state handling.
