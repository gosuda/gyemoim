# Sign in with ChatGPT

Gyemoim follows OpenAI's [Sign in with ChatGPT registration and sign-in flow](https://developers.openai.com/siwc/token-sharing-open-source/sign-in) and [account and session guidance](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions). It starts sign-in only after a user presses **Connect OpenAI account** or **Reconnect**. Startup stays offline. The browser leaves the local UI for the authorization page and returns to the same listener at `http://127.0.0.1:<port>/auth/callback`.

## Authorization flow

For each start, Gyemoim loads the OpenID discovery document from `https://auth.openai.com/.well-known/openid-configuration` through a 30 second, no-redirect HTTPS client restricted to `auth.openai.com`. It checks the issuer and the authorization, token, JWKS, and revocation endpoints against the expected Sign in with ChatGPT URLs. It does not make the revocation request in T07.

The first sign-in uses `client_id=dynamic_agent_client`, `agent_name_hint=Gyemoim`, and the persistent local `ext_agent_host_id` URN. On its callback, Gyemoim requires the provider-issued `client_id` and saves it before the token exchange. An unsuccessful first exchange therefore keeps that issued registration identifier for a later attempt. Later sign-ins use the issued ID, omit `agent_name_hint`, and may include the last stored ID token as `id_token_hint` and the verified email as `login_hint`.

Every start creates a random state, nonce, and PKCE S256 verifier. The in-memory state expires after ten minutes; a provider can have only one active flow, and a new start replaces its earlier flow. A state is consumed once. The token exchange sends the authorization code, issued client ID, verifier, same redirect URI, and `resource=https://api.openai.com/v1`. It has no client secret.

Requested scopes are `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`. The token response's `scope` string is the only grant evidence. Gyemoim verifies the ID token signature and issuer through `coreos/go-oidc`, checks its issued-client audience, expiry and issue time, requires a subject, and checks the nonce. On a returning sign-in, the verified subject must match the saved registration before any credential replacement. Only an email marked verified in that token is retained; if a later validated token omits verified email, Gyemoim keeps the address already retained from an earlier validated token. Callback query values are not identity data.

The complete token set, verified subject/email, registration ID, scopes, expiry, earliest-refresh metadata, and provider status are committed together. `earliest_refresh_at` is kept as opaque JSON text for the later refresh task. Tokens are excluded from Provider API responses and ordinary JSON serialization.

## Connection status

The requested scope list is not treated as proof that the account granted those scopes. A nonempty authoritative `token.scope` must include `chatgpt.tokens.use.direct` before the Provider is marked `connected`. When `scope` is absent, a validated identity and token set can still be retained, but the Provider is marked `plan_usage_disabled` because the grant cannot be inferred. If a reported scope set lacks `offline_access`, or the response has no refresh token, the Provider is marked `require_reauthentication`. A connected status means the direct-use scope was granted; refresh behavior and inference permission enforcement are implemented in later tasks.

The Provider page explains these states and offers a full-page sign-in action. Callback results redirect to the local root with a fixed result code. The UI maps only those fixed codes to messages and removes the query string. The callback does not apply Origin or CSRF checks because it is a top-level provider redirect; the listener's exact loopback Host guard still applies. The management guard permits only the callback's fixed, read-only result query on `/` through Fetch Metadata's cross-site navigation check. Callback responses set no-store, no-referrer, nosniff, and restrictive CSP headers. OAuth codes, errors, token values, and upstream error bodies are not returned in callback pages or logs.

## Remaining verification

The implementation has not been exercised with a live ChatGPT account. A live sign-in must verify the currently issued callback `client_id`, the actual token response shape (including `scope` and `earliest_refresh_at`), account/plan statuses, browser behavior, and a second sign-in using the issued ID. Until that manual run, the tested surface is limited to build checks, local start URL inspection, callback state rejection, and offline startup. T08 remains responsible for refresh, disconnect, and provider catalog work.
