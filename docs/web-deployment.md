# Web deployment plan

Status: approved plan, not yet implemented. This document records the confirmed
decisions for running Gyemoim as an always-on web service behind a reverse nginx
proxy, reachable by remote agents from multiple locations. Design decisions here
should be folded into `design.md` (and `oauth.md`) as they land.

## Confirmed constraints

- **Sign in with ChatGPT accepts only loopback redirect URIs.** The dynamic
  registration flow (`client_id=dynamic_agent_client`) rejects any non-loopback
  `redirect_uri`. Verified live on 2026-10-08: an authorization request with
  `redirect_uri=http://<LAN-IP>:<port>/auth/callback` fails with
  `invalid_authorize_request` / `param: "redirect_uri"`. The documentation
  confirms: the scheme, host, and path (`/auth/callback`) are fixed; only the
  port may vary; `localhost` must not be substituted. Static client IDs with
  custom redirect URIs are limited to commercial partners (waitlist).
- **The official procedure for headless/remote hosts is credential transfer**
  (`Self-hosted VMs`, developers.openai.com/siwc): complete OAuth on a machine
  with a browser, transfer the credential record to the server over a secure
  channel, and let the server own later token refreshes using its own host ID.
  Usage attribution and remote revocation for transferred sessions are not yet
  available upstream; a later re-authorization must repeat the local flow.

## Architecture

```
browsers / remote agents
        │ https://<domain>            (TLS terminated by nginx)
        ▼
   nginx reverse proxy (a DIFFERENT host on the LAN)
        │ proxy_pass http://<gyemoim-host>:9092   (plain HTTP over the LAN)
        ▼
   gyemoim (--listen :9092, firewall restricted to the nginx host)
        ├─ /login, /assets/*   session auth not required
        ├─ / , /api/*          session required (management UI + API)
        ├─ /auth/callback      no session (top-level provider redirect, unchanged)
        └─ /v1/*               Bearer only (service accounts, unchanged)
```

- TLS, HTTP→HTTPS redirect, and any network-level rate limiting stay in nginx.
  Required nginx settings for SSE: `proxy_buffering off`, `proxy_cache off`,
  `proxy_read_timeout 3600s` (reasoning models may stay silent for minutes) and
  `proxy_set_header Host $host`. The application **ignores all forwarded
  headers** (see decision 9), so `X-Forwarded-For` / `X-Forwarded-Proto`
  configuration is optional and only useful for nginx's own logging.
- The nginx→gyemoim hop is plain HTTP over the LAN. The firewall on the gyemoim
  host must allow :9092 only from the nginx host; any further hardening of this
  hop is out of scope for now.

## Decisions

1. **Multi-user management login.** All logged-in users have full management
   rights (personal-tool scope; a future `is_admin` column can refine this).
   Passwords hashed with argon2id (pure Go, respects the CGO constraint);
   minimum length 12.
2. **Sessions: 24 h absolute expiry, no sliding renewal.** Cookie
   `gym_session` carries a 256-bit random ID; only its SHA-256 hash is stored
   (same pattern as `local_keys`). Cookie flags: `HttpOnly`, `SameSite=Lax`
   (so the OAuth result landing survives the provider's top-level redirect),
   and `Secure` **always** (the UI is meant to be reached via the https nginx;
   browsers treat `http://localhost` as trustworthy so local use and curl-based
   testing still work, but plain-http LAN access to the UI will not keep a
   login). The session ID is re-issued at login (fixation defense). Changing a
   password revokes all of that user's other sessions. Expired sessions are
   pruned opportunistically.
3. **Bootstrap.** On first start with an empty `users` table, create user
   `admin` with a generated random password printed once to stderr (journald
   collects it) and `must_change_password` set; the UI forces a password change
   before any other screen. No web-based setup page (race on a public host).
4. **No public-URL flag, no forwarded-header trust.** URLs are derived from
   each request: the OAuth redirect URI stays the hardcoded loopback form, and
   the pi-config base URL is built **client-side** by the UI from
   `window.location.origin` (the browser knows the real scheme; the server
   does not need to). The Host guard (421 on anything but `127.0.0.1:<port>`)
   is removed entirely. Forwarded headers (`X-Forwarded-For`,
   `X-Forwarded-Proto`, ...) are **never trusted**: client identity is the TCP
   peer only (the nginx host for all proxied traffic), so nothing in the app
   (backoff, logs) can be spoofed via headers.
5. **Origin validation becomes request-relative.** When an `Origin` header is
   present it must match the request's own Host (host:port, scheme-agnostic
   because the app sees plain HTTP behind nginx). Userinfo/path/query checks
   and the Sec-Fetch-Site logic are unchanged. CSRF header checking is
   unchanged. DNS-rebinding defense therefore rests on origin checks + CSRF,
   consistent with the original threat model.
6. **Listener.** New `--listen <addr>` flag, default `127.0.0.1:9092`
   (current behavior). `--port` remains as an alias for the port-only form.
   The deployed server binds `:9092` (all interfaces) so the remote nginx can
   reach it; the firewall restricts the port to the nginx host.
7. **Credential transfer via a server-distributed Python script.** The admin
   logs into the server UI, presses *Connect OpenAI account*, and the UI issues
   a **single-use enrollment code** (random, ~10 min TTL, bound to the target
   provider) plus a download of a small Python script (stdlib only, Python
   assumed present on the admin's Windows/macOS machine). The script is a
   **thin bridge** — it contains no OAuth logic; the server keeps owning the
   whole `siwc` state machine, including dynamic registration with the
   **server's own `ext_agent_host_id`**, PKCE, the code exchange, and ID-token
   verification:
   1. The script claims the enrollment code against the server and starts a
      loopback HTTP server on `127.0.0.1:<ephemeral-port>`.
   2. The script asks the server to start the connect flow for the code's
      provider with `redirect_uri=http://127.0.0.1:<port>/auth/callback`
      (only the port varies from the normal flow — the one change needed in
      `siwc` is parameterizing the pending flow's redirect URI). The server
      returns the authorization URL.
   3. The script opens the system browser (printing the URL as fallback).
      OpenAI redirects to `127.0.0.1:<port>/auth/callback?code=...&state=...`.
   4. The script forwards `code` + `state` to the server endpoint. The server
      verifies the code is unused/unexpired, then completes via the existing
      callback path (`exchangeCode`, ID-token verification, atomic
      `ReplaceProviderCredentialsWithStatus`) and returns the outcome, which
      the script prints.
   - Distribution: the script is embedded in the server binary (`go:embed`)
     and served authenticated behind the management session. No Go connect
     subcommand, no extra cross-compile targets, no `build-release.sh`
     changes.
   - Because the token exchange happens server-side over TLS to OpenAI, the
     script needs no crypto beyond stdlib; the only network calls are the
     three to the server (claim/start, forward, status) plus serving the one
     local redirect.
   - No manual file upload and no export endpoint; there is exactly one
     transfer path, protected by the one-time code.
   - The existing in-server OAuth start/callback flow stays unchanged
     (loopback) for users who run the UI locally.
8. **Login brute-force defense.** In-memory per-username exponential backoff
   after repeated failures (reset on process restart is acceptable); there is
   no per-IP dimension because client IPs are not trusted (decision 9).
   Constant-time comparisons; failures logged to stderr, not to history.
9. **Forwarded headers are ignored everywhere.** No XFF parsing, no
   X-Forwarded-Proto scheme detection; request-relative Origin checks already
   work scheme-agnostic, cookies are always `Secure`, and the pi-config URL is
   client-side. Login CSRF defense: the login page carries the same injected
   CSRF token mechanism and `/api/auth/login` requires the header.
10. **Re-authentication state is surfaced.** When a provider's refresh fails
    with `require_reauthentication` (or the connection otherwise lapses), the
    provider card in the UI shows the state and a *Connect* action pointing at
    the connect-script flow; the flow notes that usage attribution and remote
    revocation for transferred sessions are upstream limitations, so a later
    re-authorization repeats the local flow.

## Schema migration (v1 → v2)

```sql
CREATE TABLE users (
    id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL, must_change_password INTEGER NOT NULL DEFAULT 0,
    disabled_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE sessions (
    id_hash BLOB PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL
);
```

`PRAGMA user_version` migration follows the existing pattern; opening an older
database must keep working.

## Implementation log

- **Step 1 — done (2026-10-09).** `--listen` flag added (`--listen` wins over
  `--port`; port-only `--port` alias kept), Host guard and its plumbing removed,
  Origin checks now compare host:port against the request's own Host
  (scheme-agnostic, case-insensitive, portless matches only portless).
  Listener switched `tcp4`→`tcp` for IPv6 `--listen` support. Verified:
  `go vet` clean, build clean, default loopback behavior unchanged (200),
  arbitrary Host accepted (421 gone), forged Origin 403, matching Origin passes
  the guard, port mismatch 403, Origin-with-path 403, Sec-Fetch-Site kept,
  missing CSRF 403, `--listen :9092` reachable via loopback and LAN IP.
- **Step 2 — done (2026-10-09).** Schema v2 (`users`, `sessions` + index
  `sessions_by_user`) behind a stepwise `migrate()` chain (v0 → v1+v2 in one
  transaction; v1 → v2 in place). Timestamps follow the v1 TEXT RFC3339Nano
  convention (deviation from the plan's INTEGER sketch, justified by
  lexicographic expiry comparison); foreign keys were already enabled in the
  DSN so `ON DELETE CASCADE` works as declared. Typed accessors in
  `internal/config/users.go` (hash-bearing getters for login, hash-free list,
  explicit `mustChange` flag on password update, delete-other-sessions,
  expired-prune). Verified: vet/build clean; fresh DB lands at v2; a real v1 DB
  (seeded provider/service-account/key/model/grant) migrates with every row
  preserved value-by-value; accessors exercised via a throwaway `go run`
  probe (duplicate username → ErrConflict, cascade delete, prune count).

## Work breakdown

| # | Task | Verification |
|---|---|---|
| 1 | `--listen` flag (+ `--port` alias); remove Host guard; request-relative Origin checks | curl: arbitrary Host accepted; forged Origin still 403; loopback behavior unchanged |
| 2 | Schema v2 migration (users, sessions) | open existing v1 DB and a fresh DB; `go vet` |
| 3 | argon2id hashing; login/logout/change-password endpoints (login CSRF, password change revokes other sessions); session middleware and routing | curl: unauthenticated access blocked, cookie flags, 24 h expiry, CSRF still enforced |
| 4 | Bootstrap (first-start random password to stderr, forced change) | first run prints password; APIs blocked until change |
| 5 | Login failure backoff (per-username) | repeated failures slow down |
| 6 | UI: login screen, forced change, user management, logout; pi-config URL built client-side from `window.location.origin` | manual browser pass; `node --check internal/httpui/assets/site.js` |
| 7 | Enrollment codes; connect-flow start with parameterized redirect port in `siwc`; code-forward completion endpoint; embedded Python connect script + authenticated download route | live round-trip: connect from a laptop, credential lands on server, run inference; forged/expired/reused code rejected |
| 7b | UI connect page: enrollment code display, script download, status polling; re-authentication state on provider cards | browser pass on the UI page; provider card shows re-auth state after forced expiry |
| 8 | Deployment notes (nginx on separate host, firewall, systemd unit) in this document; update `design.md` / `oauth.md` | docs review |

Steps 4–7 need a real ChatGPT account for full live verification.

## Open items / not yet verified

- Live round-trip of the connect flow (Python script on Windows/macOS →
  server) with a real ChatGPT account.
- Whether OpenAI accepts a redirect_uri whose port differs between start and
  callback (docs say only the port may vary; the thin-bridge flow keeps them
  identical by construction — the server uses the script's port for both).
- Live refresh of a transferred session on the server (officially supported by
  the self-hosted VMs procedure; still needs a live pass).
- Behavior when the same ChatGPT account is connected on several servers (one
  issued client ID per registration — confirm refresh does not invalidate the
  other host's session).
- Whether login backoff state should also rate-limit by connection count at the
  admission layer (P2; nginx covers the first line).
