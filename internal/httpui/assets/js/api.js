// Fetch helpers: the api() wrapper with CSRF handling and error unwrapping,
// the query-string builder, and the abort-token machinery that bounds the
// history queries (Overview, Requests, Storage).
import { historyState } from "./state.js";

const csrfToken = document.querySelector('meta[name="csrf-token"]')?.content ?? "";

export async function api(path, options = {}) {
  const headers = new Headers(options.headers ?? {});
  headers.set("Accept", "application/json");
  if (options.body !== undefined) headers.set("Content-Type", "application/json");
  if (options.method && !["GET", "HEAD"].includes(options.method.toUpperCase())) {
    headers.set("X-Gyemoim-CSRF", csrfToken);
  }
  const response = await fetch(path, {
    ...options,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  if (response.status === 204) return null;
  const text = await response.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { /* Use a status message below. */ }
  }
  if (!response.ok) {
    // The forced-change gate also covers API calls issued before the redirect:
    // the browser must finish the password change before anything else loads.
    if (data?.error?.code === "password_change_required") {
      window.location.assign("/change-password");
      throw new Error("A password change is required before using the management interface.");
    }
    // An expired 24-hour management session sends the stale tab back to the
    // sign-in page; the path guard keeps a redirect loop impossible.
    if (data?.error?.code === "unauthenticated" && window.location.pathname !== "/login") {
      window.location.assign("/login");
      throw new Error("Your management session has expired. Sign in again.");
    }
    const message = data?.error?.message || `Request failed (${response.status})`;
    // status/body ride along (message stays verbatim) so the errors.js
    // humanization layer can look up known responses by (status, code).
    const failure = new Error(message);
    failure.status = response.status;
    failure.body = data;
    throw failure;
  }
  return data;
}

export function addQuery(path, values) {
  const url = new URL(path, window.location.origin);
  for (const [key, value] of Object.entries(values)) {
    if (value !== "" && value !== null && value !== undefined) url.searchParams.set(key, String(value));
  }
  return `${url.pathname}${url.search}`;
}

export function abortHistoryChildren() {
  for (const controller of historyState.childControllers) controller.abort();
  historyState.childControllers.clear();
}

export function cancelHistoryFetches() {
  historyState.generation += 1;
  historyState.controller?.abort();
  historyState.controller = null;
  historyState.selectionToken = null;
  abortHistoryChildren();
}

export function beginHistoryFetch() {
  historyState.controller?.abort();
  abortHistoryChildren();
  historyState.selectionToken = null;
  const controller = new AbortController();
  historyState.controller = controller;
  historyState.generation += 1;
  return { generation: historyState.generation, controller };
}

export function beginHistoryChild(slot, parentToken) {
  slot.historyChildController?.abort();
  const controller = new AbortController();
  const sequence = Number(slot.dataset.historySequence || 0) + 1;
  slot.dataset.historySequence = String(sequence);
  slot.historyChildController = controller;
  historyState.childControllers.add(controller);
  const abortChild = () => controller.abort();
  if (parentToken.controller.signal.aborted) controller.abort();
  else parentToken.controller.signal.addEventListener("abort", abortChild, { once: true });
  return { generation: parentToken.generation, parentToken, controller, sequence, slot, abortChild };
}

export function finishHistoryChild(child) {
  historyState.childControllers.delete(child.controller);
  child.parentToken.controller.signal.removeEventListener("abort", child.abortChild);
}

export function historyFetchIsCurrent(token) {
  return token && token.generation === historyState.generation && !token.controller.signal.aborted;
}

export function historyChildIsCurrent(child) {
  return historyFetchIsCurrent(child.parentToken) && !child.controller.signal.aborted && Number(child.slot.dataset.historySequence) === child.sequence;
}
