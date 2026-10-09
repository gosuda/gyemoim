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
