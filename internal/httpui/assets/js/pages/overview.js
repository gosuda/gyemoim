// Overview page: runtime status, all-history usage/outcomes, and the recent
// performance table.
import { addQuery, api, beginHistoryFetch, historyFetchIsCurrent } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formatDate, formatDurationNS, formatNumber, formatOffsetNS, historyErrorMessage, identityCell } from "../format.js";
import { navigate } from "../nav.js";

export async function loadStatus() {
  const details = byId("runtime-details");
  const label = byId("status-label");
  const error = byId("status-error");
  error.hidden = true;
  try {
    const status = await api("/api/status");
    const fields = [
      ["State", status.state],
      ["Listener", `127.0.0.1:${status.port}`],
      ["Data directory", status.dataDirectory],
      ["SQLite", status.sqliteState],
      ["Request history", `${status.historyState} · ${status.historyPotentiallyLostRecords} potentially lost · ${status.historyRecoveredBytes} recovered bytes · ${status.historyActiveRequests} active · ${status.historyBytesWritten} active bytes`],
      ["Started", new Date(status.startedAt).toLocaleString()],
      ["Runtime", status.goVersion],
      ["Platform", `${status.operatingSystem} / ${status.architecture}`],
    ];
    details.replaceChildren(...fields.map(([name, value]) => {
      const row = element("div");
      row.append(element("dt", "", name), element("dd", "", value));
      return row;
    }));
    label.textContent = status.state === "ready" ? "Ready" : status.state;
    label.parentElement.classList.toggle("degraded", status.state !== "ready");
  } catch (errorValue) {
    label.textContent = "Unavailable";
    label.parentElement.classList.add("degraded");
    details.replaceChildren(element("p", "muted", "Runtime status could not be loaded."));
    error.textContent = errorValue.message;
    error.hidden = false;
  }
}

function usageCountCell(count) {
  const cell = element("td");
  const total = count?.total;
  const known = count?.knownRequests ?? 0;
  const unknown = count?.unknownRequests ?? 0;
  cell.append(element("strong", "", `${formatNumber(total)} known total`), element("small", "", `Known in ${formatNumber(known)} · unknown in ${formatNumber(unknown)}`));
  return cell;
}

function countMetric(name, count) {
  const metric = element("div", "usage-metric");
  metric.append(element("strong", "", formatNumber(count?.total)), element("span", "", `${name} · known in ${formatNumber(count?.knownRequests ?? 0)} requests · unknown in ${formatNumber(count?.unknownRequests ?? 0)}`));
  return metric;
}

function groupLabel(group, dimension) {
  if (!dimension) return "All history";
  if (dimension === "account") return `${group.accountName || "Historical account"} · ${group.accountId || "ID unavailable"}`;
  if (dimension === "model") return `${group.modelName || "Historical Model"} · ${group.modelId || "ID unavailable"}`;
  if (dimension === "provider") return `${group.providerName || "Historical provider"} · ${group.providerId || "ID unavailable"}`;
  return group.upstreamModel || "Unknown upstream model";
}

function cacheRatioLabel(group) {
  if (typeof group.cacheRatio === "number" && Number.isFinite(group.cacheRatio)) return `${(group.cacheRatio * 100).toFixed(1)}%`;
  const reasons = {
    input_or_cached_count_unknown: "input or cached count unknown",
    input_tokens_zero: "input total is zero",
    cached_tokens_exceed_input: "cached input exceeds input",
  };
  return `Unavailable · ${reasons[group.cacheRatioUnavailableReason] || "counts inconsistent or unavailable"}`;
}

function renderUsageGroups(report, dimension) {
  const body = byId("overview-usage-groups");
  body.replaceChildren();
  if (!report?.groups?.length) {
    const row = element("tr");
    const cell = element("td", "empty-cell", "No matching history is available.");
    cell.colSpan = 7;
    row.append(cell);
    body.append(row);
    return;
  }
  for (const group of report.groups) {
    const row = element("tr");
    const label = element("td");
    label.append(element("strong", "", groupLabel(group, dimension)));
    row.append(label, element("td", "", formatNumber(group.clientRequests)), usageCountCell(group.inputTokens), usageCountCell(group.outputTokens), usageCountCell(group.cachedInputTokens), usageCountCell(group.reasoningOutputTokens), element("td", "", cacheRatioLabel(group)));
    body.append(row);
  }
}

function renderOverviewUsage(baseReport, breakdownReport, dimension) {
  const summary = byId("overview-history-summary");
  const group = baseReport?.groups?.[0];
  summary.replaceChildren();
  if (!group) {
    summary.append(element("div", "empty-state", "No request history is available yet."));
    byId("overview-outcomes").replaceChildren(element("p", "empty-inline", "No request outcomes are available."));
    renderUsageGroups(breakdownReport, dimension);
    return;
  }
  const outcomes = group.outcomes || {};
  const totalRequests = Number(group.clientRequests) || 0;
  summary.append(
    countMetric("Client requests · all readable history", { total: totalRequests, knownRequests: totalRequests, unknownRequests: 0 }),
    countMetric("Input tokens", group.inputTokens),
    countMetric("Output tokens", group.outputTokens),
    countMetric("Cached input tokens · subset", group.cachedInputTokens),
    countMetric("Reasoning output tokens · subset", group.reasoningOutputTokens),
  );
  const outcomeList = element("div", "outcome-list");
  for (const name of ["active", "interrupted", "completed", "failed", "cancelled", "incomplete"]) {
    const count = Number(outcomes[name]) || 0;
    const rate = totalRequests ? `${((count / totalRequests) * 100).toFixed(1)}%` : "—";
    const row = element("div", "outcome-row");
    row.append(element("span", "", name), element("strong", "", `${formatNumber(count)} · ${rate} of ${formatNumber(totalRequests)} client requests`));
    outcomeList.append(row);
  }
  byId("overview-outcomes").replaceChildren(outcomeList);
  renderUsageGroups(breakdownReport, dimension);
}

function renderOverviewPerformance(page) {
  const rows = byId("overview-recent-requests");
  const summary = byId("overview-performance-summary");
  rows.replaceChildren();
  summary.replaceChildren();
  const requests = page?.requests || [];
  if (!requests.length) {
    const row = element("tr");
    const cell = element("td", "empty-cell", "No recent requests are available.");
    cell.colSpan = 7;
    row.append(cell);
    summary.append(element("div", "usage-metric", "No recent timing sample."));
    return;
  }
  const durations = requests.map((request) => Number(request.durationNs)).filter((value) => Number.isFinite(value) && value >= 0);
  const firstOutputs = requests.map((request) => request.timings?.first_output_ns).filter((value) => value !== null && value !== undefined && Number.isFinite(Number(value)) && Number(value) >= 0).map(Number);
  const mean = (values) => values.length ? values.reduce((sum, value) => sum + value, 0) / values.length : null;
  const durationMetric = element("div", "usage-metric");
  durationMetric.append(element("strong", "", mean(durations) === null ? "Unknown" : formatDurationNS(mean(durations))), element("span", "", `Average recorded duration · ${durations.length} of ${requests.length} recent requests`));
  const outputMetric = element("div", "usage-metric");
  outputMetric.append(element("strong", "", mean(firstOutputs) === null ? "Unknown" : formatDurationNS(mean(firstOutputs))), element("span", "", `Average first-output offset · ${firstOutputs.length} of ${requests.length} recent requests`));
  summary.append(durationMetric, outputMetric);
  for (const request of requests) {
    const row = element("tr");
    const account = request.serviceAccount || {};
    const model = request.model || {};
    const firstOutput = request.timings?.first_output_ns;
    const inspect = button("Inspect", "quiet small", () => {
      navigate("requests", { select: request.requestId });
    });
    const action = element("td");
    action.append(inspect);
    row.append(element("td", "", formatDate(request.startedAt)), identityCell(account.name, account.id), identityCell(model.name, model.id), element("td", "", request.outcome || "unknown"), element("td", "", formatDurationNS(request.durationNs)), element("td", "", formatOffsetNS(firstOutput)), action);
    rows.append(row);
  }
}

export async function loadOverviewHistory() {
  const token = beginHistoryFetch();
  const historyMessage = byId("overview-history-message");
  const performanceMessage = byId("overview-performance-message");
  showMessage(historyMessage, "Loading usage and outcomes…");
  showMessage(performanceMessage, "Loading the most recent 100 requests…");
  const dimension = byId("overview-group-by").value;
  const basePath = "/api/usage";
  const breakdownPath = dimension ? addQuery("/api/usage", { group_by: dimension }) : basePath;
  try {
    const [baseReport, breakdownReport, recentPage] = await Promise.all([
      api(basePath, { signal: token.controller.signal }),
      dimension ? api(breakdownPath, { signal: token.controller.signal }) : Promise.resolve(null),
      api("/api/requests?limit=100", { signal: token.controller.signal }),
    ]);
    if (!historyFetchIsCurrent(token) || state.page !== "overview") return;
    renderOverviewUsage(baseReport, breakdownReport || baseReport, dimension);
    renderOverviewPerformance(recentPage);
    showMessage(historyMessage, "Usage and outcomes cover all readable history; unknown token counts remain separate from zero.");
    showMessage(performanceMessage, `Timing sample: ${recentPage.requests.length} most recent requests, capped at 100.`);
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "overview") return;
    const message = historyErrorMessage(error);
    showMessage(historyMessage, `Could not load all-history usage: ${message}`, "error");
    showMessage(performanceMessage, `Could not load recent performance: ${message}`, "error");
  }
}

byId("overview-group-by").addEventListener("change", () => {
  if (state.page === "overview") loadOverviewHistory();
});
