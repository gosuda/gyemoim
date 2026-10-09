// Storage page: the health banner, the three storage tiles, the collapsed
// compression debug detail, and the date-range deletion form with its
// polling status renderer. The destructive gating chain (required inputs →
// client range check before any network call → confirm naming scope/dates →
// explicit 409 handling) is kept from the pre-overhaul page.
import { api, beginHistoryFetch, historyFetchIsCurrent } from "../api.js";
import { state } from "../state.js";
import { byId, element, showMessage } from "../dom.js";
import { withBusy, clearMessageOnInput } from "../feedback.js";
import { formatBytes, formatNumber, formatUTC, updatedStamp } from "../format.js";

// T6: "Potentially lost records" is the recorder's unsynced-buffer counter —
// at risk only if the process crashes. The neutral name and the hint travel
// together everywhere the fact is shown.
const LOST_RECORDS_NAME = "Records not yet flushed to disk";
const LOST_RECORDS_HINT = "At risk only if the process crashes; zero is healthy.";

// Count + noun with the singular/plural handled (T5): "0 segments",
// "1 segment", "2 segments".
function countNoun(count, singular) {
  return `${formatNumber(count)} ${Number(count) === 1 ? singular : `${singular}s`}`;
}

function todayUTC() {
  return new Date().toISOString().slice(0, 10);
}

// Failure facts for the health banner (T1). Everything that should turn the
// banner red: zstd unavailable, failed segments, unflushed records,
// compression or deletion errors, and deletion recovery pending.
function healthFailureFacts(status) {
  const facts = [];
  if (!status.compressionAvailable) facts.push("zstd unavailable");
  if (Number(status.failedSegments) > 0) facts.push(`${countNoun(status.failedSegments, "failed segment")}`);
  if (Number(status.potentiallyLostRecords) > 0) facts.push(`${countNoun(status.potentiallyLostRecords, "record")} not yet flushed to disk`);
  if (status.lastCompressionError) facts.push("compression reported an error");
  const deletion = status.deletion || {};
  if (deletion.state === "pending_recovery") facts.push("deletion recovery is pending");
  else if (deletion.error) facts.push("deletion reported an error");
  return facts;
}

function renderHealthBanner(status) {
  const banner = byId("storage-health");
  if (!banner) return;
  const failures = healthFailureFacts(status);
  banner.title = failures.some((fact) => fact.includes("not yet flushed"))
    ? LOST_RECORDS_HINT
    : "";
  if (failures.length) {
    banner.classList.add("unhealthy");
    banner.textContent = `Storage degraded · ${failures.join(" · ")}`;
    return;
  }
  banner.classList.remove("unhealthy");
  banner.textContent = `Recording healthy · compression ${status.compressionState} · zstd available · ${countNoun(status.failedSegments, "failed segment")}`;
}

// T5: three tiles (Active / Closed raw / Compressed); pending compression is
// folded into the raw tile's caption because pending segments ⊆ raw segments.
function renderTiles(status) {
  const summary = byId("storage-summary");
  const metrics = [
    ["Active history", formatBytes(status.activeBytes), countNoun(status.activeSegments, "active segment")],
    ["Closed raw history", formatBytes(status.rawBytes), `${countNoun(status.rawSegments, "segment")} · ${countNoun(status.pendingSegments, "segment")} awaiting compression`],
    ["Compressed history", formatBytes(status.compressedBytes), countNoun(status.compressedSegments, "segment")],
  ];
  summary.replaceChildren(...metrics.map(([label, value, note]) => {
    const metric = element("div", "usage-metric");
    metric.append(element("strong", "", value), element("span", "", `${label} · ${note}`));
    return metric;
  }));
}

// The two always-relevant rows that are neither banner facts nor debug
// detail (T6: the lost-records row keeps the neutral name and goes red only
// when the count is above zero).
function renderStatusRows(status) {
  const details = byId("storage-details");
  const lost = element("dd");
  lost.append(element("span", Number(status.potentiallyLostRecords) > 0 ? "health-bad" : "", formatNumber(status.potentiallyLostRecords)));
  const lostRow = element("div");
  lostRow.title = LOST_RECORDS_HINT;
  lostRow.append(element("dt", "", LOST_RECORDS_NAME), lost);
  details.replaceChildren(
    (() => {
      const row = element("div");
      row.append(element("dt", "", "Active requests"), element("dd", "", formatNumber(status.activeRequests)));
      return row;
    })(),
    lostRow,
  );
}

// T1/T7: the low-frequency compression rows live in the collapsed disclosure
// with their debug-jargon names replaced.
function renderDebugDetails(status) {
  const details = byId("storage-debug-details");
  if (!details) return;
  const validation = element("dd", "", status.compressedValidationPending ? "Pending or unavailable" : "Complete");
  validation.title = "Closed compressed files are re-read to verify integrity.";
  const rows = [
    ["Last compression segment", status.lastCompressionSegment || "—"],
    ["Last compression attempt", status.lastCompressionAt ? formatUTC(status.lastCompressionAt) : "—"],
    ["Last compression error", status.lastCompressionError || "None"],
  ];
  details.replaceChildren(...rows.map(([name, value]) => {
    const row = element("div");
    row.append(element("dt", "", name), element("dd", "", value));
    return row;
  }));
  const validationRow = element("div");
  validationRow.append(element("dt", "", "Compressed validation"), validation);
  details.append(validationRow);
}

// T1: the deletion state rows moved into the deletion panel; the whole
// detail block is idle-only and hidden while no deletion information exists.
function renderDeletionDetails(deletion) {
  const details = byId("history-delete-details");
  if (!details) return;
  if (!deletion.state || deletion.state === "idle") {
    details.hidden = true;
    details.replaceChildren();
    return;
  }
  const range = deletion.from && deletion.to
    ? `${formatUTC(deletion.from)} – ${formatUTC(deletion.to)} (end exclusive)`
    : "—";
  const rows = [
    ["Deletion state", deletion.state],
    ["Deletion range (UTC)", range],
    ["Deletion progress", `${formatNumber(deletion.completedSegments || 0)} / ${formatNumber(deletion.totalSegments || 0)} segments`],
    ["Deletion error", deletion.error || "None"],
  ];
  details.replaceChildren(...rows.map(([name, value]) => {
    const row = element("div");
    row.append(element("dt", "", name), element("dd", "", value));
    return row;
  }));
  details.hidden = false;
}

export async function loadStorage() {
  const token = beginHistoryFetch();
  const message = byId("storage-message");
  const summary = byId("storage-summary");
  showMessage(message, "Loading history storage…");
  // T3: the date pickers can never select a future UTC date; the cap is
  // refreshed on every render because "today" moves.
  for (const input of ["history-delete-first", "history-delete-last"]) {
    byId(input).max = todayUTC();
  }
  try {
    const status = await api("/api/storage", { signal: token.controller.signal });
    if (!historyFetchIsCurrent(token) || state.page !== "storage") return;
    renderHealthBanner(status);
    renderTiles(status);
    renderStatusRows(status);
    renderDebugDetails(status);
    renderDeletionStatus(status.deletion || { state: "idle" });
    // T7: the status line carries load feedback only; the explainer prose
    // moved to the page intro. pending_recovery keeps an explicit error.
    if ((status.deletion || {}).state === "pending_recovery") {
      showMessage(message, status.deletion.error || "History deletion needs recovery. New history queries are unavailable until it completes.", "error");
    } else {
      showMessage(message, updatedStamp());
    }
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "storage") return;
    summary.replaceChildren();
    renderStatusRows({ activeRequests: 0, potentiallyLostRecords: 0 });
    renderHealthBanner({ compressionAvailable: true, compressionState: "unknown", failedSegments: 0, potentiallyLostRecords: 0 });
    renderDeletionDetails({ state: "idle" });
    showMessage(message, `Storage status could not be loaded: ${error.message}`, "error");
  }
}

// T4: the idle line is only meaningful once a deletion has completed in this
// session (or the backend still reports a completed deletion); a fresh page
// shows nothing instead of a permanent un-actionable sentence.
let deletionCompletedSeen = false;

// T4: the whole form (date inputs + submit button) is disabled while a
// deletion is running or needs recovery.
function setDeletionFormDisabled(disabled) {
  const form = byId("history-delete-form");
  if (!form) return;
  for (const input of form.elements) input.disabled = disabled;
}

export function renderDeletionStatus(deletion = {}) {
  const status = byId("history-delete-status");
  if (!status) return;
  if (deletion.state === "completed") deletionCompletedSeen = true;
  setDeletionFormDisabled(deletion.state === "running" || deletion.state === "pending_recovery");
  renderDeletionDetails(deletion);
  const range = deletion.from && deletion.to ? ` for ${formatUTC(deletion.from)} through ${formatUTC(deletion.to)} (end exclusive)` : "";
  if (deletion.state === "running") {
    showMessage(status, `Deletion is running${range}. ${formatNumber(deletion.completedSegments || 0)} of ${formatNumber(deletion.totalSegments || 0)} closed segments are complete. New history queries receive 503 while maintenance is active.`);
  } else if (deletion.state === "pending_recovery") {
    showMessage(status, deletion.error || `Deletion recovery is pending${range}. New history queries remain unavailable.`, "error");
  } else if (deletion.state === "completed") {
    showMessage(status, `Last deletion completed${range}. ${formatNumber(deletion.completedSegments || 0)} segments processed.`, "success");
  } else if (deletionCompletedSeen) {
    showMessage(status, "No date-range deletion is running or pending.");
  } else {
    showMessage(status);
  }
}

const deleteForm = byId("history-delete-form");
const deleteMessage = byId("history-delete-message");
const deletePreview = byId("history-delete-preview");

// T3: the live range preview restates the effective UTC range as the dates
// change, including the client-side range check that runs before any
// network call.
function updateDeletePreview() {
  const first = deleteForm.elements.firstDate.value;
  const last = deleteForm.elements.lastDate.value;
  if (!first || !last) showMessage(deletePreview, "Choose a first and last date.");
  else if (last < first) showMessage(deletePreview, "The last date is before the first date; no range is valid.");
  else showMessage(deletePreview, `Will delete records started ${first} 00:00 UTC – ${last} 23:59:59 UTC.`);
}

deleteForm.addEventListener("input", updateDeletePreview);
clearMessageOnInput(deleteForm, deleteMessage);

deleteForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const firstDate = deleteForm.elements.firstDate.value;
  const lastDate = deleteForm.elements.lastDate.value;
  // Client range check before any network call (kept invariant).
  if (!firstDate || !lastDate || lastDate < firstDate) {
    showMessage(deleteMessage, "Choose a valid inclusive UTC date range.", "error");
    return;
  }
  // T2: the confirm restates the effective range including the
  // submission-time cap and names the blast radius.
  const confirmed = window.confirm(`Delete history records started from ${firstDate} 00:00 UTC through ${lastDate} end-of-day UTC (effectively capped at submission time)? This permanently deletes matching history for all service accounts and Models.`);
  if (!confirmed) return;
  const pollController = new AbortController();
  let pollInFlight = false;
  const poll = setInterval(async () => {
    if (pollInFlight || state.page !== "storage" || pollController.signal.aborted) return;
    pollInFlight = true;
    try {
      const status = await api("/api/storage", { signal: pollController.signal });
      if (!pollController.signal.aborted && state.page === "storage") renderDeletionStatus(status.deletion);
    } catch { /* The final operation response reports errors. */ }
    finally { pollInFlight = false; }
  }, 1500);
  let result = null;
  let operationError = null;
  try {
    result = await withBusy(deleteForm.querySelector('[type="submit"]'), () => api("/api/storage/delete", {
      method: "POST",
      body: JSON.stringify({ firstDate, lastDate }),
    }));
  } catch (error) {
    operationError = error;
  } finally {
    clearInterval(poll);
    pollController.abort();
  }
  // The POST resolves only after the operation itself finishes, so the form
  // is usable again either way; a still-running backend state re-disables it
  // through renderDeletionStatus below.
  setDeletionFormDisabled(false);
  if (operationError) {
    if (operationError.status === 409) {
      showMessage(deleteMessage, `${operationError.message} No files were changed.`, "error");
    } else {
      showMessage(deleteMessage, operationError.message, "error");
    }
  } else {
    deletionCompletedSeen = true;
    // The POST result reports totals rather than progress counters; map them
    // so the details block reads "N / N segments processed" until the next
    // status fetch restates the completed deletion.
    renderDeletionStatus({
      state: "completed",
      from: result.from,
      to: result.to,
      completedSegments: result.segmentsProcessed || 0,
      totalSegments: result.segmentsProcessed || 0,
    });
    showMessage(deleteMessage, `Deleted ${formatNumber(result.recordsRemoved || 0)} history records in ${formatNumber(result.segmentsProcessed || 0)} segments for [${formatUTC(result.from)}, ${formatUTC(result.to)}).`, "success");
  }
  if (state.page === "storage") await loadStorage();
});
