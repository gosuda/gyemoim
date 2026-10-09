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
