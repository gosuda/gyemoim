// Read-only, process-local observations. Never writes configuration or touches
// editor controls. One page-scoped request at a time; countdowns only edit text.
import { api } from "./api.js";
import { state } from "./state.js";
import { element } from "./dom.js";
import { formatUTC } from "./format.js";

let generation = 0;
let controller = null;
let pollTimer = 0;
let ticker = 0;
let snapshot = null;
let receivedAt = 0;
let problem = "";
let page = "";
let loggedOut = false;
const bindings = new Map();
const pairKey = (target) => JSON.stringify([target.providerId, target.upstreamModel]);
const serverNow = () => Date.parse(snapshot.observedAt) + Math.max(0, performance.now() - receivedAt);
const remaining = (value) => Math.max(0, Math.ceil((Date.parse(value) - serverNow()) / 1000));
const cooling = (target) => target.status === "cooling" && (!target.eligibleAt || remaining(target.eligibleAt) > 0);

function sourceLabel(source) {
  if (source === "default_60s") return "default 60 s";
  if (source === "retry_after_seconds") return "Retry-After seconds";
  if (source === "retry_after_date") return "Retry-After HTTP date";
  if (source === "default_30s") return "default 30 s";
  return source || "source unavailable";
}

function targetText(target) {
  let text;
  if (target.status === "disconnected") text = "Disconnected · connection/authentication required; a cooldown expiry cannot reconnect it.";
  else if (cooling(target)) text = `${target.reason === "rate_limited" ? "429 cooldown" : "Cooldown"} · ${target.reason || "reason unavailable"} · retry in ~${target.eligibleAt ? remaining(target.eligibleAt) : "?"} s`;
  else text = target.lastFailure ? "Retry eligible · not verified healthy" : "Eligible · no failure observed in this process; not verified healthy";
  if (target.eligibleAt) text += ` · observed retry time ${formatUTC(target.eligibleAt)} (${sourceLabel(target.delaySource)})`;
  if (target.lastFailure) text += ` · Last failure: ${target.lastFailure.reason}${target.lastFailure.httpStatus ? ` (HTTP ${target.lastFailure.httpStatus})` : ""} at ${formatUTC(target.lastFailure.at)}`;
  if (target.last429) {
    const failure = target.last429;
    text += ` · Last 429: ${formatUTC(failure.at)} · 429 retry time ${failure.eligibleAt ? formatUTC(failure.eligibleAt) : "unknown"} (${sourceLabel(failure.delaySource)})`;
  }
  return text;
}

function availabilityText() {
  if (problem) return snapshot ? "Runtime status stale/unavailable · last observations below are not current. Retrying automatically." : "Runtime status unavailable · eligibility unknown. Retrying automatically.";
  if (!snapshot) return "Loading runtime status · eligibility unknown. No provider probes are performed.";
  return `Runtime observations at ${formatUTC(snapshot.observedAt)} · refresh ~8 s; countdowns approximate. Not a health check or running-request indicator.`;
}

function updateBindings() {
  for (const [node, render] of bindings) {
    if (!node.isConnected) { bindings.delete(node); continue; }
    render();
  }
}

function bind(node, render) {
  bindings.set(node, render);
  render();
  return node;
}

export function modelRuntime(model, targetSlots) {
  const box = element("section", "routing-runtime");
  box.setAttribute("aria-label", "Saved route runtime observations");
  const notice = element("p", "detail-note");
  const prediction = element("p", "runtime-prediction");
  const targets = model.targets?.length ? model.targets : [{ providerId: model.providerId, upstreamModel: model.upstreamModel }];
  const lines = targets.map((target, index) => element("p", "runtime-target", `Priority ${index + 1} · ${target.upstreamModel}`));
  box.append(notice, prediction);
  lines.forEach((line, index) => targetSlots[index].append(line));
  if ((model.strategy || "single") === "single") box.append(element("p", "detail-note", "Single never skips its connected target due to observed cooldown and never falls back. An observed 429 delay is diagnostic, not a local Single send block."));
  return bind(box, () => {
    notice.textContent = availabilityText();
    const current = snapshot?.models.find((item) => item.id === model.id);
    // A saved revision and a pair, never an array index, identify observations.
    const matches = current && current.version === model.version;
    prediction.textContent = !snapshot ? (problem ? "Prediction unavailable · eligibility unknown." : "Prediction unavailable while loading.") : !matches ? "Saved configuration changed or was removed. Refresh Models to see current cards; your editor draft is unchanged." : "Prediction unavailable while status is stale.";
    lines.forEach((line, index) => {
      const target = matches && current.targets.find((item) => pairKey(item) === pairKey(targets[index]));
      line.textContent = `${problem ? "Stale observation · " : ""}${target ? targetText(target) : "Unknown · no matching saved-revision observation"}`;
    });
    if (!matches || problem) return;
    const predicted = current.executionEnabled && current.targets.find((target) => target.status !== "disconnected" && (current.strategy === "single" || !cooling(target)));
    const index = predicted ? targets.findIndex((target) => pairKey(target) === pairKey(predicted)) : -1;
    prediction.textContent = index >= 0 ? `Predicted first target · Priority ${index + 1} · ${predicted.upstreamModel} (prediction only, not active execution).` : `No predicted target · ${current.unavailableReason || "all targets unavailable"}.`;
  });
}

export function providerRuntime(providerId) {
  const box = element("section", "routing-runtime");
  box.setAttribute("aria-label", "Upstream model runtime observations");
  const notice = element("p", "detail-note");
  const note = element("p", "detail-note", "Observations are per upstream model, shared across aliases, not an account-wide 429 or connection status. Single routes do not skip observed cooldowns.");
  const list = element("div");
  const lines = new Map();
  box.append(element("h5", "", "Upstream model observations"), notice, note, list);
  return bind(box, () => {
    notice.textContent = availabilityText();
    if (!snapshot) {
      const unknown = "Upstream model eligibility unknown until runtime status loads.";
      if (list.textContent !== unknown) list.textContent = unknown;
      lines.clear();
      return;
    }
    const pairs = new Map();
    for (const model of snapshot.models) for (const target of model.targets) {
      if (target.providerId === providerId) pairs.set(pairKey(target), target);
    }
    for (const [key, line] of lines) if (!pairs.has(key)) { line.remove(); lines.delete(key); }
    for (const [key, target] of pairs) {
      if (!lines.has(key)) { const line = element("p", "runtime-target"); lines.set(key, line); list.append(line); }
      lines.get(key).textContent = `${target.upstreamModel} · ${problem ? "Stale observation · " : ""}${targetText(target)}`;
    }
    if (!pairs.size) list.textContent = "No configured upstream model pairs for this provider.";
    // The empty-state text is not a pair node and must not survive new pairs.
    else for (const child of [...list.childNodes]) if (child.nodeType === Node.TEXT_NODE) child.remove();
  });
}

export function stopRoutingStatus() {
  generation += 1;
  controller?.abort();
  controller = null;
  clearTimeout(pollTimer);
  clearInterval(ticker);
  pollTimer = ticker = 0;
}

export function syncRoutingStatus() {
  stopRoutingStatus();
  if (loggedOut || document.hidden || !["models", "providers"].includes(state.page)) return;
  page = state.page;
  snapshot = null;
  problem = "";
  updateBindings();
  const token = generation;
  const current = () => token === generation && state.page === page && !document.hidden && !loggedOut;
  const poll = async () => {
    if (!current()) return;
    const request = new AbortController();
    controller = request;
    const timeout = setTimeout(() => request.abort(), 10000);
    try {
      const data = await api("/api/routing/status", { signal: request.signal });
      if (!current()) return;
      if (!Array.isArray(data?.models) || !Number.isFinite(Date.parse(data.observedAt))) throw new Error("Invalid runtime snapshot");
      snapshot = data;
      receivedAt = performance.now();
      problem = "";
    } catch {
      if (!current()) return;
      problem = "unavailable";
    } finally {
      clearTimeout(timeout);
      if (controller === request) controller = null;
      if (current()) { updateBindings(); pollTimer = setTimeout(poll, 8000); }
    }
  };
  ticker = setInterval(updateBindings, 1000);
  poll();
}

export function logoutRoutingStatus() {
  loggedOut = true;
  stopRoutingStatus();
}
document.addEventListener("visibilitychange", syncRoutingStatus);
window.addEventListener("pagehide", stopRoutingStatus);
window.addEventListener("pageshow", (event) => { if (event.persisted) syncRoutingStatus(); });
