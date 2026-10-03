(() => {
  "use strict";

  const csrfToken = document.querySelector('meta[name="csrf-token"]')?.content ?? "";
  const state = {
    page: "overview",
    providers: [],
    accounts: [],
    models: [],
    editingModel: null,
    sensitiveCleanup: new Set(),
  };
  const titles = {
    overview: "Overview",
    providers: "Providers",
    "service-accounts": "Service accounts",
    models: "Models",
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
      const message = data?.error?.message || `Request failed (${response.status})`;
      throw new Error(message);
    }
    return data;
  }

  function navigate(page) {
    if (!titles[page]) return;
    if (state.page === "service-accounts" && page !== state.page) {
      for (const clear of [...state.sensitiveCleanup]) clear();
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
    if (page === "overview") return loadStatus();
    if (page === "providers") return loadProviders();
    if (page === "service-accounts") return loadAccounts();
    if (page === "models") return loadModelsAndProviders();
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

  function renderProvider(provider) {
    const card = element("article", "resource-card");
    const header = element("div", "resource-header");
    const titleBlock = element("div", "resource-title");
    titleBlock.append(element("h4", "", provider.name));
    const badges = element("div", "badge-row");
    badges.append(element("span", "tag", provider.type), element("span", `tag ${provider.status === "connected" ? "tag-success" : "tag-muted"}`, provider.status));
    titleBlock.append(badges);
    const controls = element("div", "card-actions");
    const edit = button("Rename", "quiet small", () => {
      renameForm.hidden = !renameForm.hidden;
      if (!renameForm.hidden) renameInput.focus();
    });
    controls.append(edit, button("Delete", "danger quiet small", () => deleteProvider(provider)));
    header.append(titleBlock, controls);
    card.append(header);

    const info = element("p", "resource-copy", "OpenAI account sign-in is unavailable in this build.");
    card.append(info, button("Connect OpenAI account · coming later", "quiet small pending-button"));
    card.lastChild.disabled = true;
    card.lastChild.setAttribute("aria-label", "Connect OpenAI account, OAuth support is coming later");

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

  function beginModelEdit(model) {
    state.editingModel = model;
    byId("model-name").value = model.name;
    byId("model-provider").value = model.providerId || "";
    byId("model-upstream").value = model.upstreamModel || "";
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
    byId("model-form-heading").textContent = "Add a Model";
    byId("model-submit").textContent = "Add Model";
    byId("model-cancel").hidden = true;
    byId("model-metadata-note").hidden = true;
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
    // The API replaces all Model fields on update. Keep metadata intact until its editor is added.
    if (editing?.metadata !== undefined) payload.metadata = editing.metadata;
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
      showMessage(message, "Provider added. OAuth connection is not available yet.", "success");
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
  document.querySelectorAll("[data-page]").forEach((item) => item.addEventListener("click", () => navigate(item.dataset.page)));
  document.querySelectorAll("[data-refresh]").forEach((item) => item.addEventListener("click", () => refreshPage(item.dataset.refresh)));

  navigate("overview");
})();
