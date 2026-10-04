# Gyemoim: Local LLM Gateway

Status: Implementation in progress; product decisions are confirmed.

## Purpose

Gyemoim is a local LLM gateway that centralizes provider authentication and records requests from multiple AI harnesses. Users connect provider accounts through a WebUI, then configure their harnesses to send inference requests through the gateway.

The service has four primary goals:

1. Authenticate with providers once instead of authenticating each harness separately.
2. Attribute requests and token usage to individual harnesses, and investigate context growth, repeated inputs, and prompt cache misses.
3. Provide evidence for debugging provider latency, gateway delays, and harness behavior.
4. Preserve request history as data for analyzing the user's usage patterns.

## Agreed First Version

- Implement the service in Go and support Linux and macOS.
- Ship a single executable with an embedded WebUI.
- Start with no configuration file or database setup required.
- Listen on `127.0.0.1:9092` by default; support `--port` to change the port.
- Implement OpenAI OAuth first, using Sign in with ChatGPT for eligible Responses API requests.
- Expose the OpenAI Responses API format to harnesses.
- Make pi agent the first supported harness. Implement the provider-supported Responses API contract rather than a pi-specific subset; Codex support is not a first-version requirement.
- Manage three user-facing concepts in the WebUI: Provider, ServiceAccount, and Model.
- Issue local API keys to ServiceAccounts used by harnesses. Provider authentication remains centralized.
- Let users assign Model names and connect each Model to one Provider and one upstream model. Implement only single-target selection in the first version, behind a Go strategy interface.
- Permit only explicitly assigned Models for each ServiceAccount. Newly created Models require an explicit grant; there is no all-Models permission.
- Provide basic history investigation and per-request provider-reported cache usage. Defer request comparison and cache-miss explanation features.
- Aggregate statistics by ServiceAccount, requested Model, and the actual Provider and upstream model used for each attempt.
- Record prompts, responses, tool definitions, and tool results by default, excluding authentication credentials.
- Preserve all request content without per-harness recording exclusions or automatic body masking.
- Block new inference requests when request recording is unavailable; do not offer an unlogged bypass.
- Allow immediate access to the local WebUI without an administrator login or initial approval flow.
- Apply Provider disconnection/deletion and ServiceAccount key revocation to new requests without explicitly interrupting requests already in progress.
- Store configuration and credentials in SQLite; store request history in NDJSON files.
- Rotate logs and compress closed files by spawning the external `zstd` executable.
- Retain request history indefinitely. Provide storage visibility and deletion of all request records in a selected date range; do not offer ServiceAccount- or Model-specific deletion in the first version.

## Architecture

### Core Concepts

| Concept | Definition | WebUI configuration |
| --- | --- | --- |
| Provider | An authenticated upstream connection instance. Two OpenAI accounts are two Providers with the same provider type. | Name, provider type, upstream address, and OAuth or API key credentials. |
| ServiceAccount | A gateway-issued account used by a harness. It can access multiple Models and therefore multiple Providers. | Name, local keys, and permitted Models. |
| Model | A user-named routing configuration, independent of an upstream model ID. | Name and one Provider/upstream-model target in the first version; selection strategy settings can be added later. |

For example, Providers `openai-personal` and `openai-work` can both have type `openai`. A ServiceAccount `pi-work` can access Models `coding` and `quick`. A request with `model: "coding"` selects that configured Model, whose routing strategy chooses a Provider and upstream model ID.

Harness is a client description rather than a separate configuration entity. Provider type describes the upstream API implementation rather than a separate authenticated account. Each Provider owns its authentication state; there is no separate user-facing ProviderConnection concept.

```mermaid
flowchart LR
    H[AI Harnesses] -->|Responses API and local key| G[Gateway]
    G --> O[OpenAI]
    U[Embedded WebUI] --> C[Provider, ServiceAccount, and Model Management]
    C --> S[(SQLite)]
    S --> G
    G --> R[Request Recorder]
    R --> N[NDJSON Files]
    N --> Z[External zstd Process]
    N --> A[Query and Aggregation]
    Z --> A
    A --> U
```

The request path handles ServiceAccount authentication, Model authorization, target selection, Provider authentication, request validation, streaming, cancellation, timing, and recording. Background work handles compression and history analysis.

Provider-specific authentication, request constraints, and model catalog conversion belong in the provider layer so that additional providers can be introduced without changing harness identity or storage behavior.

## WebUI and Feature List

| Page | Feature | Behavior |
| --- | --- | --- |
| Overview | Usage dashboard | Attribute requests to ServiceAccounts, requested Models, and actual Providers/upstream models; show per-attempt usage and request totals. |
| Overview | Performance and error summary | Show first-response latency, total duration, failure and cancellation rates, and slow requests. |
| Providers | OpenAI OAuth connections | Sign in, add accounts, inspect connection status, reauthenticate, and disconnect. |
| Providers | Token lifecycle | Refresh automatically, serialize refreshes per session, and atomically persist replacement credentials. |
| Providers | Upstream catalog and permissions | Show connection-specific upstream models and ChatGPT plan authorization; link to provider usage management. |
| ServiceAccounts | Account management | Assign a name, issue or revoke local keys, and explicitly permit access to individual Models. New Models are not automatically permitted. |
| ServiceAccounts | Connection instructions | Provide Base URL, local key, and configured Model name examples for pi agent over HTTP Responses. |
| Models | Model management | Define user-facing names and one Provider/upstream-model target per Model. |
| Requests | Request browser | Filter by ServiceAccount, requested Model, actual Provider/upstream model, time, and status; display requests in progress. |
| Requests | Request details | Inspect the incoming request, effective upstream request, response events, tool calls, errors, and usage. |
| Requests | Timing timeline | Show authentication preparation, connection, transmission, first event, first output, stream termination, and downstream delivery. |
| Requests | Cache usage | Show input tokens, cached input tokens, cache usage ratio, and output tokens per request; preserve unavailable cache information as unknown. |
| Storage | Retention and compression | Show raw and compressed sizes, pending or failed compression, and deletion of all request records in a selected date range. |
| Storage | Service status | Show data paths, SQLite and log write status, and whether `zstd` is available. |

## Harness Interface and Routing

The first version exposes:

- `POST /v1/responses`
- `GET /v1/models`

The gateway authenticates a ServiceAccount using its local bearer key, resolves the request's `model` to a configured Model, and checks that the ServiceAccount is permitted to use it. The Model's selection strategy chooses a Provider and upstream model ID. The gateway replaces the client Model name and local credential with the selected upstream model ID and Provider credential. Provider access and refresh tokens are never supplied to harnesses.

ServiceAccounts can use only explicitly granted Models. Creating a Model does not grant access to existing accounts, and there is no wildcard or all-Models permission in the first version.

ServiceAccount keys are stored as hashes in SQLite. Keys can be issued and revoked from the WebUI.

pi agent is the first integration target. Preserve Responses API behavior across supported clients rather than introducing pi-specific request semantics. OpenAI OAuth capability restrictions still apply and must produce clear errors. The pi version, configuration, and integration acceptance cases will be established during implementation.

When admitting a request, retain its ServiceAccount identity and a snapshot of the configured Model and routing settings. Record the actual Provider and upstream model for each attempt. For Responses requests, the gateway serializes Provider credential removal by disconnect or deletion with a local connected-status check, durable history admission, and token resolution. A disconnected provider is rejected before admission; token refresh remains after durable admission, and a failed admission performs no refresh or provider I/O. The lease ends after token resolution and before upstream transmission. Subsequent configuration changes, key revocation, or Provider credential removal affect new requests. The gateway does not explicitly cancel admitted requests in response to these administrative actions.

`GET /v1/models` returns configured Model names authorized for the calling ServiceAccount in the standard OpenAI model-list format. Provider upstream catalogs are available in the WebUI to configure Model targets. Harnesses use the user-defined Model name rather than a Provider-prefixed upstream model ID.

Every inference request receives a gateway request ID, returned through `X-Request-ID`. Provider request IDs are recorded separately.

Statistics are grouped by ServiceAccount, requested Model, and actual Provider/upstream model. Session and project attribution are deferred to later extensions. A ServiceAccount identity is not proof that requests belong to one conversation.

### Extensible Model Selection

The first version implements only a single-target strategy: each Model selects one configured Provider and upstream model, with no fallback or automatic retry. The WebUI does not expose a strategy picker or multiple targets until another strategy is implemented.

Model selection is abstracted through Go interfaces. Fallback is one possible strategy; the configuration model and execution path must also accommodate other strategies, such as weighted selection or selection based on observed latency. Those strategies are extension examples rather than a requirement to implement them all in the first version.

A strategy creates request-local selection state, chooses an initial target, and can receive an attempt outcome when choosing a subsequent target. An illustrative interface boundary is:

```go
type RoutingStrategy interface {
    NewSelection(ctx context.Context, request RoutingRequest) (Selection, error)
}

type Selection interface {
    Next(ctx context.Context, previous *AttemptOutcome) (Target, error)
}
```

`RoutingRequest` contains the request and a snapshot of the Model configuration. `Target` identifies a Provider and its upstream model ID. `AttemptOutcome` describes a previous attempt's result. The initial selection has no previous outcome. Exhaustion is an explicit stop result. Shared strategy implementations must keep per-request attempt state isolated.

Model configuration identifies the strategy and stores its strategy-specific settings. Future WebUI extensions can expose the settings supported by additional strategy implementations. The strategy chooses targets; the gateway executor owns authentication, transport, validation, recording, cancellation, and response delivery.

Selection does not imply compatibility between targets. Every selected target must support the request's capabilities. Unsupported request capabilities produce clear errors, and strategy execution must preserve this policy rather than silently rewriting the request.

Any strategy that performs another upstream attempt must explicitly define eligible failure conditions and record every attempt. The executor prevents switching targets after a response has been committed to the client and respects client cancellation. Default execution remains a single upstream attempt; automatic retries or fallback occur only when explicitly configured through a strategy.

## OpenAI OAuth and Request Handling

Use the official Sign in with ChatGPT flow described in [Registration and sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in).

- Use the same listener for the WebUI and the OAuth callback: `http://127.0.0.1:<port>/auth/callback`.
- Start initial registration with `client_id=dynamic_agent_client` and retain the actual issued client ID for later authorization and token exchange.
- Generate and persist a stable host ID.
- Validate state, PKCE, nonce, the ID token, and granted scopes.
- Keep account registrations and credential sets separate, including accounts with identical email addresses.
- Serialize refreshes for each session and atomically save the access token and replacement refresh token together.
- Support reauthentication and connection removal through the WebUI.

Credential renewal and disconnection behavior follow [Accounts and sessions](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions).

Inference uses the public OpenAI Responses endpoint. Upstream HTTP requests use `stream:true` and `store:false`. For a nonstreaming client request, the gateway collects the completed upstream SSE response and returns JSON. For a streaming request, it forwards SSE to the client.

The provider layer validates capabilities and records request normalization. Apart from the explicitly documented streaming and storage adaptations, unsupported fields, tools, or conversation-state requests return clear errors rather than being silently removed or changed. The first version does not provide a selectable compatibility mode. HTTP requests must include the necessary conversation history rather than depending on persistent upstream response state.

Propagate client cancellation upstream. Distinguish successful completion, failure, incomplete output, and interrupted streams. The executor does not automatically retry inference requests; any additional attempt is governed by an explicitly configured Model selection strategy.

These constraints are based on [Models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference) and [Preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations). Recheck the official requirements when implementing the provider adapter because this integration is a preview.

## Observability and Analysis

### Usage Accounting

Use token counts reported by the provider. Display cached input tokens and reasoning output tokens as components of input and output usage rather than adding them again to totals. If a request ends without reported usage, display it as unknown rather than zero.

Record the requested Model identity/name, routing configuration version, selected strategy, and each attempt's Provider, upstream model ID, outcome, timing, and reported usage. Group attempts under one gateway request ID. Count client requests separately from upstream attempts and include usage from every attempt that reports it.

Preserve the incoming request and the effective upstream request so that gateway normalization is visible during debugging.

### Latency Analysis

Record the stages observable at the gateway, including authentication preparation, connection establishment, request transmission, first upstream event, first output, stream completion, and downstream delivery.

These measurements help locate delays but do not independently prove a provider or harness bug. Time spent executing local tools or preparing the next harness request requires harness-side records or additional instrumentation. Attribution uses ServiceAccount, Model, and each attempt's actual Provider/upstream model; deeper session correlation is a later extension.

### Cache Usage

The first version displays each request's provider-reported input tokens, cached input tokens, output tokens, and cache usage ratio (`cached input tokens / input tokens`). Cached tokens are already included in input tokens. Display missing cache usage as unknown, not zero. Display the ratio as unavailable when either count is unknown or input tokens are zero.

Request comparison, input-prefix differences, and explanations for cache misses are deferred. Preserve incoming and effective request content and structured usage records so those features can be added later. Future explanations must distinguish observed changes from uncertain provider-internal routing, hidden context, or cache expiration.

### Pattern Analysis Data

The first version provides history browsing, request details, basic usage aggregation, timing, and per-request cache usage. Request comparison, exports, and automated analysis are later extensions. This creates a dataset for later analysis of repeated requests, growing context, large tool outputs, and usage patterns.

## Storage and Local Access

### SQLite

Store Providers and their credentials, issued OAuth client IDs, the host ID, ServiceAccounts and local key hashes, Models and strategy settings, ServiceAccount Model permissions, and storage settings in SQLite.

Create the application data directory automatically. Apply owner-only directory and file permissions. The loopback WebUI opens directly without an administrator login, password, or initial approval step. Protect WebUI mutations with Origin and CSRF validation; these protections must not introduce an administrator sign-in flow.

Request bodies and usage history are stored in log files rather than SQLite. In-memory aggregates are disposable and can be rebuilt from the logs.

### NDJSON Request History

Record request-start, upstream-transmission, response-event, and request-end records correlated by gateway request ID. Include schema versions and timestamps.

Record all prompts, responses, tool definitions, and tool results without per-harness exclusions or automatic content masking. Exclude gateway-managed authentication fields, including authorization headers, local keys, OAuth credentials, and authentication URLs containing token hints. Arbitrary content supplied inside prompts or tool results is preserved, so credential exclusion is not a guarantee that bodies contain no user-supplied secrets.

Keep partial request history when a stream fails or is interrupted. A completed HTTP connection alone does not establish successful inference.

If request recording becomes unavailable, including because the disk is full, reject new inference requests with an explicit `503` recording-unavailable error before calling the provider. There is no mode that bypasses recording. Keep the WebUI available to inspect and resolve the storage problem. Requests already in progress are not explicitly interrupted by this admission policy; any recording loss affecting them must be reported.

### Rotation and Compression

- Rotate a file at 64 MiB or after one hour.
- Compress only closed files.
- Check for compression work once per minute.
- Invoke the external `zstd` executable; do not embed a zstd library.
- Write compressed output to an owner-only temporary file in the history directory, sync it, rename it to `<segment>.zst`, sync the directory, then remove the source NDJSON file and sync the directory again.
- Keep the source file when compression fails.
- If `zstd` is unavailable, continue recording NDJSON and display compression as unavailable. Compressed history remains visible but queries return an explicit unavailable error until the executable is available and history is revalidated.
- Read compressed history through an external `zstd -dc` process. Cancel and reap the child if a query stops early, and check its exit status after reaching EOF.
- Validate compressed records during startup. If raw and compressed files form a crash-window pair, compare the decompressed SHA-256 and byte length before deleting or deduplicating either file.
- Expose active, raw, and compressed byte counts; pending and failed work; executable availability; and the latest safe operational error through the Storage page and `GET /api/storage`.
- Preserve records indefinitely; support storage inspection and manual deletion by date range.
- Delete all request records whose `started_at` falls in the selected inclusive UTC calendar dates across ServiceAccounts and Models. The effective interval is half-open and its end is capped at the submission time.
- Reject deletion with HTTP 409 and no file changes while a matching request is in progress. Serialize deletion with queries and compression, and recover from a durable roll-forward journal before startup validation. Account- or Model-filtered deletion and configurable retention policies are deferred.

## Implementation Sequence

Implementation is split into T01 through T17 and tracked in [implementation-plan.md](implementation-plan.md). The current implementation decisions below refine this initial design.

### Confirmed Implementation Decisions

- T02 uses SQLite through `modernc.org/sqlite` v1.59.0.
- Keep request history in NDJSON files outside SQLite. Rotate at 64 MiB or one hour at record boundaries; check idle active files once per minute. Closed segments are immutable raw NDJSON until a later maintenance task handles them.
- Invoke the external `zstd` executable; do not bundle a zstd library.
- Limit request input and upstream SSE data to 64 MiB, and limit concurrent inference to eight requests.
- The initial pi integration targets pi agent 1.0.0 and exposes reasoning models through `models.json`.
- Optional advanced Model metadata consists of `contextWindow`, `maxTokens`, `input`, `reasoning`, and `supportedReasoningEfforts`. Populate fields only from exact metadata, do not guess values, and generate `models.json` only when the metadata is complete.
- The gateway implements the provider-supported Responses API contract for reasoning and non-reasoning models. pi's initial metadata limitation does not narrow the gateway contract.

## Acceptance Criteria

- On a fresh Linux or macOS installation, the executable starts and serves the WebUI without configuration beyond an optional port.
- Two ServiceAccounts can share one Provider while their requests and usage remain independently attributable.
- One ServiceAccount can access multiple Providers through its permitted Models.
- Model listing exposes only permitted configured Model names, and requests are routed using those names.
- pi agent can connect using the documented Responses configuration, and supported Responses behavior is preserved without pi-specific API semantics.
- Unsupported request capabilities produce clear errors instead of silent compatibility transformations.
- Statistics preserve the ServiceAccount, requested Model, and actual Provider/upstream model for every attempt.
- Each Model has exactly one target in the first version; forwarding makes no automatic fallback or retry attempts. The strategy interface remains available for future selection implementations.
- Newly created Models remain inaccessible to a ServiceAccount until explicitly granted.
- Restart, concurrent refresh, missing authorization scopes, and reauthentication do not mix credential sets or account registrations.
- Tool calls, normal completion, stream errors, incomplete responses, cancellation, and disconnection are reflected consistently in client behavior and logs.
- Request details show provider-reported input, cached input, and output tokens and the cache usage ratio; missing information remains unknown.
- History can be filtered and inspected, with basic usage totals per ServiceAccount.
- Date-range deletion removes records across all ServiceAccounts and Models in that range.
- Restart, rotation, and compression failure leave existing history readable.
- Missing `zstd` does not prevent startup or NDJSON recording.
- When recording is unavailable, new inference requests are rejected before provider invocation and the management UI remains accessible.
- The local WebUI is immediately accessible without an administrator login or initial approval.
- ServiceAccount key revocation and Provider credential removal by disconnection or deletion prevent new requests without gateway-initiated cancellation of admitted requests.
- Gateway-managed authentication fields do not appear in request logs or exports; arbitrary prompt and tool content remains intact.

## Later Extensions

Extend in this order:

1. OpenAI API key authentication.
2. Claude and z.ai API key connections.
3. Chat Completions and Claude Messages protocol adapters.
4. Additional Model selection strategies, including fallback, when needed.
5. Request comparison, cache-miss investigation, and record/usage exports.
6. Session/project attribution, harness-side trace correlation, and deeper pattern analysis.
7. Filtered deletion and configurable retention policies, when needed.

Keep extension preparation limited to provider adapters, the Model selection interface, and versioned log schemas. Do not build unused strategies, plugin frameworks, or a general-purpose permission engine in the first version.

Add OAuth for another provider only after confirming its officially supported integration flow and inference permissions.
