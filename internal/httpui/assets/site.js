(() => {
  "use strict";

  const csrfToken = document.querySelector('meta[name="csrf-token"]')?.content ?? "";
  const state = {
    page: "overview",
    providers: [],
    accounts: [],
    models: [],
    users: [],
    currentUserID: "",
    editingModel: null,
    catalogRequest: 0,
    sensitiveCleanup: new Set(),
    pendingRequestID: "",
    requestCursors: [""],
    requestPageIndex: 0,
    appliedRequestFilters: null,
    selectedRequestID: "",
  };
  const historyState = { generation: 0, controller: null, childControllers: new Set(), selectionToken: null };
  const titles = {
    overview: "Overview",
    providers: "Providers",
    "service-accounts": "Service accounts",
    models: "Models",
    requests: "Requests",
    storage: "Storage",
    users: "Users",
  };

  const byId = (id) => document.getElementById(id);
  const element = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = String(text);
    return node;
  };
  const button = (label, className, action) => {
    const node = element("button", `button ${className}`, label);
    node.type = "button";
    if (action) node.addEventListener("click", action);
    return node;
  };
  const showMessage = (node, message = "", kind = "") => {
    node.textContent = message;
    node.className = kind ? `form-message ${kind}` : "form-message";
  };

  async function api(path, options = {}) {
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
      const message = data?.error?.message || `Request failed (${response.status})`;
      throw new Error(message);
    }
    return data;
  }

  function navigate(page) {
    if (!titles[page]) return;
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
    byId("page-title").textContent = titles[page];
    byId("breadcrumb").textContent = titles[page].toUpperCase();
    refreshPage(page);
  }

  async function refreshPage(page = state.page) {
    if (page === "overview") return Promise.all([loadStatus(), loadOverviewHistory()]);
    if (page === "providers") return loadProviders();
    if (page === "service-accounts") return loadAccounts();
    if (page === "models") return loadModelsAndProviders();
    if (page === "requests") return loadRequestHistory();
    if (page === "storage") return loadStorage();
    if (page === "users") return loadUsers();
  }

  async function loadStorage() {
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

  function formatUTC(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "Unknown time";
    return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium", timeZone: "UTC" }).format(date) + " UTC";
  }

  function renderDeletionStatus(deletion = {}) {
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

  function formatBytes(value) {
    const bytes = Number(value) || 0;
    if (bytes < 1024) return `${formatNumber(bytes)} B`;
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let scaled = bytes / 1024;
    let unit = units[0];
    for (let index = 1; scaled >= 1024 && index < units.length; index++) {
      scaled /= 1024;
      unit = units[index];
    }
    return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(scaled)} ${unit}`;
  }

  async function loadStatus() {
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

  async function loadProviders() {
    const list = byId("provider-list");
    list.replaceChildren(element("p", "muted", "Loading providers…"));
    try {
      state.providers = await api("/api/providers");
      renderProviders();
    } catch (error) {
      list.replaceChildren(element("p", "empty-state", `Could not load providers: ${error.message}`));
    }
  }

  function renderProviders() {
    const list = byId("provider-list");
    list.replaceChildren();
    if (state.providers.length === 0) {
      list.append(element("div", "empty-state", "No providers yet. Add one to prepare a Model route."));
      return;
    }
    for (const provider of state.providers) list.append(renderProvider(provider));
  }

  function providerStatusLabel(status) {
    if (status === "connected") return "Connected";
    if (status === "plan_usage_disabled") return "Plan usage disabled";
    if (status === "require_reauthentication") return "Re-authentication required";
    if (status === "failed") return "Connection failed";
    return "Disconnected";
  }

  function providerStatusDescription(status) {
    if (status === "connected") return "This OpenAI account is connected and ready for direct inference.";
    if (status === "plan_usage_disabled") return "Plan usage disabled — enable API access on the ChatGPT plan, then reconnect this account.";
    if (status === "require_reauthentication") return "Re-authentication required. The saved refresh grant can no longer renew access; reconnect before using this account.";
    if (status === "failed") return "The last connection attempt failed. Start the connect flow again.";
    return "Connect an OpenAI account with Sign in with ChatGPT.";
  }

  function providerStatusTagClass(status) {
    if (status === "connected") return "tag-success";
    if (["plan_usage_disabled", "require_reauthentication", "failed"].includes(status)) return "tag-danger";
    return "tag-muted";
  }

  // buildConnectPanel renders the inline enrollment panel for one provider
  // card: the single-use code with a live countdown, the script download, the
  // exact command line with the browser's own origin substituted, and a poll
  // of the provider list that closes the panel when the card leaves its
  // starting status. Polling stops when the code expires, when the panel is
  // removed, or when the user leaves the Providers page.
  function buildConnectPanel(provider, enrollment) {
    const panel = element("div", "connect-panel");
    const panelMessage = element("p", "form-message");
    panelMessage.setAttribute("aria-live", "polite");
    const seconds = Number(enrollment.expiresInSeconds);
    const expiryAt = Date.now() + (Number.isFinite(seconds) ? Math.max(0, seconds) : 0) * 1000;
    let stopped = false;
    let pollTimer = 0;
    let ticker = 0;
    const stop = () => {
      stopped = true;
      window.clearTimeout(pollTimer);
      window.clearInterval(ticker);
    };
    panel.connectStop = stop;

    const finishWithStatus = async (status) => {
      stop();
      panel.remove();
      await loadProviders();
      const notice = byId("provider-oauth-message");
      notice.hidden = false;
      if (status === "connected") {
        showMessage(notice, "OpenAI account connected. Direct inference access is ready.", "success");
      } else {
        const label = providerStatusLabel(status);
        const description = providerStatusDescription(status);
        showMessage(notice, `Connection attempt finished: ${description.startsWith(label) ? description : `${label}. ${description}`}`, "error");
      }
    };
    const finishExpired = () => {
      if (stopped) return;
      stop();
      showMessage(panelMessage, "The enrollment code expired without a completed connection. Connect again to issue a new code.", "error");
    };

    const poll = async () => {
      if (stopped || !panel.isConnected || state.page !== "providers") { stop(); return; }
      try {
        const providers = await api("/api/providers");
        if (stopped || !panel.isConnected || state.page !== "providers") return;
        const current = Array.isArray(providers) ? providers.find((item) => item.id === provider.id) : null;
        if (current && current.status !== provider.status) {
          await finishWithStatus(current.status);
          return;
        }
      } catch { /* Transient errors are retried on the next tick. */ }
      if (stopped || !panel.isConnected || state.page !== "providers") return;
      if (Date.now() >= expiryAt) { finishExpired(); return; }
      pollTimer = window.setTimeout(poll, 3000);
    };

    const heading = element("h5", "", "Connect with the enrollment script");
    const instructions = element("p", "muted", "Run the script on the machine with your web browser — where you sign in to ChatGPT. The script opens the browser; after signing in, return here and this card updates automatically.");
    const codeRow = element("div", "connect-code-row");
    const countdown = element("span", "muted connect-expiry", "");
    codeRow.append(element("strong", "connect-label", "Enrollment code (single use)"), countdown);
    const code = element("code", "connect-code", enrollment.code);
    code.tabIndex = 0;

    const command = String(enrollment.command || "").replace("SERVER_URL", window.location.origin);
    const commandLine = element("pre", "connect-command", command);
    const actions = element("div", "card-actions");
    const download = element("a", "button primary small", "Download script");
    download.href = enrollment.scriptUrl || "/api/connect/script";
    download.download = "gyemoim-connect.py";
    const copyMessage = element("span", "muted", "");
    actions.append(
      download,
      button("Copy command", "quiet small", async () => {
        try {
          await navigator.clipboard.writeText(command);
          copyMessage.textContent = "Command copied.";
        } catch {
          copyMessage.textContent = "Clipboard access is unavailable. Select and copy the command below.";
        }
      }),
      button("Close", "quiet small", () => { stop(); panel.remove(); }),
      copyMessage,
    );

    const renderCountdown = () => {
      const remaining = Math.max(0, Math.round((expiryAt - Date.now()) / 1000));
      countdown.textContent = remaining > 0
        ? `Expires in ${Math.floor(remaining / 60)}:${String(remaining % 60).padStart(2, "0")}`
        : "Code expired";
    };
    renderCountdown();
    showMessage(panelMessage, "Waiting for the connection to complete on the other machine…");

    ticker = window.setInterval(() => {
      if (stopped) return;
      if (!panel.isConnected || state.page !== "providers") { stop(); return; }
      renderCountdown();
      if (Date.now() >= expiryAt) finishExpired();
    }, 1000);
    pollTimer = window.setTimeout(poll, 3000);

    panel.append(heading, instructions, codeRow, code, actions, commandLine, panelMessage);
    return panel;
  }

  function renderProvider(provider) {
    const card = element("article", "resource-card");
    const header = element("div", "resource-header");
    const titleBlock = element("div", "resource-title");
    titleBlock.append(element("h4", "", provider.name));
    const badges = element("div", "badge-row");
    const statusLabel = providerStatusLabel(provider.status);
    badges.append(element("span", "tag", provider.type), element("span", `tag ${providerStatusTagClass(provider.status)}`, statusLabel));
    titleBlock.append(badges);
    const controls = element("div", "card-actions");
    const edit = button("Rename", "quiet small", () => {
      renameForm.hidden = !renameForm.hidden;
      if (!renameForm.hidden) renameInput.focus();
    });
    controls.append(edit, button("Delete", "danger quiet small", () => deleteProvider(provider)));
    header.append(titleBlock, controls);
    card.append(header);

    const info = element("p", "resource-copy", providerStatusDescription(provider.status));
    const connect = button(provider.status === "disconnected" ? "Connect with enrollment script" : "Reconnect with enrollment script", "primary small", async () => {
      connect.disabled = true;
      byId("provider-oauth-message").hidden = true;
      showMessage(byId("provider-oauth-message"));
      showMessage(actionMessage);
      // A previous panel's enrollment code is dead once a new one is issued.
      const existing = card.querySelector(".connect-panel");
      if (existing) existing.connectStop?.();
      existing?.remove();
      try {
        const enrollment = await api(`/api/providers/${encodeURIComponent(provider.id)}/connect/start`, {
          method: "POST", body: JSON.stringify({}),
        });
        card.append(buildConnectPanel(provider, enrollment));
      } catch (error) {
        showMessage(actionMessage, error.message, "error");
      } finally {
        connect.disabled = false;
      }
    });
    const actionMessage = element("p", "form-message");
    actionMessage.setAttribute("aria-live", "polite");
    card.append(info, connect);
    if (provider.status !== "disconnected") {
      const disconnect = button("Disconnect", "danger quiet small", async () => {
        disconnect.disabled = true;
        byId("provider-oauth-message").hidden = true;
        showMessage(byId("provider-oauth-message"));
        showMessage(actionMessage);
        try {
          const result = await api(`/api/providers/${encodeURIComponent(provider.id)}/disconnect`, {
            method: "POST", body: JSON.stringify({}),
          });
          await loadProviders();
          if (result?.revocationAttempted && !result.revocationConfirmed) {
            const notice = byId("provider-oauth-message");
            showMessage(notice, "Credentials were cleared locally, but OpenAI did not confirm remote sign-out. You can disconnect the app in ChatGPT Settings.", "error");
            notice.hidden = false;
          }
        } catch (error) {
          showMessage(actionMessage, error.message, "error");
        } finally {
          disconnect.disabled = false;
        }
      });
      card.append(disconnect);
    }
    actionMessage.setAttribute("aria-live", "polite");
    card.append(actionMessage);

    const renameForm = element("form", "inline-form");
    renameForm.hidden = true;
    const renameInput = element("input");
    renameInput.name = "name";
    renameInput.type = "text";
    renameInput.maxLength = 128;
    renameInput.required = true;
    renameInput.value = provider.name;
    renameInput.setAttribute("aria-label", `New name for ${provider.name}`);
    const renameSave = element("button", "button primary small", "Save name");
    renameSave.type = "submit";
    const renameCancel = button("Cancel", "quiet small", () => { renameForm.hidden = true; });
    const renameMessage = element("p", "form-message");
    renameMessage.setAttribute("aria-live", "polite");
    renameForm.append(renameInput, renameSave, renameCancel, renameMessage);
    renameForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      renameSave.disabled = true;
      showMessage(renameMessage);
      try {
        await api(`/api/providers/${encodeURIComponent(provider.id)}`, {
          method: "PUT", body: JSON.stringify({ name: renameInput.value }),
        });
        await loadProviders();
      } catch (error) {
        showMessage(renameMessage, error.message, "error");
      } finally {
        renameSave.disabled = false;
      }
    });
    card.append(renameForm);
    return card;
  }

  async function deleteProvider(provider) {
    if (!window.confirm(`Delete provider “${provider.name}”?`)) return;
    try {
      await api(`/api/providers/${encodeURIComponent(provider.id)}`, { method: "DELETE" });
      await loadProviders();
    } catch (error) {
      const card = [...byId("provider-list").children].find((item) => item.querySelector("h4")?.textContent === provider.name);
      const message = element("p", "inline-error", error.message);
      message.setAttribute("role", "alert");
      if (card) card.append(message);
    }
  }

  async function loadAccounts() {
    const list = byId("account-list");
    list.replaceChildren(element("p", "muted", "Loading service accounts…"));
    try {
      const [accounts, models, providers] = await Promise.all([
        api("/api/service-accounts"), api("/api/models"), api("/api/providers"),
      ]);
      state.accounts = accounts;
      state.models = models;
      state.providers = providers;
      renderAccounts();
    } catch (error) {
      list.replaceChildren(element("p", "empty-state", `Could not load service accounts: ${error.message}`));
    }
  }

  function renderAccounts() {
    const list = byId("account-list");
    list.replaceChildren();
    if (state.accounts.length === 0) {
      list.append(element("div", "empty-state", "No service accounts yet. Add one to issue a harness key."));
      return;
    }
    for (const account of state.accounts) list.append(renderAccount(account));
  }

  function renderAccount(account) {
    const card = element("article", "resource-card account-card");
    const header = element("div", "resource-header");
    const titleBlock = element("div", "resource-title");
    titleBlock.append(element("h4", "", account.name));
    titleBlock.append(element("span", `tag ${account.enabled ? "tag-success" : "tag-muted"}`, account.enabled ? "Enabled" : "Disabled"));
    const controls = element("div", "card-actions");
    controls.append(button(account.enabled ? "Disable" : "Enable", "quiet small", () => setAccountEnabled(account, !account.enabled)));
    controls.append(button("Delete", "danger quiet small", () => deleteAccount(account)));
    header.append(titleBlock, controls);
    card.append(header);

    const editToggle = button("Rename", "quiet small", () => {
      editForm.hidden = !editForm.hidden;
      if (!editForm.hidden) editInput.focus();
    });
    const editForm = element("form", "inline-form account-edit-form");
    editForm.hidden = true;
    const editInput = element("input");
    editInput.type = "text";
    editInput.name = "name";
    editInput.required = true;
    editInput.maxLength = 128;
    editInput.value = account.name;
    editInput.setAttribute("aria-label", `New name for ${account.name}`);
    const save = element("button", "button primary small", "Save name");
    save.type = "submit";
    const cancel = button("Cancel", "quiet small", () => { editForm.hidden = true; });
    const message = element("p", "form-message");
    message.setAttribute("aria-live", "polite");
    editForm.append(editInput, save, cancel, message);
    editForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      save.disabled = true;
      showMessage(message);
      try {
        await api(`/api/service-accounts/${encodeURIComponent(account.id)}`, {
          method: "PUT", body: JSON.stringify({ name: editInput.value, enabled: account.enabled }),
        });
        await loadAccounts();
      } catch (error) {
        showMessage(message, error.message, "error");
      } finally {
        save.disabled = false;
      }
    });
    const editRow = element("div", "edit-row");
    editRow.append(editToggle);
    card.append(editRow, editForm);

    const details = element("details", "account-details");
    const summary = element("summary", "", "Keys and Model access");
    const panel = element("div", "account-detail-content");
    panel.append(element("p", "muted", "Open to manage keys and Model grants."));
    details.append(summary, panel);
    details.addEventListener("toggle", () => {
      if (details.open && !details.loaded && !details.loading) loadAccountDetails(account, details, panel);
    });
    card.append(details);
    return card;
  }

  async function setAccountEnabled(account, enabled) {
    try {
      await api(`/api/service-accounts/${encodeURIComponent(account.id)}`, {
        method: "PUT", body: JSON.stringify({ name: account.name, enabled }),
      });
      await loadAccounts();
    } catch (error) {
      window.alert(error.message);
    }
  }

  async function deleteAccount(account) {
    if (!window.confirm(`Delete service account “${account.name}”? Its local keys and Model grants will also be removed.`)) return;
    try {
      await api(`/api/service-accounts/${encodeURIComponent(account.id)}`, { method: "DELETE" });
      await loadAccounts();
    } catch (error) {
      window.alert(error.message);
    }
  }

  async function loadUsers() {
    const list = byId("user-list");
    list.replaceChildren(element("p", "muted", "Loading users…"));
    try {
      const [users, me] = await Promise.all([api("/api/users"), api("/api/auth/me")]);
      state.users = users;
      state.currentUserID = me.id;
      renderUsers();
    } catch (error) {
      list.replaceChildren(element("p", "empty-state", `Could not load users: ${error.message}`));
    }
  }

  function renderUsers() {
    const list = byId("user-list");
    list.replaceChildren();
    for (const user of state.users) list.append(renderUser(user));
  }

  function renderUser(user) {
    const card = element("article", "resource-card");
    const header = element("div", "resource-header");
    const titleBlock = element("div", "resource-title");
    titleBlock.append(element("h4", "", user.username));
    const badges = element("div", "badge-row");
    const disabled = Boolean(user.disabledAt);
    badges.append(element("span", `tag ${disabled ? "tag-muted" : "tag-success"}`, disabled ? "Disabled" : "Active"));
    if (user.mustChangePassword) badges.append(element("span", "tag tag-muted", "Password change pending"));
    if (user.id === state.currentUserID) badges.append(element("span", "tag", "You"));
    titleBlock.append(badges);
    const controls = element("div", "card-actions");
    if (user.id !== state.currentUserID) {
      controls.append(button(disabled ? "Enable" : "Disable", "quiet small", () => setUserEnabled(user, !disabled)));
      controls.append(button("Reset password", "quiet small", () => resetUserPassword(user)));
      controls.append(button("Delete", "danger quiet small", () => deleteUser(user)));
    }
    header.append(titleBlock, controls);
    card.append(header);
    card.append(element("p", "resource-copy", `Created ${formatDate(user.createdAt)}`));
    return card;
  }

  async function setUserEnabled(user, disable) {
    try {
      await api(`/api/users/${encodeURIComponent(user.id)}/${disable ? "disable" : "enable"}`, {
        method: "POST", body: JSON.stringify({}),
      });
      await loadUsers();
    } catch (error) {
      window.alert(error.message);
    }
  }

  async function resetUserPassword(user) {
    const newPassword = window.prompt(`Set a new password for “${user.username}” (at least 12 characters). Their sessions are signed out and the password must be changed again at the next sign-in.`);
    if (newPassword === null) return;
    if (newPassword.length < 12) {
      window.alert("The new password must contain at least 12 characters.");
      return;
    }
    try {
      await api(`/api/users/${encodeURIComponent(user.id)}/password`, {
        method: "POST", body: JSON.stringify({ newPassword }),
      });
      await loadUsers();
    } catch (error) {
      window.alert(error.message);
    }
  }

  async function deleteUser(user) {
    if (!window.confirm(`Delete user “${user.username}”? Their sessions are signed out immediately.`)) return;
    try {
      await api(`/api/users/${encodeURIComponent(user.id)}`, { method: "DELETE" });
      await loadUsers();
    } catch (error) {
      window.alert(error.message);
    }
  }

  async function loadAccountDetails(account, details, panel) {
    if (details.loading || details.loaded) return;
    details.loading = true;
    panel.replaceChildren(element("p", "muted", "Loading keys and Model access…"));
    try {
      const [keys, grants] = await Promise.all([
        api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`),
        api(`/api/service-accounts/${encodeURIComponent(account.id)}/grants`),
      ]);
      details.loaded = true;
      renderAccountDetails(account, details, panel, keys, grants.modelIds);
    } catch (error) {
      const failure = element("p", "inline-error", `Could not load account details: ${error.message}`);
      const retry = button("Retry", "quiet small", () => loadAccountDetails(account, details, panel));
      panel.replaceChildren(failure, retry);
    } finally {
      details.loading = false;
    }
  }

  function renderAccountDetails(account, details, panel, keys, modelIds) {
    panel.replaceChildren();
    const keySection = element("section", "detail-section");
    const keyHeading = element("div", "detail-heading");
    keyHeading.append(element("h5", "", "Local API keys"), element("p", "muted", "A new key is shown once. Store it somewhere safe."));
    const issueButton = button("Issue new key", "primary small", async () => {
      issueButton.disabled = true;
      try {
        const issued = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`, {
          method: "POST", body: JSON.stringify({}),
        });
        keys = [issued.metadata, ...keys];
        renderAccountDetails(account, details, panel, keys, modelIds);
        showKeyOnce(panel, issued.key);
      } catch (error) {
        showMessage(keyMessage, error.message, "error");
      } finally {
        issueButton.disabled = false;
      }
    });
    keyHeading.append(issueButton);
    keySection.append(keyHeading);
    const keyMessage = element("p", "form-message");
    keyMessage.setAttribute("aria-live", "polite");
    keySection.append(keyMessage);
    if (keys.length === 0) {
      keySection.append(element("p", "empty-inline", "No keys have been issued."));
    } else {
      const keyList = element("ul", "key-list");
      for (const key of keys) {
        const row = element("li", "key-row");
        const keyInfo = element("div", "key-info");
        keyInfo.append(element("strong", "", key.displayHint));
        const revoked = Boolean(key.revokedAt);
        keyInfo.append(element("span", "muted", `${revoked ? "Revoked" : "Active"} · issued ${new Date(key.createdAt).toLocaleString()}`));
        row.append(keyInfo);
        if (!revoked) {
          row.append(button("Revoke", "danger quiet small", async () => {
            if (!window.confirm(`Revoke key ${key.displayHint}? New requests using this key will be denied.`)) return;
            try {
              await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys/${encodeURIComponent(key.id)}/revoke`, {
                method: "POST", body: JSON.stringify({}),
              });
              const refreshed = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`);
              renderAccountDetails(account, details, panel, refreshed, modelIds);
            } catch (error) {
              showMessage(keyMessage, error.message, "error");
            }
          }));
        }
        keyList.append(row);
      }
      keySection.append(keyList);
    }
    panel.append(keySection);

    const grantsSection = element("section", "detail-section grants-section");
    grantsSection.append(element("h5", "", "Permitted Models"));
    grantsSection.append(element("p", "muted", "Only selected Models are available to this account. New Models need an explicit grant."));
    if (state.models.length === 0) {
      grantsSection.append(element("p", "empty-inline", "Add a Model before granting access."));
    } else {
      const grantForm = element("form", "grant-form");
      const checkList = element("div", "grant-list");
      for (const model of state.models) {
        const label = element("label", "grant-option");
        const checkbox = element("input");
        checkbox.type = "checkbox";
        checkbox.name = "modelIds";
        checkbox.value = model.id;
        checkbox.checked = modelIds.includes(model.id);
        label.append(checkbox, element("span", "", model.name));
        label.append(element("small", "", `${providerName(model.providerId)} · ${model.upstreamModel || "target not set"}`));
        checkList.append(label);
      }
      const saveGrants = element("button", "button primary small", "Save Model access");
      saveGrants.type = "submit";
      const grantMessage = element("p", "form-message");
      grantMessage.setAttribute("aria-live", "polite");
      grantForm.append(checkList, saveGrants, grantMessage);
      grantForm.addEventListener("submit", async (event) => {
        event.preventDefault();
        saveGrants.disabled = true;
        showMessage(grantMessage);
        const selected = [...checkList.querySelectorAll('input[name="modelIds"]:checked')].map((input) => input.value);
        try {
          const result = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/grants`, {
            method: "PUT", body: JSON.stringify({ modelIds: selected }),
          });
          modelIds = result.modelIds;
          showMessage(grantMessage, "Model access saved.", "success");
        } catch (error) {
          showMessage(grantMessage, error.message, "error");
        } finally {
          saveGrants.disabled = false;
        }
      });
      grantsSection.append(grantForm);
    }
    panel.append(grantsSection);
    panel.append(renderPiSetupSection(account));
  }

  function renderPiSetupSection(account) {
    const section = element("section", "detail-section");
    const heading = element("div", "detail-heading");
    const headingCopy = element("div");
    headingCopy.append(element("h5", "", "Pi agent setup"));
    headingCopy.append(element("p", "muted", "Export the current grants for pi agent 1.0.0."));
    const content = element("div", "pi-setup-block");
    const loadButton = button("Check Pi setup", "quiet small", async () => {
      loadButton.disabled = true;
      content.replaceChildren(element("p", "muted", "Checking current account grants and Model metadata…"));
      try {
        const result = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/pi-config`);
        renderPiSetupResult(account, content, result);
      } catch (error) {
        content.replaceChildren(element("p", "inline-error", `Could not prepare Pi setup: ${error.message}`));
      } finally {
        loadButton.disabled = false;
      }
    });
    heading.append(headingCopy, loadButton);
    content.append(element("p", "muted", "Check setup to see which currently granted Models have complete Pi metadata."));
    section.append(heading, content);
    return section;
  }

  function renderPiSetupResult(account, content, result) {
    content.replaceChildren();
    if (!result.models.length) {
      content.append(element("p", "empty-inline", "This account currently has no Model grants."));
    } else {
      content.append(element("p", "muted", `${result.models.length} currently granted Model${result.models.length === 1 ? "" : "s"}; incomplete grants are shown below. Check again after changing grants or metadata.`));
      for (const model of result.models) {
        const row = element("div", "pi-model-readiness");
        row.append(element("strong", model.ready ? "pi-model-ready" : "pi-model-incomplete", `${model.name} · ${model.ready ? "ready" : "incomplete"}`));
        if (!model.ready) row.append(element("p", "muted", model.reasons.join(" · ")));
        content.append(row);
      }
    }

    if (!result.configuration) {
      content.append(element("p", "inline-error", result.configurationUnavailableReason || "No Pi configuration is available yet."));
      return;
    }

    const environmentName = result.apiKeyEnvironmentVariable;
    content.append(element("p", "muted", "Pi will read its API key from this environment variable. Issue a Gyemoim key above if needed, copy it when it is shown once, and set the variable in the environment used to launch pi:"));
    content.append(element("code", "pi-env-command", `export ${environmentName}='paste-the-one-time-issued-Gyemoim-key-here'`));
    content.append(element("p", "muted", "Download or copy this fragment, then merge its provider entry into the providers object in ~/.pi/agent/models.json. Preserve existing providers and other settings. Gyemoim does not write that file automatically. The provider baseUrl is built from the browser address you are using right now, so the fragment works through reverse proxies and remote access without server-side URL guessing. Check setup again after changing grants or metadata."));
    const withBaseURL = {
      ...result.configuration,
      providers: Object.fromEntries(Object.entries(result.configuration.providers).map(([id, provider]) => [
        id,
        { ...provider, baseUrl: `${window.location.origin}/v1` },
      ])),
    };
    const fragment = `${JSON.stringify(withBaseURL, null, 2)}\n`;
    const actions = element("div", "card-actions");
    const copyMessage = element("span", "muted", "");
    actions.append(button("Copy config fragment", "quiet small", async () => {
      try {
        await navigator.clipboard.writeText(fragment);
        copyMessage.textContent = "Config fragment copied.";
      } catch {
        copyMessage.textContent = "Clipboard access is unavailable. Select and copy the fragment below.";
      }
    }));
    actions.append(button("Download config fragment", "primary small", () => {
      const blob = new Blob([fragment], { type: "application/json" });
      const objectURL = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = objectURL;
      link.download = `pi-models-${account.id}.json`;
      link.click();
      window.setTimeout(() => URL.revokeObjectURL(objectURL), 0);
    }));
    content.append(actions, copyMessage, element("pre", "pi-config-fragment", fragment));
  }

  function showKeyOnce(panel, plaintext) {
    if (state.page !== "service-accounts" || !panel.isConnected) return;
    const reveal = element("section", "key-reveal");
    const warning = element("p", "key-warning", "Copy this key now. It cannot be viewed again after you dismiss this message.");
    warning.setAttribute("role", "alert");
    const secret = element("code", "key-value", plaintext);
    const actions = element("div", "card-actions");
    let value = plaintext;
    actions.append(button("Copy key", "primary small", async () => {
      try {
        await navigator.clipboard.writeText(value);
        copyMessage.textContent = "Copied to clipboard.";
      } catch {
        copyMessage.textContent = "Clipboard access is unavailable. Select and copy the key above.";
      }
    }));
    const clear = () => {
      value = "";
      secret.textContent = "";
      reveal.replaceChildren(element("p", "muted", "The one-time key has been cleared from this page."));
      reveal.classList.add("key-cleared");
      state.sensitiveCleanup.delete(clear);
    };
    state.sensitiveCleanup.add(clear);
    actions.append(button("Dismiss and clear", "quiet small", clear));
    const copyMessage = element("span", "muted", "");
    reveal.append(warning, secret, actions, copyMessage);
    panel.prepend(reveal);
    secret.tabIndex = 0;
  }

  function providerName(providerId) {
    return state.providers.find((provider) => provider.id === providerId)?.name || "Unknown provider";
  }

  async function loadModelsAndProviders() {
    const list = byId("model-list");
    list.replaceChildren(element("p", "muted", "Loading Models…"));
    try {
      const [models, providers] = await Promise.all([api("/api/models"), api("/api/providers")]);
      state.models = models;
      state.providers = providers;
      renderProviderOptions();
      renderModels();
    } catch (error) {
      list.replaceChildren(element("p", "empty-state", `Could not load Models: ${error.message}`));
    }
  }

  function renderProviderOptions() {
    const select = byId("model-provider");
    const selected = select.value;
    select.replaceChildren(new Option("Choose a provider", ""));
    for (const provider of state.providers) {
      const option = new Option(`${provider.name} · ${provider.status}`, provider.id);
      select.add(option);
    }
    if (state.providers.some((provider) => provider.id === selected)) select.value = selected;
    select.disabled = state.providers.length === 0;
    resetProviderModelCatalog();
  }

  function syncModelCatalogControl() {
    const provider = state.providers.find((item) => item.id === byId("model-provider").value);
    byId("model-catalog-load").disabled = provider?.status !== "connected";
  }

  function resetProviderModelCatalog() {
    state.catalogRequest += 1;
    byId("provider-model-catalog").replaceChildren();
    byId("model-catalog-load").textContent = "Load models";
    showMessage(byId("model-catalog-message"));
    syncModelCatalogControl();
  }

  async function loadProviderModelCatalog() {
    const providerId = byId("model-provider").value;
    const provider = state.providers.find((item) => item.id === providerId);
    const load = byId("model-catalog-load");
    const catalog = byId("provider-model-catalog");
    const message = byId("model-catalog-message");
    const requestId = ++state.catalogRequest;
    catalog.replaceChildren();
    showMessage(message);
    syncModelCatalogControl();
    if (!providerId || provider?.status !== "connected") {
      showMessage(message, "Connect the selected provider to load its account model list.", "error");
      return;
    }
    load.disabled = true;
    load.textContent = "Loading…";
    try {
      const models = await api(`/api/providers/${encodeURIComponent(providerId)}/models`);
      if (requestId !== state.catalogRequest || byId("model-provider").value !== providerId) return;
      for (const model of models) {
        const option = document.createElement("option");
        option.value = model.id;
        option.label = model.displayName;
        catalog.append(option);
      }
      showMessage(message, models.length ? `${models.length} models loaded for ${provider.name}.` : "No displayable models were returned for this account.", models.length ? "success" : "");
    } catch (error) {
      if (requestId !== state.catalogRequest || byId("model-provider").value !== providerId) return;
      showMessage(message, `${error.message} You can still enter an upstream model ID.`, "error");
    } finally {
      if (requestId === state.catalogRequest && byId("model-provider").value === providerId) {
        load.textContent = "Load models";
        syncModelCatalogControl();
      }
    }
  }

  function renderModels() {
    const list = byId("model-list");
    list.replaceChildren();
    if (state.models.length === 0) {
      list.append(element("div", "empty-state", "No Models configured. Add a provider, then create a Model route."));
      return;
    }
    for (const model of state.models) list.append(renderModel(model));
  }

  function renderModel(model) {
    const card = element("article", "resource-card model-card");
    const header = element("div", "resource-header");
    const titleBlock = element("div", "resource-title");
    titleBlock.append(element("h4", "", model.name));
    titleBlock.append(element("span", "tag tag-muted", `Revision ${model.version}`));
    const controls = element("div", "card-actions");
    controls.append(button("Edit", "quiet small", () => beginModelEdit(model)));
    controls.append(button("Delete", "danger quiet small", async () => {
      if (!window.confirm(`Delete Model “${model.name}”? Service account grants for this Model will be removed.`)) return;
      try {
        await api(`/api/models/${encodeURIComponent(model.id)}`, { method: "DELETE" });
        if (state.editingModel?.id === model.id) cancelModelEdit();
        await loadModelsAndProviders();
      } catch (error) {
        const message = element("p", "inline-error", error.message);
        message.setAttribute("role", "alert");
        card.append(message);
      }
    }));
    header.append(titleBlock, controls);
    card.append(header);
    const target = element("dl", "target-details");
    const provider = state.providers.find((item) => item.id === model.providerId);
    const providerLine = element("div");
    providerLine.append(element("dt", "", "Provider"), element("dd", "", provider?.name || "Unknown provider"));
    const upstreamLine = element("div");
    upstreamLine.append(element("dt", "", "Upstream model"), element("dd", "", model.upstreamModel || "Not set"));
    target.append(providerLine, upstreamLine);
    card.append(target);
    return card;
  }

  function fillModelMetadataEditor(metadata) {
    const source = metadata && typeof metadata === "object" && !Array.isArray(metadata) ? metadata : {};
    byId("model-context-window").value = source.contextWindow ?? "";
    byId("model-max-tokens").value = source.maxTokens ?? "";
    byId("model-reasoning").value = typeof source.reasoning === "boolean" ? String(source.reasoning) : "";
    const modalities = Array.isArray(source.input) ? source.input : [];
    byId("model-input-text").checked = modalities.includes("text");
    byId("model-input-image").checked = modalities.includes("image");
    const efforts = Array.isArray(source.supportedReasoningEfforts) ? source.supportedReasoningEfforts : [];
    document.querySelectorAll('input[name="model-effort"]').forEach((input) => {
      input.checked = efforts.includes(input.value);
    });
    const hasKnownMetadata = ["contextWindow", "maxTokens", "reasoning", "input", "supportedReasoningEfforts"].some((key) => Object.hasOwn(source, key));
    byId("model-metadata-editor").open = hasKnownMetadata;
  }

  function readModelMetadata(editing) {
    const knownFields = ["contextWindow", "maxTokens", "input", "reasoning", "supportedReasoningEfforts"];
    const metadata = editing?.metadata && typeof editing.metadata === "object" && !Array.isArray(editing.metadata)
      ? { ...editing.metadata }
      : {};
    for (const field of knownFields) delete metadata[field];

    for (const [key, id] of [["contextWindow", "model-context-window"], ["maxTokens", "model-max-tokens"]]) {
      const raw = byId(id).value.trim();
      if (!raw) continue;
      const value = Number(raw);
      if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${key} must be a positive whole number.`);
      metadata[key] = value;
    }
    const input = [];
    if (byId("model-input-text").checked) input.push("text");
    if (byId("model-input-image").checked) input.push("image");
    if (input.length) metadata.input = input;
    const reasoning = byId("model-reasoning").value;
    if (reasoning) metadata.reasoning = reasoning === "true";
    const efforts = [...document.querySelectorAll('input[name="model-effort"]:checked')].map((input) => input.value);
    if (efforts.length) metadata.supportedReasoningEfforts = efforts;
    return Object.keys(metadata).length ? metadata : undefined;
  }

  function beginModelEdit(model) {
    state.editingModel = model;
    byId("model-name").value = model.name;
    byId("model-provider").value = model.providerId || "";
    byId("model-upstream").value = model.upstreamModel || "";
    fillModelMetadataEditor(model.metadata);
    resetProviderModelCatalog();
    byId("model-form-heading").textContent = `Edit ${model.name}`;
    byId("model-submit").textContent = "Save changes";
    byId("model-cancel").hidden = false;
    byId("model-metadata-note").hidden = !model.metadata;
    showMessage(byId("model-form-message"));
    byId("model-name").focus();
    byId("model-form-heading").scrollIntoView({ block: "nearest", behavior: "smooth" });
  }

  function cancelModelEdit() {
    state.editingModel = null;
    byId("model-form").reset();
    byId("model-metadata-editor").open = false;
    byId("model-form-heading").textContent = "Add a Model";
    byId("model-submit").textContent = "Add Model";
    byId("model-cancel").hidden = true;
    byId("model-metadata-note").hidden = true;
    resetProviderModelCatalog();
    showMessage(byId("model-form-message"));
  }

  async function submitModel(event) {
    event.preventDefault();
    const form = event.currentTarget;
    const submit = byId("model-submit");
    const message = byId("model-form-message");
    const editing = state.editingModel;
    const payload = {
      name: byId("model-name").value,
      providerId: byId("model-provider").value,
      upstreamModel: byId("model-upstream").value,
    };
    try {
      const metadata = readModelMetadata(editing);
      if (metadata !== undefined) payload.metadata = metadata;
    } catch (error) {
      showMessage(message, error.message, "error");
      return;
    }
    submit.disabled = true;
    showMessage(message);
    try {
      const path = editing ? `/api/models/${encodeURIComponent(editing.id)}` : "/api/models";
      await api(path, { method: editing ? "PUT" : "POST", body: JSON.stringify(payload) });
      cancelModelEdit();
      await loadModelsAndProviders();
    } catch (error) {
      showMessage(message, error.message, "error");
    } finally {
      submit.disabled = false;
    }
  }

  function abortHistoryChildren() {
    for (const controller of historyState.childControllers) controller.abort();
    historyState.childControllers.clear();
  }

  function cancelHistoryFetches() {
    historyState.generation += 1;
    historyState.controller?.abort();
    historyState.controller = null;
    historyState.selectionToken = null;
    abortHistoryChildren();
  }

  function beginHistoryFetch() {
    historyState.controller?.abort();
    abortHistoryChildren();
    historyState.selectionToken = null;
    const controller = new AbortController();
    historyState.controller = controller;
    historyState.generation += 1;
    return { generation: historyState.generation, controller };
  }

  function beginHistoryChild(slot, parentToken) {
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

  function finishHistoryChild(child) {
    historyState.childControllers.delete(child.controller);
    child.parentToken.controller.signal.removeEventListener("abort", child.abortChild);
  }

  function historyFetchIsCurrent(token) {
    return token && token.generation === historyState.generation && !token.controller.signal.aborted;
  }

  function historyChildIsCurrent(child) {
    return historyFetchIsCurrent(child.parentToken) && !child.controller.signal.aborted && Number(child.slot.dataset.historySequence) === child.sequence;
  }

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

  function addQuery(path, values) {
    const url = new URL(path, window.location.origin);
    for (const [key, value] of Object.entries(values)) {
      if (value !== "" && value !== null && value !== undefined) url.searchParams.set(key, String(value));
    }
    return `${url.pathname}${url.search}`;
  }

  function formatNumber(value) {
    return Number.isFinite(Number(value)) ? Number(value).toLocaleString() : "Unknown";
  }

  function formatOffsetNS(value) {
    if (value === null || value === undefined || !Number.isFinite(Number(value))) return "Unknown (not recorded)";
    return `${(Number(value) / 1e6).toFixed(Number(value) % 1e6 === 0 ? 0 : 2)} ms`;
  }

  function formatDurationNS(value) {
    if (value === null || value === undefined || !Number.isFinite(Number(value))) return "Unknown";
    const ns = Number(value);
    if (ns < 1e6) return `${(ns / 1e3).toFixed(1)} µs`;
    if (ns < 1e9) return `${(ns / 1e6).toFixed(ns < 1e8 ? 1 : 0)} ms`;
    return `${(ns / 1e9).toFixed(ns < 6e10 ? 2 : 1)} s`;
  }

  function formatDate(value) {
    if (!value) return "Unknown";
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? String(value) : date.toLocaleString();
  }

  function identity(primary, id) {
    const label = primary || "Historical name unavailable";
    return { label, id: id || "Historical ID unavailable" };
  }

  function identityCell(primary, id) {
    const cell = element("span", "identity-cell");
    const info = identity(primary, id);
    cell.append(element("strong", "", info.label), element("small", "", info.id));
    return cell;
  }

  function outcomeTag(outcome) {
    const kind = outcome === "completed" ? "tag-success" : ["failed", "cancelled", "incomplete", "interrupted"].includes(outcome) ? "tag-muted" : "";
    return element("span", `tag ${kind}`, outcome || "unknown");
  }

  function historyErrorMessage(error) {
    if (error?.name === "AbortError") return "";
    return error?.message || "History could not be loaded.";
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
        state.pendingRequestID = request.requestId;
        navigate("requests");
      });
      const action = element("td");
      action.append(inspect);
      row.append(element("td", "", formatDate(request.startedAt)), identityCell(account.name, account.id), identityCell(model.name, model.id), element("td", "", request.outcome || "unknown"), element("td", "", formatDurationNS(request.durationNs)), element("td", "", formatOffsetNS(firstOutput)), action);
      rows.append(row);
    }
  }

  async function loadOverviewHistory() {
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

  async function loadRequestHistory() {
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
      if (state.pendingRequestID) {
        const pending = state.pendingRequestID;
        state.pendingRequestID = "";
        await selectRequest(pending);
      }
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

  byId("overview-group-by").addEventListener("change", () => {
    if (state.page === "overview") loadOverviewHistory();
  });
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

  byId("provider-create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const submit = form.querySelector('[type="submit"]');
    const message = byId("provider-create-message");
    submit.disabled = true;
    showMessage(message);
    try {
      await api("/api/providers", { method: "POST", body: JSON.stringify({ name: form.elements.name.value, type: "openai" }) });
      form.reset();
      showMessage(message, "Provider added. Sign in when ready.", "success");
      await loadProviders();
    } catch (error) {
      showMessage(message, error.message, "error");
    } finally {
      submit.disabled = false;
    }
  });

  byId("account-create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const submit = form.querySelector('[type="submit"]');
    const message = byId("account-create-message");
    submit.disabled = true;
    showMessage(message);
    try {
      await api("/api/service-accounts", { method: "POST", body: JSON.stringify({ name: form.elements.name.value, enabled: true }) });
      form.reset();
      showMessage(message, "Service account added. Grant Models and issue a key when ready.", "success");
      await loadAccounts();
    } catch (error) {
      showMessage(message, error.message, "error");
    } finally {
      submit.disabled = false;
    }
  });

  byId("model-form").addEventListener("submit", submitModel);
  byId("model-cancel").addEventListener("click", cancelModelEdit);
  byId("model-provider").addEventListener("change", resetProviderModelCatalog);
  byId("model-catalog-load").addEventListener("click", loadProviderModelCatalog);

  byId("user-create-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const submit = form.querySelector('[type="submit"]');
    const message = byId("user-create-message");
    submit.disabled = true;
    showMessage(message);
    try {
      await api("/api/users", { method: "POST", body: JSON.stringify({ username: form.elements.username.value, password: form.elements.password.value }) });
      form.reset();
      showMessage(message, "User added. They must change the temporary password at first sign-in.", "success");
      await loadUsers();
    } catch (error) {
      showMessage(message, error.message, "error");
    } finally {
      submit.disabled = false;
    }
  });

  byId("logout-button").addEventListener("click", async () => {
    try {
      await api("/api/auth/logout", { method: "POST", body: JSON.stringify({}) });
    } catch { /* Clearing the cookie matters more than the error. */ }
    window.location.assign("/login");
  });

  document.querySelectorAll("[data-page]").forEach((item) => item.addEventListener("click", () => navigate(item.dataset.page)));
  document.querySelectorAll("[data-refresh]").forEach((item) => item.addEventListener("click", () => refreshPage(item.dataset.refresh)));

  const oauthResult = new URLSearchParams(window.location.search).get("oauth_result");
  if (oauthResult) {
    const messages = {
      connected: ["OpenAI sign-in completed. Direct inference access is ready.", "success"],
      plan_usage_disabled: ["The account is linked, but direct inference access was not granted.", "error"],
      require_reauthentication: ["The saved refresh grant can no longer renew this account. Reconnect before using it.", "error"],
      authorization_denied: ["OpenAI sign-in was cancelled.", "error"],
      failed: ["OpenAI sign-in could not be completed. Reconnect and try again.", "error"],
    };
    const [message, kind] = Object.hasOwn(messages, oauthResult) ? messages[oauthResult] : messages.failed;
    const notice = byId("provider-oauth-message");
    showMessage(notice, message, kind);
    notice.hidden = false;
    window.history.replaceState({}, "", window.location.pathname);
    navigate("providers");
  } else {
    navigate("overview");
  }
})();
