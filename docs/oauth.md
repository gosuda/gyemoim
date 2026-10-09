# Sign in with ChatGPT

Gyemoim follows OpenAI's [Sign in with ChatGPT registration and sign-in flow](https://developers.openai.com/siwc/token-sharing-open-source/sign-in) and [account and session guidance](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions). It starts sign-in only after a user presses **Connect OpenAI account** or **Reconnect**. Startup stays offline. The browser leaves the local UI for the authorization page and returns to the same listener at `http://127.0.0.1:<port>/auth/callback`. The active listener port is captured for each sign-in attempt. [OpenAI's registration guidance](https://developers.openai.com/siwc/token-sharing-open-source/sign-in) allows the loopback port to vary on later sign-ins while keeping the scheme, host, and callback path unchanged; the exact selected URI is reused within that authorization attempt. A live reauthorization after changing Gyemoim's port has not been verified.

## Authorization flow

For each start, Gyemoim loads the OpenID discovery document from `https://auth.openai.com/.well-known/openid-configuration` through a 30 second, no-redirect HTTPS client restricted to `auth.openai.com`. It checks the issuer and the authorization, token, JWKS, and revocation endpoints against the expected Sign in with ChatGPT URLs. The same validated endpoint is used for best-effort refresh-token revocation when a user disconnects.

The first sign-in uses `client_id=dynamic_agent_client`, `agent_name_hint=Gyemoim`, and the persistent local `ext_agent_host_id` URN. On its callback, Gyemoim requires the provider-issued `client_id` and saves it before the token exchange. An unsuccessful first exchange therefore keeps that issued registration identifier for a later attempt. Later sign-ins use the issued ID, omit `agent_name_hint`, and may include the last stored ID token as `id_token_hint` and the verified email as `login_hint`.

Every start creates a random state, nonce, and PKCE S256 verifier. The in-memory state expires after ten minutes; a provider can have only one active flow, and a new start replaces its earlier flow. A state is consumed once. The token exchange sends the authorization code, issued client ID, verifier, same redirect URI, and `resource=https://api.openai.com/v1`. It has no client secret.

Requested scopes are `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`. The token response's `scope` string is the only grant evidence. Gyemoim verifies the ID token signature and issuer through `coreos/go-oidc`, checks its issued-client audience, expiry and issue time, requires a subject, and checks the nonce. On a returning sign-in, the verified subject must match the saved registration before any credential replacement. Only an email marked verified in that token is retained; if a later validated token omits verified email, Gyemoim keeps the address already retained from an earlier validated token. Callback query values are not identity data.

The complete token set, verified subject/email, registration ID, scopes, expiry, earliest-refresh metadata, and provider status are committed together. `earliest_refresh_at` is kept as opaque JSON text; refresh carries it forward when the token endpoint omits the field. Tokens are excluded from Provider API responses and ordinary JSON serialization.

## Connection status

The requested scope list is not treated as proof that the account granted those scopes. A nonempty authoritative `token.scope` must include `chatgpt.tokens.use.direct` before the Provider is marked `connected`. When `scope` is absent, a validated identity and token set can still be retained, but the Provider is marked `plan_usage_disabled` because the grant cannot be inferred. If a reported scope set lacks `offline_access`, or the response has no refresh token, the Provider is marked `require_reauthentication`. A connected status means the direct-use scope was granted. Internal callers obtain a bearer token only for a connected OpenAI provider with both direct-use and offline scopes. The token manager refreshes on demand when the access token has 60 seconds or less remaining; it does not refresh at startup or on a timer.

The provider card's Connect/Reconnect entry point is the enrollment-script flow (see [Remote enrollment](#remote-enrollment) below). Callback results redirect to the local root with a fixed result code. The UI maps only those fixed codes to messages and removes the query string. The callback requires no session — it is a top-level provider redirect — and its protection is the single-use, server-side state that is consumed at the callback; the listener accepts any Host. The loopback `oauth/start` flow (`POST /api/providers/{id}/oauth/start`) remains available as an API for local and API use, and its redirect also lands on `/?oauth_result=...`. Callback responses set no-store. OAuth codes, errors, token values, and upstream error bodies are not returned in callback pages or logs.


## Remote enrollment

OpenAI accepts only loopback redirect URIs, so a headless server cannot
complete sign-in directly; the supported procedure is credential transfer
completing OAuth on a machine with a browser. Gyemoim implements this as a
server-distributed enrollment script. The loopback rules above apply
unchanged — only the loopback port varies, and within one enrollment attempt
the start and callback URIs are identical by construction.

The admin presses **Connect** on the provider card in the management UI. The
server issues a single-use enrollment code (128 bits, ~10 minute TTL, bound
to that provider, consumed at the first claim; a later issue invalidates an
earlier unclaimed code) and offers a download of `gyemoim-connect.py`, a
stdlib-only Python script embedded in the server binary and served
session-gated at `GET /api/connect/script`. On the browser machine the admin
runs `python3 gyemoim-connect.py <server-url> <code>`:

1. The script claims the code (`POST /connect/claim`) and starts a loopback
   HTTP server on an ephemeral port.
2. It asks the server to start the connect flow with
   `redirect_uri=http://127.0.0.1:<port>/auth/callback` and opens the system
   browser at the returned authorization URL.
3. OpenAI redirects to the loopback port; the script forwards `code` and
   `state` to the server (`POST /connect/complete`), which completes the
   existing flow — dynamic registration with `client_id=dynamic_agent_client`,
   PKCE, the token exchange, and ID-token verification — and stores the
   credentials. The script only prints the outcome.

The script is a thin bridge with no OAuth logic. The claim/complete endpoints
sit outside the management session gate by design: the script is not a
browser, and the single-use enrollment code plus the single-use OAuth state
recorded at claim time are the capability; every failure answers the same
generic error. Registration uses the server's own persistent
`ext_agent_host_id` from the start, so the stored session is bound to the
server, not to the browser machine, and the server owns all later refreshes.
Re-authentication after a refresh failure (`require_reauthentication`) uses
the same enrollment flow from the provider card.

Per OpenAI's self-hosted VMs guidance, usage attribution and remote
revocation for transferred sessions are not yet available upstream; a later
re-authorization repeats the local enrollment flow.

## Refresh and disconnect

Before returning an access token to an internal caller, Gyemoim checks the provider status and saved direct-use/offline grants. If the access token expires within 60 seconds, it serializes a refresh for that provider and sends a form-encoded `refresh_token` grant with the saved issued `client_id`, saved rotating refresh token, and `resource=https://api.openai.com/v1`. It omits `scope` so the existing grant is retained, following [OpenAI account and session guidance](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions). The new access token, required replacement refresh token, expiry, scopes, `earliest_refresh_at`, and validated identity are saved in one transaction. When the response omits `scope`, Gyemoim keeps the last validated granted scopes; when it omits `id_token`, it keeps the last validated ID token and identity. If a new ID token is returned, Gyemoim checks its signature, issuer, issued-client audience, expiry, and subject continuity. A refresh response has no browser authorization nonce to compare.

A successful refresh without a replacement refresh token, or a terminal token error such as `invalid_grant` or `refresh_token_reused`, clears the token set and marks the provider `require_reauthentication`, retaining the issued client registration. A temporary transport or server failure preserves credentials and returns a generic unavailable error. `invalid_client` is reported as an OAuth client configuration problem without clearing the saved token set. See [OpenAI refresh errors](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery).

Disconnect invalidates pending authorization state under the same per-provider lock, clears local tokens and marks the provider disconnected, then loads and validates OpenID discovery to obtain the `revocation_endpoint` and attempts to revoke the saved refresh token using the issued client ID and `token_type_hint=refresh_token`. It skips discovery when there is no saved refresh token. A response is considered confirmed only for an empty HTTP 200. Local credentials remain cleared even if that best-effort revocation fails; the UI then directs the user to ChatGPT Settings. The issued client registration remains so the next sign-in reuses it. Deleting a Provider also takes this lock and invalidates pending authorization only after the database deletion succeeds; deletion removes its credentials and registration without a remote revocation request.

## Provider model catalog

The Models page can load the selected connected account's current model list from `GET https://api.openai.com/v1/models`. Gyemoim follows the [SIWC model-catalog guide](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference): it keeps entries with `visibility == "list"` and returns only each entry's `slug` and `display_name` as a model ID and display name. It does not infer context limits, capabilities, or reasoning support from the catalog. The upstream model input remains editable if the request fails or the catalog is empty. The outbound request is restricted to the fixed OpenAI models endpoint, disables redirects, and bounds the response body.

## Remaining verification

The implementation has not been exercised with a live ChatGPT account. A live sign-in must verify the currently issued callback `client_id`, the actual token response shape (including `scope` and `earliest_refresh_at`), account/plan statuses, browser behavior, and a second sign-in using the issued ID. A live account is also needed to verify refresh rotation, remote revocation, and the account-specific model catalog. CGO-disabled build and JavaScript syntax checks passed; no automated tests have been added or run.
