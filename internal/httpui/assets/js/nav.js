// Hash routing and navigation. The SPA renders from location.hash
// (#/page?key=value). navigate() is a thin wrapper that writes the hash;
// renderRoute() is the single render entry point, driven by hashchange
// (Back/Forward, manual edits, bookmarks) and called directly right after
// writing the hash.
//
// Cycle note: nav.js imports the page loaders and the page modules import
// navigate back from nav.js. This is safe because both sides only export
// function declarations (hoisted) that are called at runtime, never during
// module evaluation.
import { cancelHistoryFetches } from "./api.js";
import { state } from "./state.js";
import { byId } from "./dom.js";
import { loadStatus, loadOverviewHistory } from "./pages/overview.js";
import { loadProviders } from "./pages/providers.js";
import { loadAccounts } from "./pages/service-accounts.js";
import { loadModelsAndProviders } from "./pages/models.js";
import { loadRequestHistory } from "./pages/requests.js";
import { loadStorage } from "./pages/storage.js";
import { loadUsers } from "./pages/users.js";

const titles = {
  overview: "Overview",
  providers: "Providers",
  "service-accounts": "Service accounts",
  models: "Models",
  requests: "Requests",
  storage: "Storage",
  users: "Users",
};

// The last hash this SPA rendered. It guards against double renders when a
// location change both renders directly and fires hashchange afterwards.
let renderedHash = "";

function buildHash(page, params) {
  const query = params ? new URLSearchParams(params).toString() : "";
  return `#/${page}${query ? `?${query}` : ""}`;
}

function parseHash() {
  const raw = window.location.hash.replace(/^#\/?/, "");
  const [name, query = ""] = raw.split("?");
  const page = Object.hasOwn(titles, name) ? name : "";
  return { page, params: new URLSearchParams(query) };
}

export function renderRoute() {
  const { page, params } = parseHash();
  if (!page) {
    // Unknown or missing page: normalize without adding a history entry.
    if (window.location.hash !== "#/overview") window.location.replace("#/overview");
    applyHash("#/overview", "overview", new URLSearchParams());
    return;
  }
  applyHash(window.location.hash, page, params);
}

function applyHash(hash, page, params) {
  if (hash === renderedHash) return;
  renderedHash = hash;
  showPage(page, params);
}

function showPage(page, params = new URLSearchParams()) {
  cancelHistoryFetches();
  if (state.page === "service-accounts" && page !== state.page) {
    for (const clear of [...state.sensitiveCleanup]) clear();
  }
  if (page === "requests") {
    state.selectedRequestID = "";
    byId("request-detail-panel").hidden = true;
  }
  state.page = page;
  document.querySelectorAll("[data-page]").forEach((item) => {
    const selected = item.dataset.page === page;
    item.classList.toggle("active", selected);
    if (selected) item.setAttribute("aria-current", "page");
    else item.removeAttribute("aria-current");
  });
  document.querySelectorAll("[data-view]").forEach((view) => {
    view.hidden = view.dataset.view !== page;
  });
  document.title = `${titles[page]} · Gyemoim`;
  byId("page-title").textContent = titles[page];
  byId("breadcrumb").textContent = titles[page].toUpperCase();
  refreshPage(page, params);
}

export function navigate(page, params = null) {
  if (!titles[page]) return;
  const hash = buildHash(page, params);
  if (window.location.hash === hash) return;
  window.location.hash = hash;
  renderRoute();
}

export async function refreshPage(page = state.page, params = null) {
  if (page === "overview") return Promise.all([loadStatus(), loadOverviewHistory()]);
  if (page === "providers") return loadProviders();
  if (page === "service-accounts") return loadAccounts();
  if (page === "models") return loadModelsAndProviders();
  if (page === "requests") return loadRequestHistory(params?.get("select") || "");
  if (page === "storage") return loadStorage();
  if (page === "users") return loadUsers();
}

document.querySelectorAll("[data-page]").forEach((item) => item.addEventListener("click", () => navigate(item.dataset.page)));
document.querySelectorAll("[data-refresh]").forEach((item) => item.addEventListener("click", () => refreshPage(item.dataset.refresh)));
