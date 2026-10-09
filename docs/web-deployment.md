# Web deployment

This document describes running Gyemoim as an always-on web service behind a
reverse nginx proxy, reachable by remote agents from multiple locations. For
local-only use, the defaults (loopback WebUI, no ceremony) apply and none of
this is required.

## Constraints

- **Sign in with ChatGPT accepts only loopback redirect URIs.** The dynamic
  registration flow (`client_id=dynamic_agent_client`) rejects any non-loopback
  `redirect_uri` with `invalid_authorize_request` / `param: "redirect_uri"`.
  The documentation confirms: the scheme, host, and path (`/auth/callback`)
  are fixed; only the port may vary; `localhost` must not be substituted.
  Static client IDs with custom redirect URIs are limited to commercial
  partners (waitlist).
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
        ├─ /auth/callback      no session (top-level provider redirect)
        └─ /v1/*               Bearer only (service accounts)
```

- TLS, HTTP→HTTPS redirect, and any network-level rate limiting stay in nginx.
  Required nginx settings for SSE: `proxy_buffering off`, `proxy_cache off`,
  `proxy_read_timeout 3600s` (reasoning models may stay silent for minutes) and
  `proxy_set_header Host $http_host`. The application **ignores all forwarded
  headers**, so `X-Forwarded-For` / `X-Forwarded-Proto` configuration is
  optional and only useful for nginx's own logging.
- The nginx→gyemoim hop is plain HTTP over the LAN. The firewall on the gyemoim
  host must allow :9092 only from the nginx host; any further hardening of this
  hop is out of scope.

## Behavior

1. **Multi-user management login.** All logged-in users have full management
   rights (personal-tool scope). Passwords are hashed with argon2id (pure Go,
   respects the CGO constraint); there is no length or composition policy —
   only emptiness is rejected.
2. **Sessions: 24 h absolute expiry, no sliding renewal.** The cookie is
   `gym_session` (plain name, no `__Host-` prefix, no `Secure` flag —
   plain-HTTP LAN access is the primary usage mode, and a `__Host-`/`Secure`
   cookie would be dropped by browsers on plain HTTP) and carries a 256-bit
   random ID; only its SHA-256 hash is stored (same pattern as `local_keys`).
   Cookie flags: `HttpOnly`, `SameSite=Lax`, `Path=/` (`SameSite=Lax` keeps
   baseline cross-site POST protection while the OAuth result landing survives
   the provider's top-level redirect). The session ID is re-issued at login
   (fixation defense). Changing a password revokes all of that user's other
   sessions. Expired sessions are pruned opportunistically.
3. **Bootstrap.** On first start with an empty `users` table, the server
   creates user `admin` with a generated random password printed once to
   stderr (journald collects it) and `must_change_password` set; the UI forces
   a password change before any other screen. There is no web-based setup page
   (race on a public host).
4. **No public-URL flag, no forwarded-header trust.** URLs are derived from
   each request: the OAuth redirect URI stays the hardcoded loopback form, and
   the pi-config base URL is built **client-side** by the UI from
   `window.location.origin` (the browser knows the real scheme; the server
   does not need to). Forwarded headers (`X-Forwarded-For`,
   `X-Forwarded-Proto`, ...) are **never trusted**: client identity is the TCP
   peer only (the nginx host for all proxied traffic), so nothing in the app
   (backoff, logs) can be spoofed via headers.
5. **No browser-facing web security layer.** There is no CSRF token, no
   Origin/Sec-Fetch-Site check, and no security header set (CSP, CORP,
   Referrer-Policy, X-Content-Type-Options, X-Frame-Options); plain-HTTP LAN
   access is the primary mode. Session authentication, login backoff, and the
   bearer-authenticated `/v1` surface are unaffected. Residual risk: a
   malicious page in the same browser can drive mutations; `SameSite=Lax` on
   the session cookie retains baseline cross-site POST protection, and the
   auth value is still required for every action.
6. **Listener.** `--listen <addr>` sets the bind address, default
   `127.0.0.1:9092`; `--port` remains an alias for the port-only form
   (`--listen` wins when both are given). A deployed server binds `:9092`
   (all interfaces) so the remote nginx can reach it; the firewall restricts
   the port to the nginx host.
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
      (only the port varies from the normal flow). The server returns the
      authorization URL.
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
   - The in-server OAuth start/callback flow (loopback) stays available for
     users who run the UI locally.
   - A failed exchange leaves the provider `disconnected`; the connect panel
     reports failure at code expiry, and the script's terminal output is the
     immediate feedback channel.
8. **Login brute-force defense.** In-memory per-username exponential backoff
   after repeated failures (1 s base doubled per consecutive failure, capped
   at 15 min; reset on process restart is acceptable); there is no per-IP
   dimension because client IPs are not trusted. Rejections inside a window
   are cheap and do not extend it. Constant-time comparisons; failures are
   logged to stderr, not to history.
9. **Re-authentication state is surfaced.** When a provider's refresh fails
   with `require_reauthentication` (or the connection otherwise lapses), the
   provider card in the UI shows the state and a *Connect* action pointing at
   the connect-script flow; the flow notes that usage attribution and remote
   revocation for transferred sessions are upstream limitations, so a later
   re-authorization repeats the local flow.

## Database schema (v2)

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

Timestamp columns are stored as TEXT RFC3339Nano following the v1 convention
(so expiry comparison is lexicographic), and foreign keys are enabled in the
DSN so `ON DELETE CASCADE` works as declared. Schema migrations run
automatically at startup and are forward-only: an older database version is
migrated in place, and a database written by a newer version is rejected with
an explicit error instead of being downgraded.

## Deployment

### Build and install

Requires Go 1.25+; `CGO_ENABLED=0` is mandatory (SQLite is pure-Go
`modernc.org/sqlite`).

```sh
./scripts/build-release.sh                                  # linux/darwin × amd64/arm64 into ./dist
CGO_ENABLED=0 go build -trimpath -o gyemoim ./cmd/gyemoim   # current platform only
```

Install the artifact matching the server platform (e.g.
`dist/gyemoim-linux-amd64`) as `/usr/local/bin/gyemoim`. The ChatGPT
enrollment script is Python served by the server itself — no additional build
targets. `zstd` must be present in the server's `PATH` for history
compression: without it, new history segments are recorded uncompressed, but
already-compressed segments cannot be validated or queried until `zstd`
returns (fresh installs are unaffected).

### Data directory

On Linux, the data directory is `$XDG_DATA_HOME/gyemoim` when
`XDG_DATA_HOME` is set to an absolute path, otherwise
`~/.local/share/gyemoim` (see `internal/datadir`). It holds `config.db`,
`history/`, and the process lock, and is created with owner-only permissions
(0700). A service account therefore needs a writable directory referenced
through `XDG_DATA_HOME`; the systemd recipe below points it at `/var/lib`.

### systemd unit (gyemoim host)

```ini
[Unit]
Description=Gyemoim LLM gateway
After=network-online.target
Wants=network-online.target

[Service]
User=gyemoim
Group=gyemoim
StateDirectory=gyemoim
Environment=XDG_DATA_HOME=%S
ExecStart=/usr/local/bin/gyemoim --listen :9092
Restart=on-failure
RestartSec=5s

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
PrivateTmp=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

- `StateDirectory=gyemoim` makes systemd create `/var/lib/gyemoim` owned by
  `User=`. For system services `%S` expands to `/var/lib`, so
  `Environment=XDG_DATA_HOME=%S` resolves the data directory to
  `/var/lib/gyemoim` — the app appends `gyemoim` to `XDG_DATA_HOME`, so use
  `%S`, **not** `%S/gyemoim` (that would nest the directory twice).
  Directories declared in `StateDirectory=` remain writable under
  `ProtectSystem=strict`, so no extra `ReadWritePaths=` is needed.
- `--listen :9092` binds all interfaces so the remote nginx host can reach
  the port; the firewall section below restricts it to the nginx host.
- **First start**: with an empty users table, Gyemoim creates the user
  `admin` and prints a one-time initial password to stderr, which journald
  collects: `journalctl -u gyemoim`. Log in with it immediately — the UI
  forces a password change before any other screen — and change it. The
  plaintext is never persisted in the database or history; once changed, the
  journald copy is inert (you may drop old journal entries with
  `journalctl --vacuum-time` / `--vacuum-size` if you prefer).
- A second process on the same data directory fails on the process lock; the
  `Restart=on-failure` policy is safe because the previous process releases
  the lock on exit.

### nginx server block (proxy host)

TLS terminates on a different LAN host; the nginx→gyemoim hop is plain HTTP.

```nginx
server {
    listen 80;
    server_name gyemoim.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;
    server_name gyemoim.example.com;

    ssl_certificate     /etc/letsencrypt/live/gyemoim.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/gyemoim.example.com/privkey.pem;

    location / {
        proxy_pass http://192.0.2.10:9092;   # gyemoim host on the LAN
        proxy_http_version 1.1;
        # SSE: reasoning models may stay silent for minutes and the gateway
        # imposes no total stream timeout.
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        # $http_host preserves the host:port the browser used, so proxied
        # requests are indistinguishable from direct access (any Host-derived
        # behavior, logs, and diagnostics see the real value); $host (which
        # strips the port) would change the seen host:port.
        proxy_set_header Host $http_host;
        # The app ignores X-Forwarded-* entirely; set them only for nginx's
        # own logging.
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Issue certificates with certbot (`certbot certonly --nginx -d
gyemoim.example.com`, or the `--nginx` installer to let certbot manage the
blocks above); renewal follows certbot's standard timer. The app is reachable
over plain HTTP from the LAN on :9092 — keep the firewall rule below tight.

### Firewall (gyemoim host)

Allow tcp/9092 only from the nginx host (examples assume nginx at
192.0.2.11):

```sh
# ufw
ufw allow from 192.0.2.11 to any port 9092 proto tcp

# nftables
nft add rule inet filter input ip saddr 192.0.2.11 tcp dport 9092 accept
```

All other sources must be denied on :9092 (the host's default policy, or an
explicit ufw deny). No other inbound port is needed: the enrollment script
runs on the admin's browser machine and talks to the server over the same
https origin.

### Admin quickstart

1. **First login** — open `https://<domain>/`, sign in as `admin` with the
   bootstrap password from journald, and set a new password (the UI forces
   this before anything else).
2. **Users** — add users in the Users panel; new users also get a forced
   password change at first login. All signed-in users have full management
   rights (personal-tool scope).
3. **Connect a ChatGPT account** — create an OpenAI Provider, press
   *Connect*, and the provider card shows a single-use enrollment code
   (~10 min TTL) and a download link for `gyemoim-connect.py`. On the machine
   that has the browser (with `python3`), run the printed command:
   `python3 gyemoim-connect.py https://<domain> <code>`. The script opens the
   OpenAI authorization page and forwards the result to the server; the
   server owns registration, PKCE, the exchange, ID-token verification, and
   all later refreshes. Re-authentication later uses the same flow.
4. **Service accounts** — create a ServiceAccount and issue its key, define
   a Model (one Provider + upstream model), and grant the Model to the
   ServiceAccount (grants are always explicit).
5. **Remote agents** — point them at `https://<domain>/v1` with the
   ServiceAccount key as Bearer (`POST /v1/responses`, `GET /v1/models`).

### Upgrades

Replace the binary and restart the unit. Configuration schema migrations run
automatically at startup and are forward-only: an older database version is
migrated in place, and a database written by a newer version is rejected with
an explicit error instead of being downgraded.

## Known limitations and unverified upstream behaviors

- Whether OpenAI accepts a redirect_uri whose port differs between start and
  callback (docs say only the port may vary; the thin-bridge flow keeps them
  identical by construction — the server uses the script's port for both).
- Live refresh of a transferred session on the server is officially supported
  by the self-hosted VMs procedure but has not been exercised end-to-end.
- Behavior when the same ChatGPT account is connected on several servers (one
  issued client ID per registration — confirm refresh does not invalidate the
  other host's session).
