// Overview page: runtime status, the setup health strip/checklist, all-history
// usage/outcomes, and the recent performance table. The usage/performance
// panels auto-refresh every 30 s while the tab is visible (decision 8).
import { addQuery, api, beginHistoryFetch, historyFetchIsCurrent } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formatBytes, formatDate, formatDurationNS, formatNumber, formatOffsetNS, historyErrorMessage, identityCell, outcomeDescription, outcomeLabel, outcomeLegend, outcomeTag, updatedStamp } from "../format.js";
import { navigate } from "../nav.js";

// The shell's two status dots (topbar badge and sidebar footer) mirror the
// most recent /api/status result (review A4): green when ready, red when the
// state is degraded, red with "unreachable" text when the endpoint itself
// fails. They stay neutral gray until the first Overview load reports.
function updateInstanceDots(mode) {
  const labels = {
    ok: ["Local instance", "Running locally"],
    degraded: ["Local instance · degraded", "Degraded"],
    unreachable: ["Instance unreachable", "Unavailable"],
  }[mode] || ["Local instance", "Running locally"];
  for (const dotID of ["local-status-dot", "sidebar-status-dot"]) {
    const dot = byId(dotID);
    dot.classList.toggle("down", mode !== "ok");
    dot.classList.remove("idle");
  }
  byId("local-status-text").textContent = labels[0];
  byId("sidebar-status-text").textContent = labels[1];
}

export async function loadStatus() {
  const details = byId("runtime-details");
  const label = byId("status-label");
  const error = byId("status-error");
  error.hidden = true;
  try {
    const status = await api("/api/status");
    const row = (name, value, sub) => {
      const item = element("div");
      const dd = element("dd");
      dd.append(element("span", "", value));
      if (sub) dd.append(element("span", "runtime-sub", sub));
      item.append(element("dt", "", name), dd);
      return item;
    };
    details.replaceChildren(
      row("State", status.state),
      row("Listener", `127.0.0.1:${status.port}`),
      row("Data directory", status.dataDirectory),
      row("SQLite", status.sqliteState),
      // Review B6: the old single "Request history" value was one unparsed
      // dot-chain of five facts; it is now two labeled rows with the
      // lost/recovered detail as a muted sub-line.
      row("Request history", status.historyState, `${formatNumber(status.historyPotentiallyLostRecords)} potentially lost · ${formatBytes(status.historyRecoveredBytes)} recovered`),
      row("In-flight requests", `${formatNumber(status.historyActiveRequests)} active`, `${formatBytes(status.historyBytesWritten)} written`),
      row("Started", new Date(status.startedAt).toLocaleString()),
      row("Runtime", status.goVersion),
      row("Platform", `${status.operatingSystem} / ${status.architecture}`),
    );
    label.textContent = status.state === "ready" ? "Ready" : status.state;
    label.parentElement.classList.toggle("degraded", status.state !== "ready");
    updateInstanceDots(status.state === "ready" ? "ok" : "degraded");
  } catch (errorValue) {
    label.textContent = "Unavailable";
    label.parentElement.classList.add("degraded");
    details.replaceChildren(element("p", "muted", "Runtime status could not be loaded."));
    error.textContent = errorValue.message;
    error.hidden = false;
    updateInstanceDots("unreachable");
  }
}

// Setup health (review B1): answer "is anything wired up?" on the landing
// page. Counts come from the existing list APIs, fetched in parallel. While
// no provider is connected or no Model exists, the section shows a three-step
// first-run checklist with live checkmarks; once setup is complete it
// collapses to the compact strip.
export async function loadSetupHealth() {
  const panel = byId("overview-setup");
  const subtitle = byId("overview-setup-subtitle");
  const body = byId("overview-setup-body");
  panel.hidden = false;
  subtitle.textContent = "Checking what is wired up…";
  body.replaceChildren(element("p", "muted", "Checking setup health…"));
  let providers;
  let models;
  let accounts;
  try {
    [providers, models, accounts] = await Promise.all([
      api("/api/providers"),
      api("/api/models"),
      api("/api/service-accounts"),
    ]);
  } catch (error) {
    if (state.page !== "overview") return;
    subtitle.textContent = "Setup health is unavailable.";
    body.replaceChildren(element("p", "muted", `Setup health could not be loaded: ${error.message}`));
    return;
  }
  if (state.page !== "overview") return;
  const connected = providers.filter((provider) => provider.status === "connected").length;
  const complete = connected > 0 && models.length > 0;
  // Step 3 ("Issue a key and grant a Model") can only be marked done honestly
  // by checking per-account keys and grants; the list endpoints carry no counts.
  let keyed = false;
  if (!complete && accounts.length > 0) {
    const wired = await Promise.all(accounts.map(accountIsWired));
    if (state.page !== "overview") return;
    keyed = wired.some(Boolean);
  }
  // While setup is incomplete the strip (the counts, including the danger
  // "0 of N connected") stays visible above the checklist; once complete the
  // checklist collapses away and only the strip remains.
  subtitle.textContent = complete
    ? "Current setup at a glance; each count opens its page."
    : "Complete these steps to start serving requests.";
  const stack = element("div", "setup-stack");
  stack.append(setupStripElement(providers, models, accounts, connected));
  if (!complete) stack.append(setupChecklistElement(providers, models, accounts, connected, keyed));
  body.replaceChildren(stack);
}

function accountIsWired(account) {
  return Promise.all([
    api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`),
    api(`/api/service-accounts/${encodeURIComponent(account.id)}/grants`),
  ]).then(([keys, grants]) => {
    const keyCount = Array.isArray(keys) ? keys.length : 0;
    // GET /grants answers {"modelIds": [...]} (the PUT body shape).
    const grantIds = Array.isArray(grants) ? grants : grants?.modelIds;
    return keyCount > 0 && Array.isArray(grantIds) && grantIds.length > 0;
  }).catch(() => false);
}

function setupChecklistElement(providers, models, accounts, connected, keyed) {
  const checklist = element("ol", "setup-checklist");
  checklist.append(
    setupStep(1, connected > 0, "Connect a provider",
      connected > 0 ? `${connected} of ${providers.length} connected.` : "No provider is connected yet.",
      "Sign in with ChatGPT so the gateway can reach upstream inference.",
      "providers", "Go to Providers"),
    setupStep(2, models.length > 0, "Add a Model",
      models.length > 0 ? `${formatNumber(models.length)} configured.` : "No Models are configured yet.",
      "Name the alias agents will request and point it at one provider and upstream model.",
      "models", "Go to Models"),
    setupStep(3, keyed, "Issue a key and grant a Model",
      keyed ? "At least one account has a key and a Model grant." : accounts.length ? "No account has both a key and a Model grant yet." : "No service accounts yet.",
      "Create a service-account key and grant it the Model agents should call.",
      "service-accounts", "Go to Service accounts"),
  );
  return checklist;
}

function setupStep(number, done, title, statusText, description, page, linkLabel) {
  const item = element("li", done ? "setup-step done" : "setup-step");
  const mark = element("span", done ? "setup-mark done" : "setup-mark", done ? "✓" : String(number));
  mark.setAttribute("aria-hidden", "true");
  const body = element("div", "setup-step-body");
  body.append(
    element("h3", "", title),
    element("p", "muted", `${statusText} ${description}`),
    setupLink(linkLabel, page),
  );
  item.append(mark, body);
  return item;
}

function setupStripElement(providers, models, accounts, connected) {
  const strip = element("div", "setup-strip");
  const total = providers.length;
  const connectedClass = total > 0 && connected === total ? "tag tag-success" : total > 0 && connected === 0 ? "tag tag-danger" : "tag";
  strip.append(
    setupStripItem("providers", "Providers", element("span", connectedClass, `${connected} of ${total} connected`)),
    setupStripItem("models", "Models", element("span", "tag", formatNumber(models.length))),
    setupStripItem("service-accounts", "Service accounts", element("span", "tag", formatNumber(accounts.length))),
  );
  return strip;
}

function setupStripItem(page, label, tagNode) {
  const item = element("button", "setup-strip-item");
  item.type = "button";
  item.append(element("span", "", label), tagNode);
  item.addEventListener("click", () => navigate(page));
  return item;
}

function setupLink(label, page) {
  const link = element("button", "setup-link", label);
  link.type = "button";
  link.addEventListener("click", () => navigate(page));
  return link;
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

function renderUsageGroups(report, dimension, hideEmpty = false) {
  const body = byId("overview-usage-groups");
  // Review B5: the first column header names the active dimension instead of
  // the generic "Group".
  const dimensionNames = { account: "Service account", model: "Requested Model", provider: "Actual provider", upstream_model: "Upstream model" };
  byId("overview-usage-group-column").textContent = dimensionNames[dimension] || "Group";
  body.replaceChildren();
  if (!report?.groups?.length) {
    // With no base-report groups the consolidated empty state above already
    // explains the situation, so the per-subsection empty rows are hidden
    // (review B2). A non-empty base with an empty breakdown still gets the
    // explicit "no matching history" row.
    if (hideEmpty) return;
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
    // Review B2: the four per-section "no history" phrasings collapse into one
    // actionable empty state with links into the setup chain.
    const empty = element("div", "empty-state");
    empty.append("No requests recorded yet. Connect a provider, add a Model, and call /v1 with a service-account key to see usage here. ", setupLink("Connect a provider", "providers"), " ", setupLink("Add a Model", "models"));
    summary.append(empty);
    byId("overview-outcomes").replaceChildren();
    renderUsageGroups(breakdownReport, dimension, true);
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
    const nameSpan = element("span", "", outcomeLabel(name));
    nameSpan.title = outcomeDescription(name);
    row.append(nameSpan, element("strong", "", `${formatNumber(count)} · ${rate} of ${formatNumber(totalRequests)} client requests`));
    outcomeList.append(row);
  }
  byId("overview-outcomes").replaceChildren(outcomeList, outcomeLegend());
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
    // Review B8: failed and interrupted rows get a tint and the shared
    // human-labeled outcome tag instead of raw lowercase enum text.
    const outcomeCell = element("td");
    outcomeCell.append(outcomeTag(request.outcome));
    if (request.outcome === "failed" || request.outcome === "interrupted") row.classList.add("row-outcome-bad");
    row.append(element("td", "", formatDate(request.startedAt)), identityCell(account.name, account.id), identityCell(model.name, model.id), outcomeCell, element("td", "", formatDurationNS(request.durationNs)), element("td", "", formatOffsetNS(firstOutput)), action);
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
    // Review B3/B4: the status lines carry load feedback (the freshness
    // stamp), not permanent explainer text — those moved to the subtitles.
    showMessage(historyMessage, updatedStamp());
    showMessage(performanceMessage, updatedStamp());
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "overview") return;
    const message = historyErrorMessage(error);
    showMessage(historyMessage, `Could not load all-history usage: ${message}`, "error");
    showMessage(performanceMessage, `Could not load recent performance: ${message}`, "error");
  }
}

// Decision 8: the usage/performance panels auto-refresh every 30 s while the
// tab is visible and Overview is the open page. Each tick goes through
// beginHistoryFetch, which aborts any in-flight load first, so ticks can
// never pile up; hidden tabs and other pages skip the fetch entirely. The
// status panel and setup health keep manual refresh only. Refresh scope is
// unified: the single page-level Refresh button reloads status, usage,
// performance, and setup health together.
const AUTO_REFRESH_MS = 30000;
let autoRefreshTimer = 0;

export function startOverviewAutoRefresh() {
  if (autoRefreshTimer) return;
  autoRefreshTimer = window.setInterval(() => {
    if (document.hidden || state.page !== "overview") return;
    loadOverviewHistory();
  }, AUTO_REFRESH_MS);
}

export function stopOverviewAutoRefresh() {
  if (!autoRefreshTimer) return;
  window.clearInterval(autoRefreshTimer);
  autoRefreshTimer = 0;
}

// Coming back to a visible tab refreshes immediately instead of waiting out
// the remainder of the 30 s interval.
document.addEventListener("visibilitychange", () => {
  if (document.hidden || !autoRefreshTimer || state.page !== "overview") return;
  loadOverviewHistory();
});

byId("overview-group-by").addEventListener("change", () => {
  if (state.page === "overview") loadOverviewHistory();
});
