# Web UI usability review (2026-10-09)

Findings from a usability-focused review of the management WebUI. Method: a seeded
live instance (two disconnected providers, two service accounts, three users, empty
history) exercised by seven independent reviewers — one per page plus the app shell —
with real browser interaction (create/error/expire flows, narrow viewports at
380–760 px), each cross-checked against `internal/httpui/assets/{index.html,site.js,site.css,login.html,login.js,change-password.html}`
and, where behavior was surprising, the relevant Go handlers. Function names below are
anchors into that code. This document is the findings record; `ui-overhaul.md` is the
plan that consumes it.

## Cross-cutting patterns

Three problems repeat on every page and drive the plan's priorities:

1. **Next steps are prose, never links.** The setup chain (connect a Provider → create
   a Model → grant it to a Service account → issue a key) is taught one hop per page,
   always as a sentence with no clickable path. Empty states dead-end ("Add a Model
   before granting access." — site.js:856).
2. **Success has no feedback; failure speaks machine.** Password reset success just
   deletes the form; Rename is silent; but errors surface as `resource conflicts with
   existing configuration` and `providerId and upstreamModel are required`.
3. **"Is anything actually working?" is invisible.** The landing page shows a green
   "Ready" badge with both providers disconnected; a Model card pointing at a dead
   provider is pixel-identical to a healthy one; an account with a key but no grants
   looks enabled-and-fine; and rejected `/v1/` calls (401/403) are never recorded, so
   the UI cannot distinguish a bad key from a missing grant.

**Keep-list (verified good; an overhaul must not regress these):** keyboard/ARIA
discipline (`:focus-visible` outlines, `aria-live` status regions, `aria-labelledby`
sections, nav as real buttons with `aria-current`); confirmation dialogs that state
precise consequences; the show-once key reveal pattern; explicit server errors with
retry buttons and no auto-retry; bounded-query honesty; enrollment-panel survival
across refresh/navigation with correct countdowns; the responsive base (grid collapse
at 1050/760 px, no horizontal overflow at 380 px on most pages).

## App shell + Overview + login/change-password

- **A1 (high) No URL routing.** `navigate()` (site.js:89) never changes `location.href`;
  reload loses the page, Back leaves the app, nothing is bookmarkable, and a 24 h
  session always restarts at Overview. Minimal fix: hash routing + `hashchange`.
- **A2 (med) Title stacking.** Eyebrow "GYEMOIM / USERS" + h1 + section h2 (+h3) repeat
  the same word up to four times (Users). The eyebrow is `titles[page].toUpperCase()`.
- **A3 (low) `document.title` never changes** between pages.
- **A4 (med) Decorative green dots** (topbar "Local instance", sidebar "Running
  locally") are hard-coded green (site.css:38), never driven by `/api/status`, and say
  the same thing twice.
- **A5 (med) Narrow window:** `.local-badge { font-size: 0 }` (site.css:173) renders a
  mysterious 35 px green dot at ≤760 px.
- **A6 (low)** No skip link; 8+ tab stops before content.
- **A7 (low-med)** Session expiry redirects to /login silently; the thrown "Your
  management session has expired" message is never rendered; half-filled work is lost
  without comment.
- **B1 (high) The landing page never answers "is anything wired up?"** No provider/
  model/account counts anywhere; `refreshPage('overview')` loads only status+history.
  "Ready" (`#status-label`) means runtime state, not usability.
- **B2 (med) Empty states are dead ends** — four phrasings of "no history" with no next
  step; "Timing sample: 0 most recent requests, capped at 100" is confusing.
- **B3 (med) Manual refresh only, zero staleness signaling.** One Refresh button also
  secretly reloads the status panel; Recent performance has none; no "updated at".
- **B4 (med) Status lines used as permanent jargon explainers** ("unknown token counts
  remain separate from zero"), occupying the slot where load feedback should go.
- **B5 (med) Group-by select**: Requested Model vs Upstream model vs Actual provider is
  exactly the alias concept newcomers lack; "Group" column header doesn't change;
  Outcomes never re-groups with no note.
- **B6 (med)** Runtime "Request history" value is one unparsed dot-chain mixing five
  facts ("ready · 0 potentially lost · 0 recovered bytes · 0 active · 0 active bytes").
- **B7 (low)** Outcome enums render raw lowercase (`interrupted`/`cancelled`/
  `incomplete`) with no definitions.
- **B8 (low)** `failed` rows don't stand out (tag-danger exists but unused here).
- **C1 (med) Login/change-password errors render in neutral gray** — login.js
  `showMessage()` never applies the existing `.form-message.error` class.
- **C2 (med)** On forced change, "Current password" (the temp password) is unexplained;
  wrong-temp-password has no recovery hint.
- **C3 (med)** `/change-password` is a trap with no exit — no logout link anywhere.
- **C4 (low)** Heading/verb inconsistency ("Change password" title, "Set a new password"
  h1); "Other signed-in sessions are signed out." reads like a threat before anything
  happened.
- **C5 (low)** Login otherwise correct: disabled submit + "Signing in…", preserved
  credentials on failure.

## Providers

- **P1 (high) "Waiting…" is a dead end when the script fails in the terminal.** The
  poll (site.js:364) checks only provider status; a terminal-side script error leaves
  the UI waiting ~10 minutes. Add guidance to the waiting line; (bigger) expose
  claim/complete states so "script never reached us" vs "sign-in in progress" differ.
- **P2 (med) Re-clicking "Connect with enrollment script" silently kills a live code**
  (site.js:463 issues a new code; the UI knows the old one is dead, the user doesn't).
- **P3 (med) Delete confirm lacks consequences** ("Delete provider "work-main"?") and a
  referenced delete surfaces the generic 409 "resource conflicts with existing
  configuration" (management.go:731) — the user can't tell what references it.
- **P4 (med) Connect outcome notice lands off-screen** (`#provider-oauth-message` sits
  above the card grid; the panel is at the bottom of the list).
- **P5 (med) 400 px overflow:** `.connect-command` is `white-space: pre` (site.css:108)
  and forces the page to ~507 px; `pre-wrap` fixes it.
- **P6 (med)** "Provider added. Sign in when ready." doesn't name the next step and the
  new card appears at the list end unannounced; message persists indefinitely.
- **P7 (med)** Duplicate-name error shows the raw backend conflict text.
- **P8 (low)** "on the other machine" contradicts the "machine with your web browser"
  instruction one line up.
- **P9 (low)** A repeatedly-failing provider shows no last-attempt time or reason.
- **P10 (low)** Expired panel keeps an active Copy command for a dead code; Rename
  success is silent; python3 requirement never mentioned in the panel.
- **Keep:** panel survival (`collectConnectPanels` + per-card swap, site.js:265–347),
  countdown/expiry recovery, copy/download affordances with clipboard fallback, the
  five status states' one-line actions, UI command ↔ script agreement (verified against
  `gyemoim-connect.py --help`).

## Service accounts

- **S1 (high) Grant section dead-ends with no Models** and nothing bridges the two
  halves of the key+grant invariant; sidebar order (Service accounts before Models)
  contradicts the real setup order.
- **S2 (high) Rejected harness calls are invisible.** Pre-admission rejections
  (harness.go:69 authenticate, model_access_denied/model_not_found) run before
  `recorder.Begin` (harness.go:179) — the Requests page shows nothing. No way to
  distinguish bad key / missing grant / typo'd model.
- **S3 (med)** Collapsed "Keys and Model access" summary is a static string; an account
  with 0 keys or 0 grants looks identical to a wired-up one.
- **S4 (med) Keys cannot be named**; after rotation the list shows only last-4 hints.
- **S5 (med) Show-once key silently vanishes on navigation** (`state.sensitiveCleanup`,
  site.js:1000) — issued key lost without warning if the admin double-checks Models.
- **S6 (med)** Rename floats alone below-right (`.edit-row`, site.css:115) while
  Disable/Delete sit together.
- **S7 (low)** "Local API keys", "harness", "Issue" jargon; revoked keys are permanent
  clutter; Disable is instant and unconfirmed; post-create success message never
  clears and the new card is off-viewport; "Pi agent setup" panel is noise for non-Pi
  admins.
- **Keep:** the show-once reveal, consequence-precise confirms, grant checkboxes with
  provider·upstream sub-labels and explicit save, narrow-window behavior, lazy detail
  load with retry.

## Models

- **M1 (high) "Load models" is a silent disabled button** for disconnected providers
  (site.js:1041); the exactly-right error message in `loadProviderModelCatalog` is
  unreachable dead code for this case.
- **M2 (high) Routing to a dead provider is invisible** — creation succeeds with no
  warning; the card shows no provider status (data already in `state.providers`).
- **M3 (med) Five model-words on one screen**: Model / upstream model / model ID /
  account model / "Model details are not inferred" (which means Pi metadata).
- **M4 (med) "Optional Pi model metadata"** — Pi is never explained on this page; no
  when-to-use guidance.
- **M5 (med)** Chain guidance names steps but links nowhere; "who can use this model?"
  has no answer on the page.
- **M6 (low-med)** Server errors leak JSON field names ("providerId and upstreamModel
  are required"); catalog message is 9 px (site.css:146); no test-call button, no
  per-model usage/grant visibility.
- **Keep:** provider status embedded in dropdown options; honest invariant messaging
  ("not automatically granted", "one fixed provider and upstream target", delete
  confirm); edit/delete mechanics with prefill; responsive 3→2→1 column form.

## Requests

- **R1 (high) RFC 3339 text inputs fail while mis-teaching the format.** `2026-10-09Z`
  passes the client (site.js:1518) and 400s on the backend (history.go:281); the
  message blames the missing Z when the hour is invalid; non-UTC offsets get the
  suffix message; on a 400 the summary flips to "Request history is unavailable."
- **R2 (high) UTC-vs-local trap:** the table shows `date.toLocaleString()` (local) while
  filters demand hand-typed UTC; `datetime-local` exists and is unused; no presets.
- **R3 (high) Identity fields dead-end silently** on any unrecognized ID or a typed
  name ("No requests match these filters."); datalist suggestions exist and work
  (`populateRequestFilterOptions`, "name · UUID" labels) but nothing signals them, and
  the model datalists are empty on a fresh instance with zero affordance change.
- **R4 (med)** Empty state says "No requests match these filters." twice with zero
  filters; "total archive count is not scanned" is insider jargon; Refresh secretly
  re-submits draft filter edits (site.js:1558/2096); validation errors render in the
  results panel far from the offending field with stale results still visible; the
  only route in is Overview Recent performance's per-row View (capped at 100).
- **R5 (low)** Filter inputs at 10 px (site.css:244); unnamed action column; row not
  clickable; space-separator timestamps pass client validation; cursor stack caps at
  100 pages silently.
- **Keep:** datalist mechanism; bounded-query semantics (25/page, explicit timeouts,
  retry buttons); the Overview→Requests deep-select mechanism (`pendingRequestID`);
  responsive filter grid; the detail panel's information architecture (summary grid,
  safe-error block, cache-ratio failure reasons, timing timeline with offset checks,
  chunked body viewers); Reset behavior.

## Storage

- **T1 (high) Failure states have no visual weight.** All 14 status rows render
  identically (site.js:147–167, site.css:64) — `zstd executable: Unavailable`, failed
  segments, and deletion errors would be pixel-identical to the healthy state; six
  rows are pure idle noise.
- **T2 (high) Destructive flow confirms without scale** — no record/segment preview
  before `window.confirm`; `recordsRemoved` is learned only after the fact.
- **T3 (med) Local-vs-UTC trap on the date pickers** (browser locale `mm/dd/yyyy`,
  semantics UTC inclusive); future-range rejection discoverable only by failing.
- **T4 (med) Deletion feedback duplicated and stale** — `#history-delete-message`
  survives input changes (no listeners on the date inputs); idle state shows a
  permanent un-actionable line; form not disabled while a deletion runs.
- **T5 (med) The four tiles look like a disk partition but pending ⊆ raw** (storage.go
  211–218) — apparent double counting; "file" vs "segments" vocabulary mixed;
  "segment" never defined; 9 px captions.
- **T6 (med) "Potentially lost records: 0"** is alarming wording for the healthy case.
- **T7 (low)** Debug jargon ("Affected segment", "Last attempt", "Compressed
  validation"); `#storage-message` permanently repurposed for an essay; Refresh scope.
- **Keep:** the destructive gating chain (required inputs → client range check before
  any network call → confirm naming scope/dates → explicit 409), honest invariant
  text ("Failed compression keeps the raw source", "History has no automatic
  expiration"), live deletion progress with the 503 explanation, formatBytes/formatUTC.

## Users

- **U1 (high) Temp-password handoff is broken** — `type="password"` with no reveal, and
  `form.reset()` runs before the success message (site.js:2152), so the one value that
  must be handed to the new user is unrecoverable. The reset form has the same shape.
- **U2 (high) Successful reset gives zero feedback** — success path only calls
  `loadUsers()` (site.js:737); the form vanishes; "sessions revoked + forced change"
  exist only in a source comment.
- **U3 (med)** Duplicate-username error is the generic conflict string; no field-level
  invalid marking.
- **U4 (med)** No last login / session visibility — before Disable/Delete the admin
  cannot see whether the person is active (API returns no such data).
- **U5 (med)** Disable is instant and unconfirmed though it revokes sessions
  irreversibly; Delete confirms — the asymmetry exposes the milder action.
- **U6 (low)** Stale error survives a natively blocked submit; "Password change
  pending" doesn't say what to do; 64-char maxlength is silent; the "You" card's
  missing actions are correct but unexplained.
- **Keep:** the plain-statement all-admins model ("Every signed-in user has full
  management rights."); delete confirm wording; the create flow's first-login contract
  (stated twice, plus from the recipient side on /change-password); accessible reset
  form micro-interactions; no password-policy leakage.

## Backend-flagged candidates (not UI-only)

1. **Auth-rejection ring buffer** (last N pre-admission rejections: time, key hint,
   code, requested model) + API to read it — fixes S2 without touching history
   invariants.
2. **`lastLoginAt`** on users (store + API) — fixes U4 minimally.
3. **Optional key label** (schema addition + API field) — fixes S4.
4. **Deletion dry-run/preview** — fixes T2 properly (flagged, deferred).
5. Deferred/no action: manual "compress now", oldest-record date, key last-used
   timestamps, per-user session listing, roles.
