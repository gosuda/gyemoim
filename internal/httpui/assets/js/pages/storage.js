// Storage page: history storage tiles/details, the date-range deletion form
// with its polling status renderer.
import { api, beginHistoryFetch, historyFetchIsCurrent } from "../api.js";
import { state } from "../state.js";
import { byId, element, showMessage } from "../dom.js";
import { formatBytes, formatNumber, formatUTC } from "../format.js";

export async function loadStorage() {
  const token = beginHistoryFetch();
  const message = byId("storage-message");
  const summary = byId("storage-summary");
  const details = byId("storage-details");
  showMessage(message, "Loading history storage…");
  try {
    const status = await api("/api/storage", { signal: token.controller.signal });
    if (!historyFetchIsCurrent(token) || state.page !== "storage") return;
    const metrics = [
      ["Active history", formatBytes(status.activeBytes), `${formatNumber(status.activeSegments)} active file`],
      ["Closed raw history", formatBytes(status.rawBytes), `${formatNumber(status.rawSegments)} segments`],
      ["Compressed history", formatBytes(status.compressedBytes), `${formatNumber(status.compressedSegments)} segments`],
      ["Pending compression", formatBytes(status.pendingBytes), `${formatNumber(status.pendingSegments)} raw segments`],
    ];
    summary.replaceChildren(...metrics.map(([label, value, note]) => {
      const metric = element("div", "usage-metric");
      metric.append(element("strong", "", value), element("span", "", `${label} · ${note}`));
      return metric;
    }));
    const deletion = status.deletion || { state: "idle" };
    renderDeletionStatus(deletion);
    const deletionRange = deletion.from && deletion.to ? `${formatUTC(deletion.from)} – ${formatUTC(deletion.to)} (end exclusive)` : "—";
    const fields = [
      ["History recording", status.recorderState],
      ["Potentially lost records", formatNumber(status.potentiallyLostRecords)],
      ["Active requests", formatNumber(status.activeRequests)],
      ["Compression", status.compressionState],
      ["zstd executable", status.compressionAvailable ? "Available" : "Unavailable"],
      ["Failed segments", formatNumber(status.failedSegments)],
      ["Compressed validation", status.compressedValidationPending ? "Pending or unavailable" : "Complete"],
      ["Date deletion", deletion.state || "idle"],
      ["Deletion range (UTC)", deletionRange],
      ["Deletion progress", `${formatNumber(deletion.completedSegments || 0)} / ${formatNumber(deletion.totalSegments || 0)} segments`],
      ["Deletion error", deletion.error || "None"],
      ["Last compression error", status.lastCompressionError || "None"],
      ["Affected segment", status.lastCompressionSegment || "—"],
      ["Last attempt", status.lastCompressionAt ? new Date(status.lastCompressionAt).toLocaleString() : "—"],
    ];
    details.replaceChildren(...fields.map(([name, value]) => {
      const row = element("div");
      row.append(element("dt", "", name), element("dd", "", value));
      return row;
    }));
    if (deletion.state === "pending_recovery") {
      showMessage(message, deletion.error || "History deletion needs recovery. New history queries are unavailable until it completes.", "error");
    } else {
      showMessage(message, "Closed raw segments are replaced by compressed segments after successful publication. History has no automatic expiration.");
    }
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "storage") return;
    summary.replaceChildren();
    details.replaceChildren();
    showMessage(message, `Storage status could not be loaded: ${error.message}`, "error");
  }
}

export function renderDeletionStatus(deletion = {}) {
  const status = byId("history-delete-status");
  if (!status) return;
  const range = deletion.from && deletion.to ? ` for ${formatUTC(deletion.from)} through ${formatUTC(deletion.to)} (end exclusive)` : "";
  if (deletion.state === "running") {
    showMessage(status, `Deletion is running${range}. ${formatNumber(deletion.completedSegments || 0)} of ${formatNumber(deletion.totalSegments || 0)} closed segments are complete. New history queries receive 503 while maintenance is active.`);
  } else if (deletion.state === "pending_recovery") {
    showMessage(status, deletion.error || `Deletion recovery is pending${range}. New history queries remain unavailable.`, "error");
  } else if (deletion.state === "completed") {
    showMessage(status, `Last deletion completed${range}. ${formatNumber(deletion.completedSegments || 0)} segments processed.`, "success");
  } else {
    showMessage(status, "No date-range deletion is running or pending.");
  }
}

byId("history-delete-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const firstDate = form.elements.firstDate.value;
  const lastDate = form.elements.lastDate.value;
  const message = byId("history-delete-message");
  if (!firstDate || !lastDate || lastDate < firstDate) {
    showMessage(message, "Choose a valid inclusive UTC date range.", "error");
    return;
  }
  const confirmed = window.confirm(`Permanently delete all request history started from ${firstDate} through ${lastDate} UTC for every ServiceAccount and Model?`);
  if (!confirmed) return;
  const submit = form.querySelector('[type="submit"]');
  submit.disabled = true;
  showMessage(message, "Preparing history deletion…");
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
    result = await api("/api/storage/delete", {
      method: "POST",
      body: JSON.stringify({ firstDate, lastDate }),
    });
  } catch (error) {
    operationError = error;
  } finally {
    clearInterval(poll);
    pollController.abort();
    submit.disabled = false;
  }
  if (operationError) {
    showMessage(message, operationError.message, "error");
  } else {
    showMessage(message, `Deleted ${formatNumber(result.recordsRemoved || 0)} history records in ${formatNumber(result.segmentsProcessed || 0)} segments for [${formatUTC(result.from)}, ${formatUTC(result.to)}).`, "success");
  }
  if (state.page === "storage") await loadStorage();
});
