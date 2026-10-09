# Web UI overhaul plan

Started 2026-10-09. Consumes `ui-review.md` (usability findings). The goal is a
usability overhaul of the management WebUI — not a visual redesign. Working method is
the established one: each step is implemented by a spawned subagent, reviewed and
verified by the orchestrator, then committed as one commit.

## Goal

Fix the usability problems recorded in `ui-review.md`: make the setup chain
clickable, make success/failure feedback human and consistent, make "is anything
actually working?" visible, remove hostile inputs, and give the app URL routing —
while preserving everything on the review's keep-list.

## Non-goals

- No visual redesign/reskin; the current panel skeleton, spacing, and palette stay.
- No framework, no npm/frontend toolchain, no build step. Vanilla JS/CSS only.
- No change to behavioral invariants: explicit errors (no auto-retry/masking), bounded
  history queries, no silent capability drops, admission-before-recording,
  no-auto-grant, single-target routing.
- Deferred backend ideas (see ui-review.md): deletion dry-run preview, oldest-record
  date, key last-used timestamps, per-user session listing, roles.

## Constraints

- Assets are embedded via `go:embed` in `internal/httpui/ui.go` and served as static
  files; `index.html`/`login.html`/`change-password.html` are parsed by `html/template`
  (CSRF injection). Rebuild the binary to see asset changes.
- Strict CSP `script-src 'self'` (plus `style-src 'self'`): no inline scripts/styles,
  no inline event handlers, no import maps (inline JSON). ES modules with **relative
  imports** work fine under this CSP.
- UI text in English. No `*_test.go`; verification is `go vet`, builds,
  `node --check` per changed JS file, and manual browser/API passes.

## Decisions

1. **Routing: hash-based, SPA stays single-page.** `#/overview` … `#/users`; unknown
   hash redirects to `#/overview`; parameters ride in the hash query
   (`#/requests?outcome=failed&select=<id>`). `navigate()` writes the hash; a
   `hashchange` listener renders; load reads the hash. Real-path (History API) routing
   is rejected: it needs server route changes for no additional benefit. Deep links
   replace the `state.pendingRequestID` side channel.
2. **Module split, no bundler.** `site.js` (2,199 lines) is split into ES modules
   under `assets/js/` — `api.js` (fetch/CSRF helpers), `dom.js` (helpers, show/hide),
   `format.js` (date/bytes/UTC), `state.js` (shared mutable state), `nav.js`
   (routing/nav), `pages/{overview,providers,accounts,models,requests,storage,users}.js`,
   `app.js` (entry; index.html loads `<script type="module" src="/assets/js/app.js">`).
   The split is a **pure move refactor first** (no behavior change), verified by a full
   browser walk before any page is restructured. `login.js` stays standalone.
3. **Error humanization layer.** A small mapping module translates known
   generic/conflict responses (409 `resource conflicts…`, JSON-field leaks like
   `providerId and upstreamModel are required`) into labeled, actionable sentences,
   keyed by endpoint+status. Server messages that are already specific pass through
   verbatim. Never auto-retry; never hide an unexpected error.
4. **Feedback standard.** Every mutating action ends in exactly one of: an inline
   success message that names the consequence, or a card/row update with
   `scrollIntoView`. Messages clear on input. Loading states disable their buttons.
   Freshness is shown as "Updated HH:MM:SS UTC" in the status line slots that today
   hold permanent explainer text (the explainers move to subtitles).
5. **Cross-page linking convention.** Empty states and next-step prose that reference
   another page become real links (`navigate(page, params)`). Every empty state
   carries an actionable link when a next step exists.
6. **UTC policy.** Timestamps display with explicit UTC (local time may accompany).
   Time-range inputs get presets ("Last hour", "Last 24 hours", "Today (UTC)") and,
   where a picker is used, it is labeled UTC and converted on submit. Client-side
   validation messages match what the backend actually accepts.
7. **Terminology.** Capital-M "Model" is reserved for the route alias; "upstream model
   ID" elsewhere. "harness" becomes "agent(s)" in UI text. Pi-specific sections are
   labeled and explained where they appear. Internal enums get human labels + a legend
   where first shown.
8. **Auto-refresh:** Overview's usage/performance panels only, 30 s, paused while
   `document.hidden`; every other page keeps manual refresh (with a staleness stamp).
9. **Backend support additions (step 3 only):** optional key label (config schema
   migration + API field), `lastLoginAt` on users (updated at successful login), and
   an in-memory ring buffer of the last 100 pre-admission auth rejections (time, key
   hint, code, requested model) exposed via `GET /api/rejections`. The ring buffer
   lives beside the harness admission path, records only post-rejection, and does not
   interact with request recording or the inference semaphore.
10. **Keep-list is binding.** The review's keep-list items (ARIA discipline, confirm
    wording, show-once pattern, explicit errors, panel survival, responsive base) are
    acceptance criteria for every step.

## Work breakdown

### Step 1 — Hash routing + document.title
site.js only (pre-split; routing lands in `navigate()` and moves in step 2).
Hash write/parse, `hashchange`, unknown→`#/overview`, param plumbing
(`#/requests?…` replaces `pendingRequestID`'s cross-page hand-off), `document.title`
per page, Back/Forward/reload/bookmark verified. Log out and login redirects still
land sensibly (`/` → `#/overview`).

### Step 2 — Module split (pure refactor)
Move site.js into the step-2 module layout above; zero behavior change. CSP-safe
relative imports; `node --check` every module; full manual walk of all seven pages
plus login/change-password; compare behavior against step 1 (panel survival, polling,
countdowns, confirm dialogs).

### Step 3 — Backend UX support
(a) `local_keys.label` nullable text (schema migration in the existing chain; API
accepts/returns optional label; UI wiring comes in step 7); (b) users API gains
`lastLoginAt`, updated transactionally at successful login; (c) auth-rejection ring
buffer + `GET /api/rejections` (CSRF/GET-only read like other management endpoints).
Invariants: rejection recording happens only after the admission decision, never
blocks or gates inference, and adds no allocation on the hot path beyond the ring
push. curl verification for each.

### Step 4 — Shell + Overview
Health strip on Overview (Providers x/y connected · Models n · Service accounts n,
each a link) fetched from the existing list APIs; first-run checklist card when 0
connected providers or 0 models; consolidate the four empty states into one actionable
empty state; drop the eyebrow breadcrumb (A2) and keep h1+section h2; drive both green
dots from `/api/status` (red on failure) and fix the `font-size: 0` badge (A5); skip
link (A6); runtime status dot-chain split (B6); status-line hygiene (B4) with
"Updated …" stamps (B3); outcome labels+legend and failed-row tint (B7/B8); 30 s
auto-refresh for the usage/performance panels (decision 8).

### Step 5 — Shared feedback/error/link utilities
Build the decision-3 mapping module, decision-4 helpers (success message + clear-on-
input + scrollIntoView + busy states), and the decision-5 `navigate` link helper.
Apply them as part of each later page step; this step only ships the utilities plus a
representative application (provider duplicate-name mapping) and unit-level checks.

### Step 6 — Providers page
Connect panel: re-click reveals the existing panel instead of reissuing (P2); waiting
line gains terminal-failure guidance (P1); outcome notice also lands on the card +
scrollIntoView (P4); expired panel disables code/copy (P10); "other machine" wording
(P8); python3 requirement line; `pre-wrap` on the command (P5); create success names
the next step and scrolls (P6); delete confirm mentions Models + 409 mapping (P3/P7);
rename success feedback; last-attempt line if the data already exists client-side
(P9, no backend change).

### Step 7 — Service accounts page
Readiness on the collapsed summary (`n keys · m of k Models granted`, or a readiness
dot) (S3); grant empty state links to Models and vice versa (S1); key label input +
labeled key rows (needs step 3a) (S4); leave-guard confirm when a revealed key is
undismissed (S5); Rename into the header action row (S6); recent-rejections panel fed
by step 3c (S2); jargon rewording, revoked-key removal affordance, Disable confirm
hint, create feedback + scroll (S7).

### Step 8 — Users page + login/change-password
Temp-password handoff: reveal toggle now, one-time copyable handoff block after create
(reusing the show-once pattern) (U1); reset success keeps the form open with the
consequence message, plus a pre-submit hint (U2); disable confirm (U5); duplicate-
username mapping + field invalid marking (U3); `lastLoginAt`/"Never signed in" on
cards (needs step 3b) (U4); stale-message clearing, 64-char hint, "You" explanation
(U6). login.js error styling (C1); change-password: temp-password hint, logout escape
hatch, wording fixes (C2/C3/C4).

### Step 9 — Requests page
Presets + UTC-labeled pickers or corrected text inputs with concrete example hints and
accurate split validation messages matching the backend (R1); UTC (+local) display in
table/detail (R2); unrecognized-ID non-blocking warnings + datalist affordance
signaling (R3); empty-state differentiation, summary phrasing, Refresh uses applied
filters, filter-local errors with `aria-invalid` (R4); outcome/breakdown links from
Overview and model cards as param'd deep links (R4/M5); clickable rows + action column
header; 13 px filter inputs (R5).

### Step 10 — Storage page
Health banner + collapsible debug detail; hide idle-only deletion rows and move
deletion state into the deletion panel (T1); confirm restates the effective range
incl. the submission-time cap; UTC `max` on date inputs + live range preview (T2/T3);
stale-message clearing + form disable while running (T4); three tiles with pending
folded into the raw caption + one-sentence segment definition (T5); "Potentially lost
records" rename + conditional red (T6); debug renames (T7); Refresh label (T7).

### Step 11 — Terminology sweep + docs fold-in
Model glossary line and hint rewrites (M3), Pi explanation (M4), "Load models"
enable-with-message fix (M1 — may land in step 5's utilities if earlier), provider
status tag on Model cards (M2), remaining enum capitalization/legends, any leftover
9 px fonts, final full-browser regression over all seven pages + login/change-
password, and folding decisions into `design.md` ("Web UI overhaul (agreed)").

## Implementation log

Appended per step with verification evidence.

### Step 1 — Hash routing + document.title (2026-10-09)

`internal/httpui/assets/site.js` only; `index.html` unchanged. `navigate(page, params)`
(site.js:150) is now a thin wrapper: it builds `#/<page>?<query>` via `URLSearchParams`,
no-ops when the hash already equals the target (no double render, no junk history
entry), otherwise writes `location.hash` and calls `renderRoute()` directly. The old
`navigate()` body (cancel fetches, sensitive cleanup, request-detail reset, nav
`aria-current`, view visibility, h1/breadcrumb) moved into `showPage(page, params)`
(site.js:133), which now also sets `document.title` to `<Page> · Gyemoim` (review A3).
`renderRoute()` (site.js:108) parses the hash (`parseHash`, site.js:101), renders the
page, and normalizes an empty or unknown page to `#/overview` via
`location.replace` (no history entry). A `window` `hashchange` listener drives
Back/Forward/bookmarks; an internal `renderedHash` guard makes the direct render +
queued `hashchange` pair render exactly once. Boot (site.js:2239) calls
`renderRoute()` instead of the hardcoded `navigate("overview")`.

`state.pendingRequestID` is removed. Overview Recent-performance Inspect rows call
`navigate("requests", { select: requestId })` (site.js:1504); `refreshPage` forwards
`select` to `loadRequestHistory(selectID)` (site.js:1581), which opens the detail via
`selectRequest` after the page settles (also when the list query fails, so an unknown
id still shows the detail's explicit load error). The `select` param is **kept** in
the hash (not cleared on selection/close), so reload and bookmarks re-open the same
detail; pagination/filter actions and "Close details" clear only the in-memory
selection as before. Unknown hash params are ignored; the requests page consumes only
`select`. Session-expiry redirect to `/login` and the logout flow are untouched
(login assigns `/`, which normalizes to `#/overview`).

Verification: `node --check site.js` OK; `go vet ./...` and
`CGO_ENABLED=0 go build -trimpath` OK (pre-existing committed `internal/history/
query.go` is `gofmt -l`-dirty; not touched by this step). Browser pass on a private
instance (port 9961, seeded providers/accounts/users, admin password changed): all 7
nav clicks update hash, title, `aria-current`, and leave exactly one view visible;
Back/Forward restore page+title+`aria-current`; reload on `#/users` stays on Users;
`#/bogus` normalizes to `#/overview` with no extra history entry; deep link
`#/requests?select=nonexistent` opens Requests with the detail panel showing its
explicit load error; with `/api/requests*` stubbed to a synthetic request, the deep
link (fresh load) auto-opens the detail with summary grid + 2 event cards and
highlights the row, and an Overview Inspect row click lands on
`#/requests?select=synthetic-1` with the detail auto-opened (real
`navigate(page, params)` path); logout → `/login`, re-login → `#/overview`; a nav
click on the current page is a no-op; one navigation triggers exactly one page load
(single `/api/users`+`/api/auth/me` pair per Users render). Deviations: none.

### Step 2 — Module split (pure refactor) (2026-10-09)

`internal/httpui/assets/site.js` (2,242 lines) is deleted and its IIFE body moved
verbatim into ES modules under `internal/httpui/assets/js/`, loaded from index.html
via `<script type="module" src="/assets/js/app.js"></script>` (replacing the old
`<script src="/assets/site.js" defer>` tag; modules are deferred by default, so the
form wiring that used to run at `defer` time still runs after the DOM is ready).
Final layout (line counts):

- `js/state.js` (22) — `state` and the history fetch-control `historyState` objects,
  exported and mutated by property.
- `js/dom.js` (21) — `byId`, `element`, `button`, `showMessage`.
- `js/format.js` (67) — `formatUTC/formatBytes/formatNumber/formatOffsetNS/
  formatDurationNS/formatDate` plus the shared presentational builders
  `identity/identityCell/outcomeTag` and `historyErrorMessage` (used by Overview and
  Requests; placed here rather than in a page module to keep them shared).
- `js/api.js` (101) — `api()` (CSRF header, error unwrapping, forced-change and
  session-expiry redirects), `addQuery`, and the bounded-history abort-token
  machinery (`beginHistoryFetch`, `beginHistoryChild`, `finishHistoryChild`,
  `historyFetchIsCurrent`, `historyChildIsCurrent`, `cancelHistoryFetches`,
  `abortHistoryChildren`).
- `js/nav.js` (109) — `titles`, `buildHash/parseHash/renderRoute/applyHash/showPage/
  navigate/refreshPage`, the `renderedHash` double-render guard, and the
  `data-page`/`data-refresh` button wiring.
- `js/pages/{overview,providers,service-accounts,models,requests,storage,users}.js`
  (193/331/369/231/588/126/137) — the page loaders/renderers, each owning its
  page-scoped form wiring exactly as it appeared in the monolith (e.g. the Storage
  module owns the `history-delete-form` submit handler, Requests owns the filter/
  paging/detail-close wiring).
- `js/app.js` (32) — entry point: logout wiring, `oauth_result` handling, initial
  `renderRoute()`, and the `hashchange` listener registered last, matching the
  original boot order.

Cycle/registry choice: one deliberate cycle — `nav.js` imports the seven page
loaders for `refreshPage` while page modules import `navigate` back from `nav.js`.
This is safe because both sides export only hoisted function declarations called at
runtime, never during module evaluation; no registry was needed. No other cycles
exist (`state`→none; `dom`→none; `format`→`dom`; `api`→`state`; pages→
`api/dom/format/state/nav` as needed; `app`→`nav/api/dom`). Only Overview and
Requests import `navigate` (the other pages never navigate), so the cycle is
narrow. Boundary adjustments from the plan's sketch: `addQuery` and the
abort-token machinery live in `api.js` rather than a page module (shared by
Overview + Requests + nav's `showPage`); `historyState` lives in `state.js` beside
`state`; `identity/identityCell/outcomeTag/historyErrorMessage` live in `format.js`
(Overview + Requests share them); `providerName` stayed in the service-accounts
page module (its only user).

Pure-move proof: the concatenation of all modules with `import` statements and
`export` keywords stripped was compared line-for-line (sorted diff, comment lines
excluded) against `git show HEAD:site.js` with the IIFE wrapper removed and
dedented — identical code lines, i.e. zero behavior change by construction (the
only diff initially caught was a transcription typo, `0x80` vs `0xc0` in
`utf8Preview`'s continuation-byte mask, fixed before verification). Strict mode: the
monolith already declared `"use strict"`, so module strictness changes nothing; no
sloppy-mode fixes were needed. `window` leakage: none introduced —
`Object.keys(window)` on the SPA is identical to the unchanged `/login` page
baseline; the pre-existing DOM-node expandands (`panel.connectStop`,
`details.loading`, `slot.historyChildController`, …) are unchanged.

Verification: `node --check` on every module in module syntax (each copied to
`/tmp/opencode/modcheck/*.mjs`; 13/13 OK); `go vet ./...` OK;
`CGO_ENABLED=0 go build -trimpath` OK and `./scripts/build-release.sh` builds all
four targets; the built binary serves `/assets/js/app.js` (and all sibling modules)
with `Content-Type: text/javascript; charset=utf-8` (`go:embed assets/*` picks up
the subdirectory without Go changes) and `/assets/site.js` is 404. Full browser
walk on a private instance (port 9962, XDG-isolated data dir, seeded 2 providers +
2 service accounts + 1 model + users, admin password changed via the forced gate):
login error path shows the inline error; temp-password login redirects to
`/change-password` and after the change lands on `#/overview`; all 7 pages render
with correct hash/title/`aria-current`/h1/breadcrumb and exactly one visible view;
Back/Forward restore page+title+`aria-current`; reload on `#/users` stays on Users;
`#/bogus` normalizes to `#/overview`; `#/requests?select=bogus-id` auto-opens the
detail with its explicit load error and keeps the param in the hash; with
`/api/requests*` stubbed, a fresh load of `#/requests?select=synthetic-1`
auto-opens the detail (16 summary items, 2 event cards, highlighted row) and an
Overview Inspect click lands on the same deep link; provider create shows its
success message, the connect panel opens with the single-use code and a ticking
countdown (9:59 → 9:56 observed), and the panel survives navigation away/back and
the manual Refresh button; service-account expand loads keys/grants/Pi sections and
the issue-key show-once reveal + "Dismiss and clear" work; Model form's provider
dropdown lists all seeded providers; Requests filter validation ("must include a
UTC suffix"), valid apply, and Reset all behave; Storage renders 4 tiles + 14
detail rows; Users cards show the "You"/"Password change pending" badges, the
reset-password form expands with its aria-label, and user creation adds a card;
logout → `/login`, re-login → `#/overview`; a nav click on the current page adds no
history entry; one Users render issues exactly one `/api/users`+`/api/auth/me`
pair; browser console is clean across the entire session. Deviations: none.

### Step 3 — Backend UX support (2026-10-09)

**(a) Optional key label.** Schema v3 (one new migration block appended to the
chain; `currentSchemaVersion = 3`): `ALTER TABLE local_keys ADD COLUMN label TEXT;`
plus the (b) column below in the same block — both are plain nullable TEXT, NULL
meaning "no label" / "never signed in", no timestamp-convention change. The
management key-issue endpoint (`POST /api/service-accounts/{id}/keys`) now decodes
an optional body via a new `decodeOptionalJSON` helper (empty body and `{}` still
accepted, `DisallowUnknownFields` kept): `{"label": "…"}` is trimmed of
surrounding whitespace, empty is allowed (stored as NULL), and >128 Unicode runes
is rejected with 400 `invalid_request` (documented choice: reject, not truncate).
`gateway.IssueLocalKey(ctx, accountID, label)` and `store.CreateLocalKey(…, label)`
pass it through; `LocalKey.Label string` with `json:"label,omitempty"` rides every
key metadata response, so unlabeled keys are byte-identical to before. UI wiring
is display-only in `service-accounts.js` (no form field — that is step 7): a key
row's strong text is `key.label` with a secondary muted "Key ending <hint>" span
when a label exists, otherwise today's display-hint-only row. The key-issue form
input remains untouched.

**(b) users.lastLoginAt.** Same v3 migration block:
`ALTER TABLE users ADD COLUMN last_login_at TEXT;`. `User.LastLoginAt *time.Time`
serializes as `"lastLoginAt": null` when never. The stamp is written inside the
same transaction that creates the session: `store.CreateSession` (whose only
caller is the successful-login path in `auth.go`) now wraps the session INSERT and
`UPDATE users SET last_login_at = ? WHERE id = ?` in one tx, so the login
timestamp commits atomically with the session that proves it; `updated_at` is
deliberately left alone. Consequence worth noting: the login *response body*
still shows the pre-login value (the user row is read before session creation);
the Users page refetches `/api/users`, so the UI is unaffected. All three user
queries (`GetUser`, `GetUserByUsername`, `ListUsers`) select the new column.
UI wiring is display-only in `users.js`: a second `resource-copy` line under
"Created" renders `Last signed in <formatUTC>` or `Never signed in`.

**(c) Auth-rejection ring buffer.** New `internal/httpapi/rejections.go`:
`RejectionLog` is a mutex-guarded fixed `[100]Rejection` ring with a write cursor
and count — `Record` is a constant-time, allocation-free copy into the array
(oldest entry overwritten when full, safe under concurrent rejections, nil-safe);
`Snapshot` returns newest-first copies. Entries store UTC `at`, `keyHint`
(last four characters of the presented bearer key when it has the `gym_` shape
and sane length — the same last-4 style as stored hints, never the full key,
"" for absent/malformed headers), `code`, `model` (when known), and
`serviceAccountId`/`serviceAccountName` (when authenticated). One instance is
created in `main.go` and shared: `NewHarness` records into it,
`NewManagement` reads it. Hooks cover every pre-admission rejection in the
harness path (everything before `recorder.Begin`): method-not-allowed,
`invalid_api_key`/`internal_error` in `authenticate`, `concurrent_request_limit`,
`service_unavailable`, body-read failures (`invalid_request`/`request_too_large`),
`invalid_json`, `model_required`, the gateway error mapping (now
`writeGatewayError(w, r, err, identity, model)` so `model_access_denied`,
`model_not_found`, `invalid_api_key`, and `internal_error` record with the exact
response code and the requested model name), and pre-admission provider-auth
preparation failures (client cancellations are not recorded). Post-admission
failures (recording, capability, route, upstream) never touch the ring. The ring
never interacts with request recording, history files, or the inference
semaphore. New endpoint `GET /api/rejections` (management session gate, GET-only
like other reads, newest-first array response) is served by
`managementAPI.listRejections`.

Verification: `gofmt -l` clean on all changed files; `go vet ./...` OK;
`CGO_ENABLED=0 go build -trimpath` OK and `./scripts/build-release.sh` builds all
four targets; `node --check` on both changed JS modules OK (no UI structure
changes). Migration: fresh-dir start writes `user_version = 3` with both
`ALTER TABLE` columns present (verified by a throwaway `go run` probe reading
`PRAGMA user_version` + `sqlite_master`, deleted afterwards); the upgrade path
was exercised for real by building the pre-change binary from a `git archive`
extract of HEAD in /tmp (no worktree/branch/stash), seeding a v2 database
(admin + service account + key + second user), then starting the new binary on
the same data directory: version 2 → 3 migrated, all rows preserved (key still
authenticates on `/v1/models` with 200, label/lastLoginAt NULL, no re-bootstrap).
curl pass: key without label via empty body and `{}` (metadata has no `label`
field — back-compat); key with label stored and returned by POST and GET
(whitespace-trimmed); 129-rune label → 400 "label must contain at most 128
Unicode characters"; 128-rune label accepted; unknown body field → 400. Login
updates `lastLoginAt` (`/api/users`: signed-in user shows a UTC timestamp,
never-signed-in user shows `null`). `/api/rejections` empty before any `/v1`
call, then populated newest-first with `invalid_api_key` (hint `y-99` from a
fake key; empty hint for a malformed `Authorization` header), `model_not_found`
with model name, `model_required`, and `model_access_denied` (valid key + model
route + no grant, 403) each carrying key hint and service-account id/name.
History directory untouched by all rejected requests (only the empty
`active.ndjson`). Concurrency: 160 bad-key `/v1/responses` posts at concurrency
24 → buffer holds exactly 100, valid JSON, strict newest-first ordering,
all hints ≤ 4 characters. Browser spot-check on a private instance (port 9964,
the migrated database): Users cards render "Last signed in Oct 9, 2026, 9:06:20
AM UTC" (admin) and "Never signed in" (olduser); the expanded account detail
renders labeled key rows as strong label + muted "Key ending <hint>" and
unlabeled rows unchanged; browser console clean. Deviations: the GET
`/api/rejections` response is a bare newest-first array (consistent with the
other management list endpoints rather than an envelope object), and the
login response body's `lastLoginAt` is the pre-login snapshot (see (b) above).

### Step 4 — Shell + Overview (2026-10-09)

Shell (`index.html` + `nav.js` + `site.css` + `app.js`): the eyebrow breadcrumb is
gone (A2) — the topbar is now h1 + section h2 only, and the `.eyebrow` CSS rules
were removed (zero occurrences remain in any HTML/CSS). Both shell status dots
are real signals now (A4): the topbar badge (`#local-status-dot` +
`#local-status-text`) and the sidebar footer (`#sidebar-status-dot` +
`#sidebar-status-text`) start neutral gray (`.status-dot.idle`) and are driven by
the most recent `/api/status` result inside `loadStatus` — green "Local
instance"/"Running locally" when ready, red (`.status-dot.down`) with "Local
instance · degraded"/"Degraded" on a degraded state, and red
"Instance unreachable"/"Unavailable" when the endpoint itself fails. The sidebar
footer is kept (it is hidden ≤760px where the topbar badge covers the need).
Choice documented: dots update only when `/api/status` is fetched (every
Overview visit and manual refresh); no other page polls status, so off-Overview
they show the last known state. The ≤760px `font-size: 0` hack is gone (A5) —
the badge text lives in `#local-status-text` and is visually hidden (clipped)
at narrow widths, keeping the accessible name while the badge collapses to a
small dot pill. A visually-hidden skip link (A6) is the first tab stop and
jumps to `<main id="main-content" tabindex="-1">`; its click is intercepted in
`app.js` because letting the browser follow `#main-content` would feed a
non-page hash to the SPA router (which would normalize to `#/overview` and drop
the current page).

Overview: a Setup section (`#overview-setup`, between the welcome row and the
Runtime card) answers "is anything wired up?" (B1). Counts come from
`/api/providers`, `/api/models`, and `/api/service-accounts` fetched in
parallel. The compact strip ("Providers 0 of 2 connected · Models 1 · Service
accounts 2") renders with `tag-danger` when 0 connected, `tag-success` when all
connected, and every fragment navigates. The three-step first-run checklist
(Connect a provider → Add a Model → Issue a key and grant a Model) carries live
checkmarks: step 2 flips on `models.length > 0`; step 3 is honestly derived by
fetching each account's `/keys` + `/grants` (the grants GET returns
`{"modelIds": [...]}`, which `accountIsWired` handles); step 1 on a connected
provider. Deviation (deliberate): while setup is incomplete the strip stays
visible *above* the checklist (the brief said "replacing/above"), so the danger
"0 of 2 connected" remains on screen during first-run; the checklist collapses
to the strip alone once ≥1 provider is connected and ≥1 Model exists. The four
history empty states are consolidated into one actionable empty state (B2) with
inline links to Providers and Models; the per-subsection empty rows (outcomes
inline, usage-groups row, performance's duplicate "No recent timing sample."
metric) no longer render when the base report has no groups. Status lines now
carry load feedback only (B4): success shows "Updated HH:MM:SS UTC"
(`updatedStamp()` in `format.js`), failure the explicit fetch error; the
explainer sentences moved into the panel subtitles. Refresh is unified (B3): a
single page-level Refresh button in the welcome row reloads status + setup +
usage + performance (the panel-level button was removed) — documented choice.
Decision 8: the usage/performance panels auto-refresh every 30 s via a module
timer started/stopped by `showPage`, gated on `document.hidden` and
`state.page`; a `visibilitychange` handler refetches immediately when the tab
becomes visible again; every tick goes through `beginHistoryFetch` (stale loads
aborted) so ticks cannot pile up; the subtitles state "Auto-refreshes every 30
s." The runtime "Request history" dot-chain is split into two labeled rows —
"Request history" and "In-flight requests" — with the lost/recovered and
written-bytes detail as muted sub-lines (B6). Outcomes render human labels with
per-row title definitions plus a collapsed "What the outcomes mean" legend, and
the Recent-performance Outcome cells use the shared `outcomeTag` with
failed/interrupted rows tinted (`.row-outcome-bad`, `tag-danger`) (B7/B8). The
usage-table first column header now names the active dimension ("Service
account", "Requested Model", "Actual provider", "Upstream model"; "Group" when
ungrouped), the Requested-Model option gained the suffix "(the alias agents
request)", and a note states Outcomes are always global (B5). All of
`outcomeLabel`/`outcomeDescription`/`outcomeTag`/`outcomeLegend` live in
`format.js` so step 9's Requests page can reuse them (`outcomeTag` already
flows to the Requests table).

Verification: `node --check` on all 13 modules OK (as `.mjs` copies); `go vet
./...` OK; `CGO_ENABLED=0 go build -trimpath` OK; `./scripts/build-release.sh`
builds all four targets; no Go files changed. Browser pass on a private
instance (port 9965, XDG-isolated data dir, seeded 2 disconnected providers, 2
service accounts, 1 user; then a Model + key + grant created via API):
strip shows "Providers 0 of 2 connected · Models 0 · Service accounts 2" with
the danger tag (computed rgb(156,65,65) on rgb(255,241,241)); the three
checklist steps render unchecked and their links navigate to
`#/providers`, `#/models`, `#/service-accounts`; the usage empty-state links
navigate too. After creating a Model the checklist's step 2 flips to done; after
issuing a key and a grant step 3 flips (initially unchecked until the
`modelIds` shape was handled — caught by this test). With `/api/providers`
stubbed to a connected provider, the checklist collapses to the strip-only
state with `tag-success` "1 of 1 connected" (rgb(40,117,82) on
rgb(233,247,239)); stubs removed afterwards. With `/api/requests?limit=100`
stubbed, failed and interrupted rows are tinted rgb(253,247,247) with
`tag-danger` outcome tags and definition tooltips, completed stays
`tag-success`; with `/api/usage` stubbed, the outcome list renders human labels
and the legend opens with all six definitions. Group-by renames the column
header to "Requested Model" and "Upstream model" as the dimension changes.
"Updated 09:26:58 UTC" stamps appear after loads. Auto-refresh: in-page fetch
instrumentation shows tick pairs exactly 29 999 ms apart (2 fetches per tick
with a group-by dimension selected); with `document.hidden` overridden true
(`Object.defineProperty`), a 35 s window produced zero usage fetches; a single
synthetic `visibilitychange` produced exactly one load (no feedback loop, no
pileup); navigating to Users for 35 s produced zero usage fetches (timer
stopped on page leave). Dots: both turn `.status-dot.down` rgb(185,83,83) with
"Instance unreachable"/"Unavailable" after the server is stopped and Refresh is
clicked, and recover to green after restart. Skip link: first Tab stop reveals
it (top −52px → 12px), Enter moves focus to `#main-content` with the hash
unchanged. Narrow windows 620px and 390px: no page overflow
(`scrollWidth == clientWidth`), strip and checklist stack cleanly, badge text
visually hidden (computed clip). Browser console buffer empty at end of
session. Deviations: strip-above-checklist as described; dots reflect the last
`/api/status` observation (no polling on other pages); the legend renders
wherever the outcome list renders (with zero history the consolidated empty
state takes that slot).

### Step 5 — Shared feedback/error/link utilities (2026-10-09)

Three new/extended shared modules plus the one allowed page-level application.

**`js/errors.js` (new, error humanization — decision 3).**
`humanizeApiError(status, body, context)` maps one failed management-API
response (body = parsed `{error: {message, code}}` envelope or null) to a
human sentence, keyed by status + error code, with
`context = {kind, name, action}` from the caller. Table contents:

- 409 `conflict` + `action: "create"`/`"rename"`: per-kind sentences —
  provider → `A provider named “X” already exists. Choose another name.`,
  service-account → same shape, Model → `A Model named “X” already exists.
  Choose another name.`, user → `Username “X” is already taken.`
  (unknown kind or empty name → null).
- 409 `conflict` + `action: "delete"`: `This <kind> is still in use. Remove
  the items that point to it first.` — deliberately generic because the
  server's `writeManagementFailure` collapses `ErrConflict`/`ErrReferenced`
  into one message and never says what references the resource (review P3/P7).
- 400 JSON-field leaks (message patterns, tolerant of a trailing period and
  the `metadata.` prefix used by the Pi endpoint): `providerId and
  upstreamModel are required` → `Choose a provider and an upstream model.`;
  `contextWindow|maxTokens must be a positive whole number|integer` →
  `Context window|Max tokens must be a positive whole number of tokens.`
- Everything else → `null`: callers keep the server message verbatim
  (explicit-errors invariant; no retry, no 5xx masking). Convenience wrapper
  `formErrorText(error, context)` returns the humanized sentence for known
  errors, otherwise `error.message` unchanged. To feed it, `api()` (api.js)
  now attaches `status` and `body` to the thrown Error (message unchanged),
  so every existing catch keeps working byte-identically.

**`js/feedback.js` (new — decision 4).** `withBusy(buttonEl, fn)` disables the
button and sets `aria-busy` for the duration of an async action, restoring
both in `finally` (double submit dies: the first handler disables the button
synchronously before its first await); `announceSuccess(messageEl, text)`
fills an existing form-message slot with the "success" kind
(`.form-message.success` already existed) and adds `aria-live="polite"` only
when the slot lacks one; `clearMessageOnInput(form, messageEl)` clears any
non-empty message on the form's `input`/`change` events (never writes);
`scrollCardIntoView(cardEl)` smooth-scrolls (instant under
prefers-reduced-motion) and applies a brief `.card-highlight` class
(restarted on repeat calls via reflow, removed after 1.5 s / 1.2 s reduced).

**`js/nav.js`: `pageLink(text, page, params)` (decision 5).** A real
`<button type="button">` (keyboard/AT semantics for free) styled as a text
link via a new `.link-button` CSS class (same visual convention as the
step-4 `.setup-link`, but `font: inherit` so it blends into surrounding
empty-state text), clicking `navigate(page, params)`. `navigate()` itself
was confirmed sufficient for in-page links (no-op when the hash already
matches, builds `#/page?query`).

**`site.css`.** `.card-highlight` (2 px accent box-shadow pulse via
keyframes) with a `@media (prefers-reduced-motion: reduce)` fallback that
disables the animation and shows a static outline for the same short window;
`.link-button`. CSS additions are required by the brief (highlight class +
link-styled builder consume them); no existing rules changed.

**Representative application (the only page-level change):** the provider
create form in `pages/providers.js` now goes through `withBusy` + a single
message slot cleared on input, success via `announceSuccess`, and errors via
`formErrorText(error, {kind: "provider", action: "create", name: <input>})`
so a duplicate-name 409 renders the human sentence. The rename form,
connect/disconnect/delete flows, and every other page module are untouched.

Verification: `node --check` on all changed modules OK (as `.mjs` copies);
`go vet ./...` OK; `CGO_ENABLED=0 go build -trimpath` OK;
`./scripts/build-release.sh` builds all four targets. Browser pass on a
private instance (port 9966, fresh XDG-isolated data dir, admin password
changed via the forced gate): creating provider "work-main" succeeds
("Provider added. Sign in when ready.") and a second submit shows
`A provider named “work-main” already exists. Choose another name.` (never
"resource conflicts…"); a rapid double-click on Add provider fired exactly
one POST `/api/providers` (request-log delta before=3 after=4) and the
button settles with `disabled=false`, `aria-busy` absent; typing in the
name field after the error (and after success) cleared the message on the
first keystroke; unknown-error passthrough — `network route` cannot set a
status code, so a CDP fetch wrapper stubbed POST `/api/providers` with a 500
and alien body `{"error":{"message":"Kx-99 void overflow: quux/flurb
disengaged","code":"weird_unknown_code"}}` — the message rendered verbatim
and the button restored (stub then disabled); user create with an existing
username still shows the raw `resource conflicts with existing
configuration` (untouched form, as expected until step 8);
`scrollCardIntoView` via dynamic `import()` on a provider card added
`card-highlight` with `animationName: card-highlight` and removed it after
1.7 s; with real CDP emulation (`set media light reduced-motion`) the same
call reports `animationName: none`, shows only the static
`rgba(56,103,220,0.25)` outline, and cleans up after 1.4 s;
`pageLink("Go to Models", "models")` renders `button.link-button` and
navigates to `#/models` on click, with params producing
`#/requests?outcome=failed` + correct title. Browser console clean across
the whole session. Deviations: CSS additions to `site.css` (outside the
stated `js/**` ownership, required by the brief); the 500-stub used a CDP
fetch wrapper instead of `network route` (which cannot set status codes);
the `contextWindow`/`maxTokens` leak patterns are covered in the table but
not yet wired to the Models page's client-side validator (later step).

### Step 6 — Providers page (2026-10-09)

All changes in `internal/httpui/assets/js/pages/providers.js` plus two
additions to `internal/httpui/assets/site.css`; no Go changes,
`index.html` untouched.

- **P2 (reveal, not reissue):** the Connect/Reconnect handler
  (providers.js:246) now returns early when the card already has a
  `.connect-panel`: it calls `scrollCardIntoView(card)` and issues
  nothing — no POST `/connect/start`, no panel replacement, so the live
  code and its countdown keep running. There is deliberately no reissue
  path from the button; the panel's own Close (providers.js:201) is the
  way out, after which Connect issues a fresh code. The old
  kill-and-reissue branch (`existing.connectStop?.(); existing?.remove()`)
  is gone. The issue path now goes through `withBusy` instead of manual
  disable/restore.
- **P1/P8:** the panel waiting line (providers.js:212) is now "Waiting
  for the sign-in to finish… If the script printed an error in your
  terminal, close this panel and run Connect again — nothing is saved
  until the flow completes." The old "on the other machine…" wording is
  gone (the instruction line above already says "machine with your web
  browser"), and the guidance's recovery instruction matches the new P2
  button behavior (Close, then Connect).
- **P10:** the panel instruction ends with "Requires python3."
  (providers.js:168). `finishExpired` (providers.js:139) now dims the
  code display (`.connect-code-expired`, site.css:130), sets
  `aria-disabled="true"`, and disables the Copy command button; the
  recovery message stays. Its text was adjusted to "Close this panel and
  run Connect again to issue a new code" so it matches the P2 rule that
  re-clicking an open (even expired) panel never reissues.
- **P4:** `finishWithStatus` (providers.js:111) now renders the
  replacement card into a variable and, besides the page-level
  `#provider-oauth-message` notice, writes the same outcome text (success
  or error kind) into the fresh card's own action message
  (`card.providerMessage`, set in `renderProvider`, providers.js:299) and
  calls `scrollCardIntoView(finished)`. Cards carry `data-provider-id`
  (providers.js:228); `findProviderCard` (providers.js:39) locates cards
  after re-renders. Panel survival of other cards is untouched (per-card
  swap, same `collectConnectPanels` snapshot path).
- **P6:** provider-create success (providers.js:365) now reads "Provider
  added. Open its card and run the enrollment script to connect — Models
  can target it once it shows Connected." and after the reload
  `scrollCardIntoView` runs on the new card (found via the POST response
  id). The step-5 wiring (withBusy, clearMessageOnInput, formErrorText)
  is unchanged.
- **P3:** the delete confirm (providers.js:339) is now
  `Delete provider "X"? Models targeting it must be removed first.` and
  the error path uses `formErrorText(error, {kind: "provider",
  action: "delete", …})`, so a 409 renders "This provider is still in
  use. Remove the items that point to it first." instead of the raw
  conflict text (the card lookup now uses `data-provider-id` instead of
  an h4 text match).
- **P7 + rename feedback:** the rename submit handler (providers.js:318)
  goes through `withBusy` and `formErrorText` ({kind: "provider",
  action: "rename"}), so a duplicate name renders `A provider named "X"
  already exists. Choose another name.`; success reloads the list and
  places `Name saved.` (announceSuccess) on the fresh card's message slot
  (providers.js:329). `clearMessageOnInput` is wired on the rename form.
- **P5:** `.connect-command` gains `white-space: pre-wrap` (site.css:131).
- **P9 — skipped, no client-side data:** `GET /api/providers` returns the
  `config.Provider` JSON (id/name/type/baseUrl/status/createdAt/updatedAt,
  internal/config/types.go:18) — no last-attempt timestamp or reason
  exists anywhere client-side (a failed connect is not even persisted
  server-side: `CompleteConnectFlow` returns "failed" without writing
  provider status). Surfacing a last attempt would require a backend
  change, which this step forbids. Not implemented.

Verification: `node --check` on all 13 modules OK (as `.mjs` copies);
`go vet ./...` OK; `CGO_ENABLED=0 go build -trimpath` OK (binary
`/tmp/opencode/step6-bin`). Browser pass on a private instance (port
9967, XDG-isolated data dir; seeded via curl: 2 providers + 1 service
account + 1 Model `gpt-x` targeting `work-main`; admin password changed
through the forced gate). Evidence: create "work-third" showed the new
next-step message and the new card carried `card-highlight` with
`animationName: card-highlight`; duplicate create still humanized; open
panel on `work-main`, click Connect again → same panel object, same code
`l-r-7CIQ…`, countdown still ticking (9:50 → 9:39), `card-highlight`
applied, and the `connect/start` request count stayed at 1 (network log:
6 start POSTs total, each accounted for by a distinct real open; the
re-clicks fired none); panel waiting line, python3 mention, and the
absence of "other machine" verified by reading the panel DOM; with
`/api/providers` stubbed to status changes (stub unrouted after each
check), `finishWithStatus` produced the page notice AND the card's own
message ("OpenAI account connected…" as `.form-message.success`;
"Connection attempt finished: Re-authentication required…" and "…
Connection failed…" as `.form-message.error`) with `card-highlight`
observed mid-animation on the finished card, while the other card's open
panel survived with the same code and a ticking countdown; expired panel
via a stubbed `connect/start` with `expiresInSeconds: 0` (unrouted
afterwards): countdown "Code expired", code got
`.connect-code-expired` (computed opacity 0.72) with
`aria-disabled="true"`, Copy disabled, recovery message present;
re-clicking Connect on the expired panel revealed the same dimmed panel
(no POST), Close then Connect issued a fresh real code (9:59); delete
confirm dialog text read back verbatim as `Delete provider "work-third"?
Models targeting it must be removed first.`; deleting `work-main`
(targeted by the Model) produced the role="alert" sentence "This provider
is still in use. Remove the items that point to it first."; deleting
`work-third` succeeded; rename to a fresh name showed "Name saved." on
the fresh card (`.form-message.success`) with the button restored, and a
duplicate rename showed the humanized already-exists sentence; 400 px
viewport with a panel open: `scrollWidth == clientWidth == 400`, command
`white-space: pre-wrap` wrapping at 280 px; panel survival across
navigation to Overview and back plus manual Refresh (same code, countdown
ticking); browser console clean across the whole session. API-level
failed-connect path re-verified with curl (start → claim on port 9767 →
complete with a bogus authorization code → `{"status":"failed"}`,
provider status stays `disconnected`), which is why the P4 browser check
stubs the poll response. Deviations: none beyond the P9 skip documented
above.

### Step 7 — Service accounts page (2026-10-09)

Changes in `internal/httpui/assets/js/pages/service-accounts.js`,
`js/nav.js`, `js/format.js`, `js/pages/models.js`, `assets/index.html`,
and `assets/site.css`; no Go changes.

- **S1 (grant/Models bridge).** The grants section's empty state
  (service-accounts.js:250-253) now reads "Add a Model before granting
  access. " followed by a `pageLink("Go to Models", "models")` button.
  Mirrored on the Models page: the Model form's "not automatically
  granted" note gains " Grant access on the Service accounts page.
  [Service accounts]" via a module-level append in models.js:233-239
  (index.html text unchanged; the link is a `pageLink` button because the
  CSP forbids inline handlers and the router is hash-based).
- **S3 (collapsed summary made stateful) — choice documented: the summary
  line, not the header readiness dot.** A header readiness color would
  require eagerly fetching keys+grants for every account card, which the
  brief forbids ("keep lazy loading"); the summary line gets its counts
  from the lazy detail load. `updateAccountSummary` (service-accounts.js:163)
  rewrites the collapsed `<summary>` to `Keys and Model access — <n>
  active key<s> · <g> of <k> Model<s> granted` after the lazy load
  (service-accounts.js:295), after key create/revoke (via the
  renderAccountDetails re-render), and after a grants save
  (service-accounts.js:284). Counts derive from non-revoked keys and
  `modelIds.length` against `state.models.length`; pluralization is
  grammatical ("1 of 1 Model granted" — deliberate deviation from the
  brief's literal `<k> Models` template for the 1-of-1 case). Before the
  first expand the summary stays the plain label (counts unknown without
  the lazy fetch).
- **S2 (Recent rejections panel).** New full-width panel in index.html:138-141
  ("Recent rejections", subtitle explaining pre-recording rejections,
  manual `#rejections-refresh` button). `loadRejections` (service-accounts.js:303)
  is fired detached from the accounts load on every page load/refresh
  (service-accounts.js:21) — lazy with the page, manual refresh only, no
  auto-polling. Rows are a `.data-table` (service-accounts.js:316):
  time via `formatUTC` (explicit UTC), key hint as "Key ending <hint>"
  or "Unknown key", code via the new `rejectionCodeLabel` (format.js:117-133,
  human labels for all eleven pre-admission codes the harness records;
  unknown codes pass through verbatim per the explicit-errors invariant,
  and known codes carry the raw enum as a title attribute), model name,
  and service-account name with muted id. Error state is a role="alert"
  message plus a Retry button (service-accounts.js:310-313). Empty state
  is exactly the brief's sentence.
- **S4 (key labels).** The key-issue heading gains a "Key label
  (optional)" input (maxlength 128, aria-labeled) beside the create
  button (service-accounts.js:174-180, `.key-issue-controls` layout in
  site.css:160-161). Creating POSTs `{label}` when non-empty and the
  byte-identical `{}` when empty (service-accounts.js:188); the input
  clears on success. Step 3's labeled-row rendering already displays it.
- **S5 (leave-guard) — scope: the revealed-key case only.** One
  choke point in nav.js `applyHash` (nav.js:48-76):
  `declinedSensitiveLeave` asks `window.confirm("A just-issued key that
  has not been copied will be lost. Leave anyway?")` when leaving
  Service accounts while `state.sensitiveCleanup` is non-empty — i.e.
  exactly while a show-once reveal is undismissed, because dismissing
  the key removes its cleanup from the set. Declining in-app reverts the
  hash via `location.replace(renderedHash)` (no history entry); the
  pre-existing `showPage` cleanup then handles accepted leaves. Reload/
  close is covered by a `beforeunload` handler in service-accounts.js:463-470
  consulting the same set, so both guards vanish after "Dismiss and
  clear". An intermediate implementation that checked in both `navigate()`
  and `renderRoute()` double-fired the confirm on the in-app path
  (caught in browser testing) — fixed by collapsing to the single
  `applyHash` choke point.
- **S6 (Rename placement).** Rename moved into the header `.card-actions`
  row as Rename · Disable · Delete (service-accounts.js:105-108); the
  stray `.edit-row` wrapper and both of its CSS rules are deleted
  (site.css former line 138 and the 420px media rule).
- **S7 (wording + revoked keys + confirms + feedback).** "Issue local
  keys for harnesses" → "Create API keys for your agents" (index.html:121);
  detail heading "Local API keys" → "API keys"; "Issue new key" →
  "Create key"; "No keys have been issued." → "No keys yet."; the account
  empty state and create success drop "harness"/"issue" ("...create a key
  when ready."). Revoked keys: verified there is no key-delete endpoint
  (management routes only expose `POST .../keys/{id}/revoke`), so per the
  brief they stay listed — visually muted via `.key-row-revoked`
  (opacity .6, muted strong, site.css:157-159) with no Remove affordance.
  Disable on an enabled account now confirms with
  `Disable “X”? Their keys stop working immediately. You can enable them
  again later.` (service-accounts.js:90; Enable stays instant). Key
  creation shows "Key created. Copy it from the highlighted box now — it
  is shown only once." and `scrollCardIntoView`s the account card
  (service-accounts.js:192-195). Create/rename errors go through
  `formErrorText` (create: service-accounts.js:199, rename :86, account
  create :487), so a duplicate account name renders the step-5 human
  sentence. Rename success survives the list re-render via a
  `pendingCardMessages` map consumed by `renderAccount`
  (service-accounts.js:14,81,122-125) and lands "Name saved." on the
  fresh card's message slot. Account-create scrolls the new card into
  view via its `data-account-id` (service-accounts.js:490).
- **Item 8 (feedback standard).** `withBusy` wraps create-key,
  disable/enable, rename, account create, and revoke buttons;
  `clearMessageOnInput` on the account create form (and the rename form);
  `announceSuccess` for all success messages.

Verification: `node --check` on the changed modules OK (nav.js,
format.js, pages/service-accounts.js, pages/models.js; full 13-module
.mjs sweep also clean); `go vet ./...` OK; `CGO_ENABLED=0 go build
-trimpath` OK; `./scripts/build-release.sh` builds all four targets.
Browser pass on a private instance (port 9968, XDG-isolated data dir,
seeded via API: 1 provider, 1 Model `gpt-x`, accounts "pi work" (labeled
key + grant) and "ci bot" (unlabeled key, no grant), plus real `/v1`
rejections triggered by curl). Evidence: rejections panel renders 3 rows
newest-first with "Invalid API key"/"Model access denied" labels, "Key
ending s-99"/"Key ending z9Lg"/"Unknown key", model "gpt-x", service
account "ci bot" + muted id, and "—" for fields the ring omits; fresh
instance (port 9969, second data dir) shows exactly the brief's empty
state; error state via an aborted `/api/rejections` route shows
"Could not load rejections: Failed to fetch" + Retry, and Retry after
unrouting restores the rows; pi work's collapsed summary reads "Keys and
Model access — 1 active key · 1 of 1 Model granted" (and "6 active keys"
after churn) without expanding; labeled key creation via the new input
("rotation test", "guard test") shows the label row, success message,
cleared input, and card-highlight; leave-guard: nav to Overview with a
revealed key fires exactly one confirm with the exact text, dismiss
stays on the page with the reveal intact, accept leaves to #/overview
with the cleanup applied ("The one-time key has been cleared…", secret
emptied), and after "Dismiss and clear" no dialog appears; with
`--no-auto-dialog`, reload with an active reveal raises a beforeunload
dialog and after dismissing the key reload proceeds dialog-free;
Rename · Disable · Delete in the header row (420px layout stacks them
below the title); rename shows "Name saved." on the fresh card and
round-trips the name; Disable confirm text read back verbatim,
Disable→"Disabled" badge→Enable→"Enabled" round trip; revoke puts the
row in `key-row-revoked` (computed opacity 0.6, "Revoked ·", no Revoke
button); deleting the Model flipped the grants section to "Add a Model
before granting access. Go to Models" whose link lands on #/models,
where the form note reads "…Grant access on the Service accounts page."
with a working link back; duplicate account create renders `A service
account named “dup” already exists. Choose another name.` and typing
clears it; Router regression after the applyHash rework: nav buttons,
Back (restores #/service-accounts + title), `#/bogus` normalizes to
`#/overview`, same-page click is a no-op, no spurious dialogs; 390px
viewport: `scrollWidth == clientWidth`, header actions stack, summary
visible collapsed (the rejections table keeps the established
`.table-scroll` horizontal-scroll container, min-width 640px); browser
console clean on both instances. Deviations: none — the only judgment
calls are the documented S3 choice (summary line over header dot, for
lazy loading), grammatical "1 of 1 Model granted", revoked keys staying
listed but muted (no server endpoint), and the S5 single-choke-point
rework after the double-confirm bug was caught in testing.

### Step 8 — Users page + login/change-password (2026-10-09)

Changes in `internal/httpui/assets/js/pages/users.js`, `js/nav.js`,
`js/state.js`, `assets/index.html`, `assets/site.css`, `assets/login.js`,
and `assets/change-password.html`; no Go changes.

**Users (U1–U6).**

- **U1 (temp-password handoff).** The create form's password field gained a
  show/hide toggle (index.html:248-251, `.password-field` layout in
  site.css:160-161): a real button wired in users.js:197-207 (no inline JS),
  flipping `input.type`, visible text Show/Hide, `aria-label`
  "Show/Hide password", and `aria-pressed`; focus returns to the input.
  On create success the password is captured **before** anything clears the
  field, stored in `state.userHandoffs` (a `Map` keyed by user id,
  state.js:14 — the documented choice: a page-local map like the
  service-accounts reveal state, so the block survives every list re-render,
  including manual Refresh, until dismissed; the entry is removed on dismiss,
  on that user's delete, and when the Users page is left via a cleanup in
  nav.js `showPage` (nav.js:95-98), mirroring the key-reveal `sensitiveCleanup`
  sweep), and the form then resets. The new card (cards now carry
  `data-user-id`) renders a one-time handoff block (`buildPasswordHandoff`,
  users.js:83-110) reusing the show-once visual pattern (`.key-reveal`/
  `.key-value`, role="alert" warning): "Temporary password for <username>:"
  + the code in a copyable `<code>` + Copy (clipboard with the standard
  fallback message) + "Hand this to the user; they must change it at first
  sign-in." + "Dismiss and clear", which empties the code and leaves the
  established `.key-cleared` "cleared from this page" line. Create success
  message: "User added. Hand over the temporary password shown on their
  card — they must change it at first sign-in." plus `scrollCardIntoView`.
- **U2 (reset feedback).** The reset form (users.js:126-166) now carries a
  full-width pre-submit hint "Signing them out everywhere; they must change
  it at next sign-in." (`.inline-hint`, site.css:164) and submits through
  `withBusy`. Success keeps the form open: the message ("Password set. All
  their sessions were signed out; they must change it at next sign-in.")
  rides a module-level `pendingResetMessages` map consumed once by
  `buildPasswordResetForm` while rendering the fresh card after `loadUsers()`
  (the re-render rebuilds every form, so the pending message both re-opens
  the form and announces via `announceSuccess`), the input is cleared, and
  the card is scrolled into view. `clearMessageOnInput` is wired on the
  reset form too (decision 4).
- **U5 (Disable confirm).** `setUserEnabled` (users.js:171-172) confirms with
  `Disable “X”? Their sessions are signed out immediately.` (curly quotes
  matching the existing delete confirm); Enable stays instant.
- **U3 (duplicate username).** The create handler (users.js:239-245) reports
  through `formErrorText(error, {kind: "user", action: "create", name})`, so
  a 409 renders `Username “X” is already taken.`; the username field gets
  `aria-invalid="true"` and focus, both cleared on the next input.
- **U6.** `clearMessageOnInput` on the create form (users.js:212); "Up to 64
  characters." hint under the username (index.html:246 with
  `aria-describedby`); the "You" card gains the muted line "You are signed
  in with this account." (users.js:69); "Password change pending" → "Must
  change password at next sign-in" (badge kept, users.js:48).
- **U4 polish.** Created + last-sign-in are one muted line:
  `Created <date> · Last signed in <UTC>` or `· Never signed in`
  (users.js:64-66).

**login/change-password (C1–C4).** Both standalone pages already use
`site.css` (so `.form-message.error` applies); `showMessage` in login.js now
takes a kind and sets the class (login.js:31-34).

- **C1:** the login failure path renders `Sign-in failed: <message>.` with
  the error styling — `failedSentence` (login.js:38) appends the period only
  when the server message lacks terminal punctuation, so the generic 401
  reads exactly "Sign-in failed: invalid credentials."
- **C2:** hint under Current password: "This is the temporary password you
  were given." (change-password.html:22, linked via `aria-describedby`); a
  401 `invalid_credentials` from `/api/auth/password` (login.js:100-102)
  renders "The current password is incorrect. Use the temporary password
  from your admin." instead of the server's generic message; other failures
  are wrapped as "Password change failed: <message>." Both error-styled.
- **C3:** "Wrong account? Sign out" below the form
  (change-password.html:31) POSTs `/api/auth/logout` through the page's
  existing fetch helper (which already sends the template-injected CSRF
  header) and lands on `/login` regardless of the call's outcome
  (login.js:109-118; the endpoint accepts stale sessions server-side).
- **C4:** the verb is "Set a new password" everywhere — `<title>`, h1, and
  submit button (change-password.html:7,16,28); the sessions note moved out
  of the heading subtitle into a muted line directly above the submit
  button (change-password.html:27).

Verification: `node --check` on users.js, nav.js, state.js, login.js OK (as
`.mjs` copies); `go vet ./...` OK; `CGO_ENABLED=0 go build -trimpath` OK
(binary `/tmp/opencode/step8-bin`); `./scripts/build-release.sh` builds all
four targets. Browser pass on a private instance (port 9970, XDG-isolated
data dir `/tmp/opencode/step8-data`, seeded via API: user "olduser"; admin
password changed through the forced gate). Evidence: toggle flips
password↔text with aria-label Show password→Hide password and
aria-pressed false→true and refocuses the input; create "uix-alpha" with
the toggle open → handoff block on the new card with the exact code
`temp-alpha-secret-1`, Copy showed "Copied to clipboard." (`writeText`
resolved; headless harness denies read-back, so content equality is by the
resolved-write path), success message on the create form, fields cleared
only after the block was stored; creating "uix-beta" re-rendered the list
and alpha's block survived with the same code; manual Refresh kept the
undismissed block; "Dismiss and clear" emptied the code to the
".key-cleared" line and a subsequent Refresh confirmed the dismissed block
stays gone while beta's survives; navigating Overview→Users cleared the
remaining handoff (0 `.key-reveal` after the round trip); duplicate create
"uix-beta" rendered `Username “uix-beta” is already taken.` with
`aria-invalid="true"`, focus on the username field, and both cleared on the
first input; empty password submit was natively blocked
(`validity.valueMissing`, no new card, no POST) and a seeded stale error
cleared on the first real keystroke; reset on uix-alpha showed the
pre-submit hint before any submit and after success kept the form open with
"Password set. All their sessions were signed out; they must change it at
next sign-in." (`.form-message.success`, aria-live polite, input cleared,
button restored, badge now "Must change password at next sign-in"); Disable
confirm read back verbatim `Disable “olduser”? Their sessions are signed
out immediately.`, dismiss cancelled (still Active), accept flipped to
Disabled, Enable round-tripped with no dialog; admin card shows
"Created … · Last signed in … UTC" plus "You are signed in with this
account." and no action buttons; other users show "· Never signed in";
delete confirm/flow unchanged and exercised. Second browser session:
login as uix-alpha (reset password) → forced `/change-password` with title
"Set a new password · Gyemoim", h1/button "Set a new password", subtitle
without the sessions sentence, the temp-password hint under Current
password, the sessions note as a muted line above the submit; wrong current
password rendered "The current password is incorrect. Use the temporary
password from your admin." in `rgb(164, 63, 63)`; Sign out landed on
`/login`; a full successful change as uix-beta landed on `#/overview`;
login failure rendered "Sign-in failed: invalid credentials." in
`.form-message.error`; 390px viewport on Users (screenshot) and /login:
`scrollWidth == clientWidth`, password field + Hide toggle fit on one row,
"Up to 64 characters." hint visible; browser console and page-error buffers
empty on both sessions at end. Deviations: `state.userHandoffs` + nav.js
cleanup (documented above) instead of reusing `state.sensitiveCleanup`, so
the users handoff deliberately gets no beforeunload/in-app leave confirm
(not in the brief, and the S5 guard text is key-specific); the reset-form
toggle from the review's U1 prose was not added — the brief's scope asks
for the toggle on the create form only; one throwaway user ("fresh-name")
was created by a mistyped test step and deleted again via the UI.

### Step 9 — Requests page (2026-10-09)

Changes in `internal/httpui/assets/js/pages/requests.js`, `js/pages/overview.js`,
`js/nav.js`, `assets/index.html`, and `assets/site.css`; no Go changes.

**R1 — accurate split validation, exact backend parity (requests.js:60-133).**
Decision documented: the client stays as strict as the backend. Reading
`internal/httpapi/history.go` `parseHistoryTime` (:277) shows Go
`time.Parse(time.RFC3339Nano, …)` — since Go 1.20 a strict RFC3339 parse with a
general-layout fallback that requires the same literals — so the backend accepts
exactly `YYYY-MM-DDTHH:mm:ss[.fraction]` + `Z` / `+00:00` / `-00:00` (any nonzero
offset is rejected after parsing), over a valid calendar date, with mandatory
seconds and an uppercase `T`; it does **not** tolerate space separators,
lowercase `t`/`z`, date-only values, or missing seconds (contrary to the step
brief's guess). The client accepts exactly that set (strict regex + component
validation incl. leap years; accepted-but-unusual values such as `+00:00` and
9-digit fractions are sent verbatim, never rewritten) and splits failures into
accurate messages: (a) date-only / missing time → "Add a time — use
YYYY-MM-DDTHH:mm:ssZ (UTC)."; (b) everything structurally invalid (impossible
calendar values, missing seconds, garbage) → "That isn't a valid date and
time."; (c) missing zone or nonzero offset → "End with Z for UTC — e.g.
2026-10-09T14:30:00Z."; plus one refinement for the space-separator case (L3):
(d) "Use T between the date and time — e.g. 2026-10-09T14:30:00Z." The
from-earlier-than-to check keeps its label-based message. **Input choice
(R2):** option (b) — text inputs kept, with concrete example hints under each
field (index.html:191-192, `aria-describedby`) and example placeholders;
`datetime-local` was rejected because its picker shows locale formatting with
no timezone, needs seconds handling, and would still require full custom
validation of partial states, while presets + hints remove most typing. On a
backend 400, `fetchRequestPage` (requests.js:332) shows the server message as a
FILTERS-panel error (never "Request history is unavailable."), grays the stale
results, and sets the summary to "Filters were not applied — the results below
are from the last successful query."; `historyFilterServerMessage`
(requests.js:316) translates raw param names to labels ("from must be a UTC
RFC3339 timestamp" → "Started from must be a UTC RFC3339 timestamp"; unknown
server messages pass through verbatim per the explicit-errors invariant).

**R1/R2 — presets:** three buttons ("Last hour", "Last 24 hours", "Today
(UTC)", index.html:190) fill both fields with correctly generated UTC strings
(second precision — `toISOString().slice(0,19) + "Z"`; Today uses UTC midnight
→ now; requests.js:222). Fill-only; Apply remains the explicit submit.

**R3 — identity warnings + datalist affordance (requests.js:137-201).** On
Apply, each identity field whose value matches no configured entity (checked
against `state.accounts/models/providers` and configured upstream models)
shows a non-blocking amber warning under the field — "No configured service
account matches '<value>' — searching historical IDs anyway." — and the query
still fires. Refinement beyond the brief: a value matching a configured
entity's **name only** (e.g. "pi work") gets its own warning naming the ID
("'pi work' is the name of the configured service account whose ID is … —
filters match IDs exactly; searching historical IDs anyway."), because the
backend compares IDs exactly and a typed name would otherwise dead-end
silently — R3's "typed name" case. Datalist affordance: all four identity
placeholders now say "type to see suggestions" (index.html:193-196), and an
empty datalist says so in a field hint ("No Models configured yet.", "No
service accounts are configured yet.", "No providers are configured yet.",
"No upstream models are configured yet.", requests.js:166). Warnings clear on
input (decision 4).

**R4 — feedback.** New `#request-filter-message` slot inside the filter form
(index.html:199, `role="status" aria-live="polite", spans the grid row) holds
all filter validation errors; offending from/to fields get
`aria-invalid="true"`. Stale results **gray out** (documented choice over
clearing: `.stale-results` opacity .55 on the table body, site.css:296) and
stay grayed until the next successful fetch. Empty states: no filters + empty
archive → "No recorded requests yet. Requests appear here once the gateway
handles traffic." in the message slot with no table row; a filtered empty
result → "No requests match these filters." in the table cell with the message
slot cleared — exactly one instance per page either way
(requests.js:341-344). Summary line: "Page N · up to 25 requests per page ·
pages load on demand, so no total is shown." (requests.js:403; the "total
archive count is not scanned" jargon is gone). Refresh re-fetches
`state.appliedRequestFilters` — draft edits are never promoted
(requests.js:281-307; the old draft recompute is gone).

**R4/R5 — discoverability.** Table rows are clickable (`clickable-row`,
cursor pointer; row click ignores button targets) and open the detail; the
explicit Inspect button stays under a new "Actions" column header
(index.html:205). Filter inputs 10px → 13px (site.css:290). UTC display: the
table's Started column shows `formatUTC` with the local rendering as a title
tooltip (requests.js:391-393); the detail panel shows both (`formatUTC … ·
local …`, requests.js:482) and the detail subtitle is UTC.

**M5 — Overview cross-links (overview.js:229-236, 259-273, 300-308).** Each
Outcomes row is now a real `<button class="outcome-row">` (hover style in
site.css:297-298) navigating to `#/requests?outcome=<value>`; usage-breakdown
rows are clickable and their label cell is an accessible `pageLink` button
with the dimension's param. Requests consumes the params on render
(`deepLinkFilters`, requests.js:242): **param names are `outcome`,
`account_id`, `model_id`, `provider_id`, `upstream_model`** — matching the
`/api/requests` query params — alongside the existing `select`. Applying
filters syncs the hash through the new `nav.js` `replaceHashParams`
(nav.js:146): `history.replaceState` + `renderedHash` update, so the URL
always matches the applied filters (shareable deep links) without adding a
history entry or re-rendering (verified: Back after Apply skips straight past
the replaced entry).

**L3/L5.** Space separators: rejected with message (d), matching the backend.
The 100-page cursor stack cap now sets a title tooltip on Next page when the
stack first shifts ("Only the 100 most recent pages stay reachable with
Previous page.", requests.js:423) — code-reviewed only; driving 100 page
transitions in the stub harness was not practical.

Behavior note: a fresh page render (any navigation into Requests) re-syncs the
filter form from `state.appliedRequestFilters`; the manual Refresh button
passes no params and keeps draft edits visible in the form while still
fetching with the applied filters.

Verification: `node --check` on all 14 modules OK (as `.mjs` copies); `go vet
./...` OK; `CGO_ENABLED=0 go build -trimpath` OK (binary
`/tmp/opencode/step9-bin`); `./scripts/build-release.sh` builds all four
targets. Browser pass on a private instance (port 9971, XDG-isolated data dir
`/tmp/opencode/step9-data`, seeded via API: provider "work-main", service
account "pi work" with key+grant, Model `gpt-x` → `gpt-5.3`; admin password
changed through the forced gate; history empty — `/api/requests*` stubbed via
agent-browser network routes for data states, unrouted after each). Evidence:
presets produce correct UTC strings verified against `Date.now` (Last hour
from −60 min / to −1 s; Last 24 hours −24 h; Today = UTC midnight → now, all
`…Z` second-precision); the three brief messages plus the separator message
each rendered in the FILTERS panel with `form-message error`,
`aria-invalid="true"` on the offending field only, stale graying applied, and
**zero** `/api/requests` fetches across all six bad-input cases; parity sweep:
`+00:00`, `-00:00`, 1- and 9-digit fractions, and a leap-day date are accepted
(fire queries), while `2021-02-29`, second `:60`, lowercase `t`, missing
seconds, `2026-13-01`, hour `24`, and bare dates are rejected with the right
message — client and backend accept the same set; messages/aria-invalid clear
on first input; backend-400 path (fetch-wrapper stub returning the real
backend message) → "Started from must be a UTC RFC3339 timestamp" in the
FILTERS panel, summary NOT flipped to "unavailable", stale graying, clean
recovery after unstubbing; identity warnings for unknown account/model values
(fire anyway), no warning for exact IDs, name-only match shows the ID-naming
warning, clear on input; empty datalist hints ("No Models configured yet." /
"No upstream models are configured yet.") with `/api/models` stubbed to [];
no-filter empty state sentence appears once (zero table rows) and the
filtered empty cell appears once with an empty message slot; stubbed results
page: Actions header, UTC Started ("Oct 9, 2026, 11:45:00 AM UTC") with local
tooltip, Failed `tag-danger` tag with definition tooltip, row click on a plain
cell opens the detail panel (selected-row highlight), Inspect still works;
`#/requests?outcome=failed` fresh load pre-applies the select and fetches
`?outcome=failed`; the Overview Failed outcome button and a stubbed
usage-breakdown row (Group by = Service account) land on
`#/requests?outcome=failed` / `#/requests?account_id=<uuid>` with the field
prefilled and the fetch carrying the param; Refresh with unapplied draft edits
(outcome=completed + from=2020) fetched exactly `/api/requests?limit=25` (no
draft params) and kept the draft in the form; Apply synced the hash
(`#/requests?account_id=…`) with no history entry (Back → previous page); 13px
computed on filter inputs; 390px viewport `scrollWidth == clientWidth` with
presets/hints stacking (screenshot); all seven pages render with correct
titles after the nav.js change; `?select=` deep link still auto-opens the
detail (UTC subtitle); browser console and page-error buffers empty at end of
session. Deviations: CSS additions to site.css (required by the brief's
13px/row/stale/preset items); the extra space-separator message and the
name-only identity warning (documented above); stale results gray out rather
than clear (documented choice); hash sync on Apply (documented above); the
cursor-cap tooltip has no browser proof (100-page drive impractical).
