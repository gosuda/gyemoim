// Requests page: the bounded history list with filters/pagination, the
// request detail panel (usage, timing timeline, chunked content viewers),
// and the paged response-event list.
import {
  addQuery,
  api,
  beginHistoryChild,
  beginHistoryFetch,
  cancelHistoryFetches,
  finishHistoryChild,
  historyChildIsCurrent,
  historyFetchIsCurrent,
} from "../api.js";
import { historyState, state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formatDate, formatDurationNS, formatNumber, formatOffsetNS, historyErrorMessage, identityCell, outcomeTag } from "../format.js";

function setRequestPageLoading(loading) {
  state.requestPageLoading = loading;
  if (loading) {
    byId("request-page-previous").disabled = true;
    byId("request-page-next").disabled = true;
    byId("request-list").querySelectorAll("button").forEach((item) => { item.disabled = true; });
    return;
  }
  byId("request-list").querySelectorAll("button").forEach((item) => { item.disabled = false; });
  byId("request-page-previous").disabled = state.requestPageIndex <= 0;
  byId("request-page-next").disabled = !byId("request-list").dataset.nextCursor;
}

function setDatalist(id, values) {
  const list = byId(id);
  list.replaceChildren();
  for (const [value, label] of values) {
    if (value) list.append(new Option(label || value, value));
  }
}

function populateRequestFilterOptions() {
  setDatalist("request-account-options", state.accounts.map((account) => [account.id, `${account.name} · ${account.id}`]));
  setDatalist("request-model-options", state.models.map((model) => [model.id, `${model.name} · ${model.id}`]));
  setDatalist("request-provider-options", state.providers.map((provider) => [provider.id, `${provider.name} · ${provider.id}`]));
  const upstreams = new Map();
  for (const model of state.models) if (model.upstreamModel) upstreams.set(model.upstreamModel, `${model.name} · ${model.upstreamModel}`);
  setDatalist("request-upstream-options", [...upstreams.entries()]);
}

function currentRequestFilters() {
  const from = byId("request-from").value.trim();
  const to = byId("request-to").value.trim();
  const utcPattern = /(?:Z|\+00:00)$/i;
  for (const [label, value] of [["Started from", from], ["Started before", to]]) {
    if (value && (!utcPattern.test(value) || Number.isNaN(Date.parse(value)))) throw new Error(`${label} must include a UTC suffix such as Z.`);
  }
  if (from && to && !(Date.parse(from) < Date.parse(to))) throw new Error("Started from must be earlier than Started before.");
  return {
    from,
    to,
    account_id: byId("request-account-id").value.trim(),
    model_id: byId("request-model-id").value.trim(),
    provider_id: byId("request-provider-id").value.trim(),
    upstream_model: byId("request-upstream-model").value.trim(),
    outcome: byId("request-outcome").value,
  };
}

function resetRequestCursors() {
  state.requestCursors = [""];
  state.requestPageIndex = 0;
}

export async function loadRequestHistory(selectID = "") {
  const token = beginHistoryFetch();
  const message = byId("request-list-message");
  showMessage(message, "Loading request history…");
  byId("request-list").replaceChildren();
  byId("request-results-summary").textContent = "Loading a bounded page…";
  byId("request-page-previous").disabled = true;
  byId("request-page-next").disabled = true;
  try {
    const [accounts, models, providers] = await Promise.all([
      api("/api/service-accounts", { signal: token.controller.signal }),
      api("/api/models", { signal: token.controller.signal }),
      api("/api/providers", { signal: token.controller.signal }),
    ]);
    if (!historyFetchIsCurrent(token) || state.page !== "requests") return;
    state.accounts = accounts;
    state.models = models;
    state.providers = providers;
    populateRequestFilterOptions();
    state.appliedRequestFilters = currentRequestFilters();
    resetRequestCursors();
    await fetchRequestPage("", token, state.appliedRequestFilters);
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "requests") return;
    showMessage(message, `Could not load request history: ${historyErrorMessage(error)}`, "error");
    setRequestPageLoading(false);
    byId("request-results-summary").textContent = "Request history is unavailable.";
  }
  // A ?select= deep link opens the detail panel once the page has settled;
  // the detail loads independently of the list query's outcome. The select
  // param stays in the hash so reload and bookmarks re-open the detail.
  if (selectID) await selectRequest(selectID);
}

async function fetchRequestPage(cursor, token, filters = state.appliedRequestFilters, onSuccess = null) {
  const path = addQuery("/api/requests", { ...(filters || {}), limit: 25, cursor });
  setRequestPageLoading(true);
  try {
    const page = await api(path, { signal: token.controller.signal });
    if (!historyFetchIsCurrent(token) || state.page !== "requests") return;
    onSuccess?.();
    renderRequestPage(page);
    showMessage(byId("request-list-message"), page.requests.length ? "Each page is bounded to 25 requests." : "No requests match these filters.");
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.page !== "requests") return;
    setRequestPageLoading(false);
    showMessage(byId("request-list-message"), `Could not load requests: ${historyErrorMessage(error)}`, "error");
    byId("request-results-summary").textContent = "Request history is unavailable.";
  }
}

function renderRequestPage(page) {
  const list = byId("request-list");
  list.replaceChildren();
  const requests = page.requests || [];
  if (!requests.length) {
    const row = element("tr");
    const cell = element("td", "empty-cell", "No requests match these filters.");
    cell.colSpan = 8;
    row.append(cell);
    list.append(row);
  }
  for (const request of requests) {
    const row = element("tr");
    if (state.selectedRequestID === request.requestId) row.classList.add("selected-row");
    row.dataset.requestId = request.requestId;
    const provider = request.provider;
    const providerCell = element("td");
    if (provider) providerCell.append(identityCell(provider.name, provider.id), element("small", "", provider.upstream_model || "Upstream model unavailable"));
    else providerCell.append(element("small", "", request.active ? "No actual provider observed yet" : "Actual provider unavailable"));
    const status = request.httpStatus === 0 ? (request.upstreamAttempts === 0 ? "0 · no transmission recorded" : "0 · no upstream HTTP status recorded") : formatNumber(request.httpStatus);
    const outcomeCell = element("td");
    outcomeCell.append(outcomeTag(request.outcome));
    const inspect = button(state.selectedRequestID === request.requestId ? "Selected" : "Inspect", "quiet small", () => selectRequest(request.requestId));
    row.append(element("td", "", formatDate(request.startedAt)), identityCell(request.serviceAccount?.name, request.serviceAccount?.id), identityCell(request.model?.name, request.model?.id), providerCell, outcomeCell, element("td", "", formatDurationNS(request.durationNs)), element("td", "", status));
    const action = element("td");
    action.append(inspect);
    row.append(action);
    list.append(row);
  }
  byId("request-list").dataset.nextCursor = page.nextCursor || "";
  const current = state.requestPageIndex + 1;
  byId("request-results-summary").textContent = `${formatNumber(requests.length)} requests on this page · page ${current} · total archive count is not scanned.`;
  setRequestPageLoading(false);
}

function requestPageNext() {
  if (byId("request-page-next").disabled || state.requestPageLoading) return;
  const currentIndex = state.requestPageIndex;
  const priorCursors = [...state.requestCursors];
  const nextCursor = byId("request-list").dataset.nextCursor;
  if (!nextCursor) return;
  const nextCursors = [...priorCursors];
  let nextIndex = currentIndex + 1;
  let cursor = nextCursor;
  if (nextIndex < nextCursors.length) {
    cursor = nextCursors[nextIndex];
  } else {
    if (nextCursors.length >= 100) {
      nextCursors.shift();
      nextIndex -= 1;
    }
    nextCursors.push(nextCursor);
  }
  const token = beginHistoryFetch();
  state.selectedRequestID = "";
  byId("request-detail-panel").hidden = true;
  setRequestPageLoading(true);
  fetchRequestPage(cursor, token, state.appliedRequestFilters, () => {
    state.requestCursors = nextCursors;
    state.requestPageIndex = nextIndex;
  });
}

function requestPagePrevious() {
  if (state.requestPageIndex <= 0 || state.requestPageLoading) return;
  const previousIndex = state.requestPageIndex - 1;
  const token = beginHistoryFetch();
  state.selectedRequestID = "";
  byId("request-detail-panel").hidden = true;
  setRequestPageLoading(true);
  fetchRequestPage(state.requestCursors[previousIndex], token, state.appliedRequestFilters, () => { state.requestPageIndex = previousIndex; });
}

async function applyRequestFilters() {
  const token = beginHistoryFetch();
  state.selectedRequestID = "";
  byId("request-detail-panel").hidden = true;
  let filters;
  try {
    filters = currentRequestFilters();
  } catch (error) {
    setRequestPageLoading(false);
    showMessage(byId("request-list-message"), error.message, "error");
    return;
  }
  state.appliedRequestFilters = filters;
  resetRequestCursors();
  byId("request-list").dataset.nextCursor = "";
  setRequestPageLoading(true);
  await fetchRequestPage("", token, filters);
}

function detailMetric(label, value) {
  const item = element("div", "detail-summary-item");
  item.append(element("span", "", label), element("strong", "", value));
  return item;
}

function detailSection(title, note = "") {
  const section = element("section", "detail-section");
  section.append(element("h4", "", title));
  if (note) section.append(element("p", "detail-note", note));
  return section;
}

function usageValue(value) {
  return value === null || value === undefined ? "Unknown" : formatNumber(value);
}

function renderRequestUsage(summary) {
  const usage = summary.usage;
  const section = detailSection("Provider-reported usage", "Cached input and reasoning output are subsets of input/output totals.");
  if (!usage) {
    section.append(element("p", "empty-inline", "Usage was not reported."));
    return section;
  }
  const values = element("div", "detail-summary-grid");
  values.append(detailMetric("Input tokens", usageValue(usage.input_tokens)), detailMetric("Output tokens", usageValue(usage.output_tokens)), detailMetric("Cached input · subset", usageValue(usage.cached_input_tokens)), detailMetric("Reasoning output · subset", usageValue(usage.reasoning_output_tokens)));
  const input = usage.input_tokens;
  const cached = usage.cached_input_tokens;
  let ratio = "Unavailable";
  if (typeof input === "number" && typeof cached === "number" && input > 0 && cached <= input) ratio = `${((cached / input) * 100).toFixed(1)}%`;
  else if (input === 0) ratio = "Unavailable · input is zero";
  else if (typeof input === "number" && typeof cached === "number" && cached > input) ratio = "Unavailable · cached input exceeds input";
  else ratio = "Unavailable · input or cached count unknown";
  section.append(values, element("p", "detail-note", `Cache ratio: ${ratio}. A reported zero count is shown as zero; missing counts are unknown.`));
  return section;
}

function renderTimeline(timings) {
  const section = detailSection("Timing timeline", "Offsets show when the gateway observed stages; they do not establish the cause of a delay.");
  const stages = [
    ["Authentication preparation", timings.authentication_preparation_ns],
    ["Connection requested", timings.connection_requested_ns],
    ["Connection established", timings.connection_established_ns],
    ["Request transmission", timings.request_transmission_ns],
    ["First response event", timings.first_event_ns],
    ["First output", timings.first_output_ns],
    ["Stream completion", timings.stream_completion_ns],
    ["Downstream delivery", timings.downstream_delivery_ns],
  ];
  const observed = stages.filter(([, value]) => value !== null && value !== undefined && Number.isFinite(Number(value))).map(([name, value]) => [name, Number(value)]);
  if (!observed.length) {
    section.append(element("p", "empty-inline", "No timing offsets were recorded."));
    return section;
  }
  const list = element("ol", "timeline-list");
  let previous = null;
  for (const [name, value] of observed) {
    const item = element("li", "timeline-item");
    item.append(element("strong", "", name), element("span", "", `Offset ${formatOffsetNS(value)}`));
    if (previous) {
      if (value >= previous[1]) item.append(element("span", "", `${formatDurationNS(value - previous[1])} since ${previous[0]}`));
      else item.append(element("span", "", `Earlier offset than ${previous[0]} · ordering is inconsistent`));
    }
    list.append(item);
    previous = [name, value];
  }
  if (typeof timings.connection_reused === "boolean") list.append(Object.assign(element("li", "timeline-item"), { textContent: `Connection reused: ${timings.connection_reused ? "yes" : "no"}` }));
  section.append(list);
  return section;
}

function utf8Preview(bytes, offset, total) {
  let start = 0;
  let end = bytes.length;
  if (offset > 0) {
    while (start < Math.min(3, end) && (bytes[start] & 0xc0) === 0x80) start += 1;
  }
  if (offset + bytes.length < total && end > 0) {
    let continuation = 0;
    while (continuation < Math.min(3, end) && (bytes[end - 1 - continuation] & 0xc0) === 0x80) continuation += 1;
    const leadIndex = end - continuation - 1;
    if (leadIndex >= 0) {
      const lead = bytes[leadIndex];
      let expected = 1;
      if ((lead & 0xf8) === 0xf0) expected = 4;
      else if ((lead & 0xf0) === 0xe0) expected = 3;
      else if ((lead & 0xe0) === 0xc0) expected = 2;
      if (expected > continuation + 1) end = leadIndex;
    }
  }
  const previewBytes = bytes.subarray(start, Math.max(start, end));
  let invalidUTF8 = false;
  try { new TextDecoder("utf-8", { fatal: true }).decode(previewBytes); } catch { invalidUTF8 = true; }
  const text = new TextDecoder("utf-8", { fatal: false }).decode(previewBytes);
  return { text, skippedStart: start, skippedEnd: bytes.length - end, invalidUTF8 };
}

async function loadContentChunk(slot, label, url, offset, existingToken = null) {
  const token = existingToken || historyState.selectionToken;
  if (!historyFetchIsCurrent(token)) return;
  if (state.page !== "requests" || !state.selectedRequestID) return;
  const child = beginHistoryChild(slot, token);
  slot.dataset.requestId = state.selectedRequestID;
  slot.chunkLabel = label;
  slot.chunkURL = url;
  slot.chunkOffset = offset;
  slot.replaceChildren(element("p", "content-state", `Loading ${label} at byte ${formatNumber(offset)}…`));
  const path = addQuery(url, { offset, limit: 256 * 1024 });
  try {
    const response = await fetch(path, { credentials: "same-origin", cache: "no-store", signal: child.controller.signal, headers: { Accept: "application/octet-stream" } });
    if (!response.ok) {
      let message = `Request failed (${response.status})`;
      try { const data = await response.json(); message = data?.error?.message || message; } catch { /* Keep the safe status message. */ }
      throw new Error(message);
    }
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (!historyChildIsCurrent(child) || state.selectedRequestID !== slot.dataset.requestId) return;
    const actualOffset = Number(response.headers.get("X-Content-Offset") || offset);
    const total = Number(response.headers.get("X-Content-Total-Bytes") || 0);
    const complete = response.headers.get("X-Content-Complete") === "true";
    const truncated = response.headers.get("X-Content-Truncated") === "true";
    const readFailed = response.headers.get("X-Content-Read-Failed") === "true";
    const preview = utf8Preview(bytes, actualOffset, total);
    const summary = element("p", "content-state", `${label} · bytes ${formatNumber(actualOffset)}–${formatNumber(actualOffset + bytes.length)} of ${formatNumber(total)} · ${complete ? "all recorded bytes shown" : "more recorded bytes available"}${truncated ? " · recorded response was truncated" : ""}${readFailed ? " · recorded response read failed" : ""}${preview.skippedStart || preview.skippedEnd ? ` · UTF-8 boundary bytes omitted from text preview (${preview.skippedStart} at start, ${preview.skippedEnd} at end)` : ""}${preview.invalidUTF8 ? " · invalid UTF-8 bytes replaced in the text preview" : ""}`);
    const previewNote = element("p", "detail-note", "Text preview only. Chunk boundaries may split JSON, SSE, Unicode characters, or tool content; this preview does not claim to be a complete JSON value.");
    const pre = element("pre", "content-preview", preview.text || "(empty text preview)");
    const controls = element("div", "content-controls");
    const previous = button("Previous chunk", "small", () => loadContentChunk(slot, label, url, Math.max(0, actualOffset - 256 * 1024)));
    previous.disabled = actualOffset === 0;
    const next = button("Next chunk", "small", () => loadContentChunk(slot, label, url, actualOffset + bytes.length));
    next.disabled = complete || bytes.length === 0;
    controls.append(previous, next);
    slot.replaceChildren(summary, previewNote, controls, pre);
    slot.dataset.offset = String(actualOffset);
    slot.dataset.total = String(total);
    slot.dataset.complete = String(complete);
  } catch (error) {
    if (!historyChildIsCurrent(child) || state.selectedRequestID !== slot.dataset.requestId) return;
    const message = element("p", "content-state error", `Could not read ${label}: ${historyErrorMessage(error)}`);
    const retry = button("Retry this chunk", "small", () => loadContentChunk(slot, label, url, offset));
    slot.replaceChildren(message, retry);
  } finally {
    finishHistoryChild(child);
  }
}

function abortEventChunkLoads(container) {
  container.querySelectorAll(".content-chunk-slot").forEach((slot) => slot.historyChildController?.abort());
}

function renderEventPage(container, page, requestID) {
  container.replaceChildren();
  const list = element("div", "event-list");
  const events = page.events || [];
  if (!events.length) list.append(element("p", "empty-inline", "No response events were recorded."));
  for (const event of events) {
    const card = element("article", "event-card");
    const heading = element("div", "event-card-heading");
    const name = event.eventName || "Unnamed event";
    heading.append(element("strong", "", `#${event.sequence} · ${name}${event.eventNameTruncated ? " · name preview truncated" : ""}`), element("small", "", `${formatOffsetNS(event.elapsedNs)} · attempt ${event.attempt} · ${formatNumber(event.rawBytes)} bytes`));
    card.append(heading);
    if (event.rawOmitted) card.append(element("p", "detail-note", `Raw frame omitted from this event page: ${event.rawOmitted}. Load it in bounded chunks below.`));
    if (event.rawBase64) {
      const binary = atob(event.rawBase64);
      const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
      const preview = utf8Preview(bytes, 0, bytes.length);
      card.append(element("pre", "event-raw", preview.text));
      if (preview.invalidUTF8) card.append(element("p", "detail-note", "Invalid UTF-8 bytes were replaced in this plain-text preview."));
    } else {
      card.append(element("p", "detail-note", "Raw SSE text can include response data, tool calls, arguments, and tool results."));
    }
    const rawControls = element("div", "event-controls");
    const load = button(event.rawBase64 ? "Read full frame in chunks" : "Load raw frame", "small", () => {
      const slot = card.querySelector(".content-chunk-slot") || element("div", "content-chunk-slot");
      slot.dataset.requestId = requestID;
      if (!slot.isConnected) card.append(slot);
      loadContentChunk(slot, `Event #${event.sequence}`, event.rawUrl, 0);
    });
    rawControls.append(load);
    card.append(rawControls);
    card.append(element("p", "detail-note", `Event name preview: ${formatNumber(event.eventNameBytes)} bytes${event.eventNameTruncated ? " · shortened preview" : " · complete name"}.`));
    list.append(card);
  }
  const controls = element("div", "event-controls");
  const next = button("Next events", "small", () => loadNextEventPage(container, requestID, page.nextCursor));
  next.disabled = !page.hasMore;
  controls.append(element("span", "detail-note", `Showing ${formatNumber(events.length)} events on this bounded page.`), next);
  container.append(list, controls);
}

async function loadNextEventPage(container, requestID, cursor) {
  if (!cursor) return;
  const token = historyState.selectionToken;
  if (!historyFetchIsCurrent(token)) return;
  abortEventChunkLoads(container);
  const child = beginHistoryChild(container, token);
  container.replaceChildren(element("p", "content-state", "Loading next response-event page…"));
  try {
    const page = await api(addQuery(`/api/requests/${encodeURIComponent(requestID)}/events`, { limit: 25, cursor }), { signal: child.controller.signal });
    if (!historyChildIsCurrent(child) || state.selectedRequestID !== requestID || state.page !== "requests") return;
    renderEventPage(container, page, requestID);
  } catch (error) {
    if (!historyChildIsCurrent(child)) return;
    const message = element("p", "content-state error", `Could not load response events: ${historyErrorMessage(error)}`);
    const retry = button("Retry event page", "small", () => loadNextEventPage(container, requestID, cursor));
    container.replaceChildren(message, retry);
  } finally {
    finishHistoryChild(child);
  }
}

async function loadInitialEventPage(container, url, requestID) {
  const token = historyState.selectionToken;
  if (!historyFetchIsCurrent(token)) return;
  abortEventChunkLoads(container);
  const child = beginHistoryChild(container, token);
  container.replaceChildren(element("p", "content-state", "Loading response events…"));
  try {
    const page = await api(url, { signal: child.controller.signal });
    if (!historyChildIsCurrent(child) || state.selectedRequestID !== requestID || state.page !== "requests") return;
    renderEventPage(container, page, requestID);
  } catch (error) {
    if (!historyChildIsCurrent(child)) return;
    const message = element("p", "content-state error", `Could not load response events: ${historyErrorMessage(error)}`);
    const retry = button("Retry response events", "small", () => loadInitialEventPage(container, url, requestID));
    container.replaceChildren(message, retry);
  } finally {
    finishHistoryChild(child);
  }
}

function renderRequestDetails(detail, token) {
  const content = byId("request-detail-content");
  const summary = detail.request || {};
  const account = summary.serviceAccount || {};
  const model = summary.model || {};
  const provider = summary.provider;
  const incoming = detail.incomingRequestUrl;
  content.replaceChildren();
  const grid = element("div", "detail-summary-grid");
  grid.append(
    detailMetric("Service account", `${account.name || "Historical name unavailable"} · ${account.id || "ID unavailable"}`),
    detailMetric("Requested Model", `${model.name || "Historical name unavailable"} · ${model.id || "ID unavailable"}`),
    detailMetric("Model configuration version", model.version ? String(model.version) : "Unknown"),
    detailMetric("Selection strategy", model.strategy || "Unknown"),
    detailMetric("Actual provider", provider ? `${provider.name || "Historical name unavailable"} · ${provider.id}` : "Unknown / not yet selected"),
    detailMetric("Actual upstream model", provider?.upstream_model || "Unknown"),
    detailMetric("Local request ID", summary.requestId || "Unknown"),
    detailMetric("Upstream request ID", summary.upstreamRequestId || "Unknown"),
    detailMetric("Started", formatDate(summary.startedAt)),
    detailMetric("Outcome", summary.outcome || "Unknown"),
    detailMetric("Duration", formatDurationNS(summary.durationNs)),
    detailMetric("HTTP status", summary.httpStatus === 0 ? (summary.upstreamAttempts === 0 ? "0 · no transmission recorded" : "0 · no upstream HTTP status recorded") : formatNumber(summary.httpStatus)),
  );
  content.append(grid);
  if (summary.safeError) {
    const error = detailSection("Recorded safe error");
    error.append(element("pre", "event-raw", summary.safeError));
    content.append(error);
  }
  content.append(renderRequestUsage(summary), renderTimeline(summary.timings || {}));

  const incomingSection = detailSection("Incoming request body", "Recorded request bytes are shown as bounded plain text. The preview is not parsed as complete JSON.");
  const incomingViewer = element("div", "content-chunk-slot");
  incomingViewer.dataset.requestId = summary.requestId;
  incomingSection.append(incomingViewer);
  content.append(incomingSection);
  if (incoming) loadContentChunk(incomingViewer, "Incoming request", incoming, 0, token);
  else incomingViewer.append(element("p", "empty-inline", "Incoming request bytes are unavailable."));

  const attempts = detail.attempts || [];
  const effectiveSection = detailSection("Effective upstream requests", "Each attempt is shown separately. These bytes reflect the effective request sent to that actual provider target.");
  const attemptList = element("div", "response-list");
  for (const attempt of attempts) {
    const row = element("div", "response-row");
    const info = element("div");
    info.append(element("strong", "", `Attempt ${attempt.number} · ${attempt.provider?.name || "Historical provider"}`), element("small", "", `${attempt.provider?.id || "Provider ID unavailable"} · ${attempt.provider?.upstream_model || "Upstream model unavailable"} · ${formatNumber(attempt.effectiveRequestBytes)} bytes`));
    const holder = element("div");
    const viewer = element("div", "content-chunk-slot");
    viewer.dataset.requestId = summary.requestId;
    holder.append(button("Read effective request", "small", () => loadContentChunk(viewer, `Effective request · attempt ${attempt.number}`, attempt.effectiveRequestUrl, 0)), viewer);
    row.append(info, holder);
    attemptList.append(row);
  }
  if (!attempts.length) attemptList.append(element("p", "empty-inline", "No upstream transmission was recorded."));
  effectiveSection.append(attemptList);
  if (detail.attemptsTruncated) effectiveSection.append(element("p", "detail-note", `Attempt metadata was capped at ${formatNumber(attempts.length)} entries. Additional request bytes remain addressable by attempt number.`));
  content.append(effectiveSection);

  const responsesSection = detailSection("Upstream HTTP responses", "HTTP response bodies remain separate from streamed SSE event frames.");
  const responseList = element("div", "response-list");
  for (const response of detail.upstreamResponses || []) {
    const row = element("div", "response-row");
    const info = element("div");
    const status = response.httpStatus === 0 ? "0 · no upstream HTTP status recorded" : formatNumber(response.httpStatus);
    info.append(element("strong", "", `Attempt ${response.attempt} · HTTP ${status}`), element("small", "", `Upstream request ID: ${response.upstreamRequestId || "Unknown"} · ${formatNumber(response.bodyBytes)} bytes${response.bodyTruncated ? " · body truncated" : ""}${response.bodyReadFailed ? " · body read failed" : ""}`));
    const holder = element("div");
    const viewer = element("div", "content-chunk-slot");
    viewer.dataset.requestId = summary.requestId;
    holder.append(button("Read response body", "small", () => loadContentChunk(viewer, `HTTP response body · attempt ${response.attempt}`, response.bodyUrl, 0)), viewer);
    row.append(info, holder);
    responseList.append(row);
  }
  if (!responseList.childElementCount) responseList.append(element("p", "empty-inline", "No upstream HTTP response body was recorded."));
  responsesSection.append(responseList);
  if (detail.responsesTruncated) responsesSection.append(element("p", "detail-note", `Response metadata was capped at ${formatNumber((detail.upstreamResponses || []).length)} entries.`));
  content.append(responsesSection);

  const eventsSection = detailSection("Response events and tool content", "Raw SSE frames are plain text. Tool arguments and results, when recorded, remain part of those frames.");
  const eventContainer = element("div", "event-page-container");
  eventContainer.append(element("p", "content-state", "Loading response events…"));
  eventsSection.append(element("p", "detail-note", `${formatNumber(detail.eventCount)} events · ${formatNumber(detail.eventBytes)} recorded event bytes · event frames also load in bounded, replaceable chunks.`), eventContainer);
  content.append(eventsSection);
  const eventChild = beginHistoryChild(eventContainer, token);
  api(detail.eventsUrl, { signal: eventChild.controller.signal }).then((page) => {
    if (!historyChildIsCurrent(eventChild) || state.selectedRequestID !== summary.requestId) return;
    renderEventPage(eventContainer, page, summary.requestId);
  }).catch((error) => {
    if (!historyChildIsCurrent(eventChild) || state.selectedRequestID !== summary.requestId) return;
    const message = element("p", "content-state error", `Could not load response events: ${historyErrorMessage(error)}`);
    const retry = button("Retry response events", "small", () => loadInitialEventPage(eventContainer, detail.eventsUrl, summary.requestId));
    eventContainer.replaceChildren(message, retry);
  }).finally(() => finishHistoryChild(eventChild));
}

async function selectRequest(requestID) {
  state.selectedRequestID = requestID;
  const token = beginHistoryFetch();
  historyState.selectionToken = token;
  const panel = byId("request-detail-panel");
  panel.hidden = false;
  byId("request-detail-subtitle").textContent = `Loading request ${requestID}…`;
  byId("request-detail-content").replaceChildren(element("p", "content-state", "Loading bounded request details…"));
  panel.scrollIntoView({ behavior: "smooth", block: "start" });
  try {
    const detail = await api(`/api/requests/${encodeURIComponent(requestID)}`, { signal: token.controller.signal });
    if (!historyFetchIsCurrent(token) || state.selectedRequestID !== requestID || state.page !== "requests") return;
    const summary = detail.request || {};
    byId("request-detail-subtitle").textContent = `${summary.outcome || "unknown"} · started ${formatDate(summary.startedAt)} · local request ID ${summary.requestId || requestID}`;
    historyState.selectionToken = token;
    renderRequestDetails(detail, token);
    renderRequestPageSelection(requestID);
  } catch (error) {
    if (!historyFetchIsCurrent(token) || state.selectedRequestID !== requestID) return;
    byId("request-detail-content").replaceChildren(element("p", "content-state error", `Could not load request details: ${historyErrorMessage(error)}`));
  }
}

function renderRequestPageSelection(requestID) {
  document.querySelectorAll("#request-list tr").forEach((row) => row.classList.remove("selected-row"));
  const selected = [...document.querySelectorAll("#request-list tr")].find((row) => row.dataset.requestId === requestID);
  selected?.classList.add("selected-row");
}

byId("request-filter-form").addEventListener("submit", (event) => {
  event.preventDefault();
  applyRequestFilters();
});
byId("request-filter-reset").addEventListener("click", () => {
  byId("request-filter-form").reset();
  applyRequestFilters();
});
byId("request-refresh").addEventListener("click", () => {
  state.selectedRequestID = "";
  byId("request-detail-panel").hidden = true;
  loadRequestHistory();
});
byId("request-page-next").addEventListener("click", requestPageNext);
byId("request-page-previous").addEventListener("click", requestPagePrevious);
byId("request-detail-close").addEventListener("click", () => {
  cancelHistoryFetches();
  state.selectedRequestID = "";
  byId("request-detail-panel").hidden = true;
});
