// Service accounts page: account cards, the keys/grants detail (with the
// show-once key pattern and its leave-guard), the recent-rejections panel,
// the Pi setup export, and create/rename/enable/delete.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { pageLink } from "../nav.js";
import { formatUTC, rejectionCodeLabel } from "../format.js";
import { announceSuccess, clearMessageOnInput, scrollCardIntoView, withBusy } from "../feedback.js";
import { formErrorText } from "../errors.js";

// Success messages that must survive a full list re-render (rename: the fresh
// card is a new DOM subtree). renderAccount consumes and clears each entry.
const pendingCardMessages = new Map();

export async function loadAccounts() {
  const list = byId("account-list");
  list.replaceChildren(element("p", "muted", "Loading service accounts…"));
  // The rejections panel loads with the page but never auto-polls; it owns its
  // own error/retry state, so it runs detached from the accounts fetch.
  loadRejections();
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
    list.append(element("div", "empty-state", "No service accounts yet. Add one to create an API key for your agents."));
    return;
  }
  for (const account of state.accounts) list.append(renderAccount(account));
}

function renderAccount(account) {
  const card = element("article", "resource-card account-card");
  card.dataset.accountId = account.id;
  const header = element("div", "resource-header");
  const titleBlock = element("div", "resource-title");
  titleBlock.append(element("h4", "", account.name));
  titleBlock.append(element("span", `tag ${account.enabled ? "tag-success" : "tag-muted"}`, account.enabled ? "Enabled" : "Disabled"));

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
  const editMessage = element("p", "form-message");
  editMessage.setAttribute("aria-live", "polite");
  editForm.append(editInput, save, cancel, editMessage);
  const renameButton = button("Rename", "quiet small", () => {
    editForm.hidden = !editForm.hidden;
    if (!editForm.hidden) editInput.focus();
  });
  clearMessageOnInput(editForm, editMessage);
  editForm.addEventListener("submit", (event) => {
    event.preventDefault();
    withBusy(save, async () => {
      showMessage(editMessage);
      try {
        await api(`/api/service-accounts/${encodeURIComponent(account.id)}`, {
          method: "PUT", body: JSON.stringify({ name: editInput.value, enabled: account.enabled }),
        });
        pendingCardMessages.set(account.id, { text: "Name saved.", kind: "success" });
        await loadAccounts();
      } catch (error) {
        showMessage(editMessage, formErrorText(error, { kind: "service-account", action: "rename", name: editInput.value }), "error");
      }
    });
  });

  const toggleButton = button(account.enabled ? "Disable" : "Enable", "quiet small", () => {
    if (account.enabled && !window.confirm(`Disable “${account.name}”? Their keys stop working immediately. You can enable them again later.`)) return;
    withBusy(toggleButton, async () => {
      try {
        await api(`/api/service-accounts/${encodeURIComponent(account.id)}`, {
          method: "PUT", body: JSON.stringify({ name: account.name, enabled: !account.enabled }),
        });
        await loadAccounts();
      } catch (error) {
        window.alert(error.message);
      }
    });
  });

  const controls = element("div", "card-actions");
  controls.append(renameButton, toggleButton, button("Delete", "danger quiet small", () => deleteAccount(account)));
  header.append(titleBlock, controls);
  card.append(header);

  const cardMessage = element("p", "form-message");
  cardMessage.setAttribute("aria-live", "polite");
  card.append(cardMessage, editForm);

  const details = element("details", "account-details");
  const summary = element("summary", "", "Keys and Model access");
  const panel = element("div", "account-detail-content");
  panel.append(element("p", "muted", "Open to manage keys and Model grants."));
  details.append(summary, panel);
  details.addEventListener("toggle", () => {
    if (details.open && !details.loaded && !details.loading) loadAccountDetails(account, details, panel);
  });
  card.append(details);

  const stored = pendingCardMessages.get(account.id);
  if (stored) {
    pendingCardMessages.delete(account.id);
    showMessage(cardMessage, stored.text, stored.kind);
  }
  return card;
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

// The collapsed summary becomes stateful once the lazy detail load has real
// counts (review S3). It stays the plain label before the first load because
// the counts are only known after the lazy keys/grants fetch.
function updateAccountSummary(details, keys, modelIds) {
  const active = keys.filter((key) => !key.revokedAt).length;
  const total = state.models.length;
  details.querySelector("summary").textContent =
    `Keys and Model access — ${active} active key${active === 1 ? "" : "s"} · ${modelIds.length} of ${total} Model${total === 1 ? "" : "s"} granted`;
}

function renderAccountDetails(account, details, panel, keys, modelIds) {
  panel.replaceChildren();
  const keySection = element("section", "detail-section");
  const keyHeading = element("div", "detail-heading");
  const headingCopy = element("div");
  headingCopy.append(element("h5", "", "API keys"), element("p", "muted", "A new key is shown once. Store it somewhere safe."));
  const labelInput = element("input");
  labelInput.type = "text";
  labelInput.maxLength = 128;
  labelInput.placeholder = "Key label (optional)";
  labelInput.setAttribute("aria-label", "Key label (optional)");
  const keyMessage = element("p", "form-message");
  keyMessage.setAttribute("aria-live", "polite");
  const createKeyButton = button("Create key", "primary small", () => {
    withBusy(createKeyButton, async () => {
      const label = labelInput.value.trim();
      showMessage(keyMessage);
      try {
        const issued = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`, {
          method: "POST", body: label ? JSON.stringify({ label }) : JSON.stringify({}),
        });
        keys = [issued.metadata, ...keys];
        labelInput.value = "";
        const fresh = renderAccountDetails(account, details, panel, keys, modelIds);
        announceSuccess(fresh.keyMessage, "Key created. Copy it from the highlighted box now — it is shown only once.");
        scrollCardIntoView(panel.closest(".account-card"));
        showKeyOnce(panel, issued.key);
      } catch (error) {
        showMessage(keyMessage, formErrorText(error, { kind: "service-account", action: "create" }), "error");
      }
    });
  });
  const issueControls = element("div", "key-issue-controls");
  issueControls.append(labelInput, createKeyButton);
  keyHeading.append(headingCopy, issueControls);
  keySection.append(keyHeading, keyMessage);
  if (keys.length === 0) {
    keySection.append(element("p", "empty-inline", "No keys yet."));
  } else {
    const keyList = element("ul", "key-list");
    for (const key of keys) {
      const revoked = Boolean(key.revokedAt);
      const row = element("li", revoked ? "key-row key-row-revoked" : "key-row");
      const keyInfo = element("div", "key-info");
      if (key.label) {
        keyInfo.append(element("strong", "", key.label));
        keyInfo.append(element("span", "muted", `Key ending ${key.displayHint}`));
      } else {
        keyInfo.append(element("strong", "", key.displayHint));
      }
      keyInfo.append(element("span", "muted", `${revoked ? "Revoked" : "Active"} · issued ${new Date(key.createdAt).toLocaleString()} · ${key.lastUsedAt ? `Last used ${formatUTC(key.lastUsedAt)}` : "Never used"}`));
      row.append(keyInfo);
      if (!revoked) {
        const revokeButton = button("Revoke", "danger quiet small", () => {
          if (!window.confirm(`Revoke key ${key.displayHint}? New requests using this key will be denied.`)) return;
          withBusy(revokeButton, async () => {
            try {
              await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys/${encodeURIComponent(key.id)}/revoke`, {
                method: "POST", body: JSON.stringify({}),
              });
              const refreshed = await api(`/api/service-accounts/${encodeURIComponent(account.id)}/keys`);
              renderAccountDetails(account, details, panel, refreshed, modelIds);
            } catch (error) {
              showMessage(keyMessage, error.message, "error");
            }
          });
        });
        row.append(revokeButton);
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
    // Review S1: the empty state bridges to the other half of the key+grant
    // invariant instead of dead-ending.
    const empty = element("p", "empty-inline", "Add a Model before granting access. ");
    empty.append(pageLink("Go to Models", "models"));
    grantsSection.append(empty);
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
        updateAccountSummary(details, keys, modelIds);
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
  updateAccountSummary(details, keys, modelIds);
  return { keyMessage };
}

// Recent rejections (review S2): the pre-admission harness rejections ring
// (decision 9c), rendered newest-first. Loaded with the page, manual refresh
// only — deliberately no auto-polling, since these are diagnostics, not
// live traffic.
async function loadRejections() {
  const content = byId("rejections-content");
  content.replaceChildren(element("p", "muted", "Loading rejections…"));
  try {
    const rejections = await api("/api/rejections");
    renderRejections(rejections);
  } catch (error) {
    const failure = element("p", "inline-error", `Could not load rejections: ${error.message}`);
    failure.setAttribute("role", "alert");
    content.replaceChildren(failure, button("Retry", "quiet small", loadRejections));
  }
}

function renderRejections(rejections) {
  const content = byId("rejections-content");
  content.replaceChildren();
  if (!rejections.length) {
    content.append(element("p", "empty-state", "No rejected requests recorded. Rejections happen before request recording — a call with a bad key or missing grant shows here."));
    return;
  }
  const table = element("table", "data-table rejections-table");
  const head = element("thead");
  const headRow = element("tr");
  for (const label of ["Time (UTC)", "Key", "Code", "Model", "Service account"]) headRow.append(element("th", "", label));
  head.append(headRow);
  const body = element("tbody");
  for (const rejection of rejections) {
    const row = element("tr");
    row.append(element("td", "", formatUTC(rejection.at)));
    row.append(element("td", "", rejection.keyHint ? `Key ending ${rejection.keyHint}` : "Unknown key"));
    const code = element("td", "", rejectionCodeLabel(rejection.code));
    if (rejection.code && rejectionCodeLabel(rejection.code) !== rejection.code) code.title = rejection.code;
    row.append(code);
    row.append(element("td", "", rejection.model || "—"));
    const account = element("td");
    account.append(element("strong", "", rejection.serviceAccountName || "—"));
    if (rejection.serviceAccountName && rejection.serviceAccountId) account.append(element("small", "", rejection.serviceAccountId));
    row.append(account);
    body.append(row);
  }
  table.append(head, body);
  const scroll = element("div", "table-scroll");
  scroll.append(table);
  content.append(scroll);
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
  content.append(element("p", "muted", "Pi will read its API key from this environment variable. Create a Gyemoim key above if needed, copy it when it is shown once, and set the variable in the environment used to launch pi:"));
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

// Review S5: reloading or closing the tab with a revealed one-time key still
// on screen loses it forever, so the browser's own leave-guard fires. The
// in-app navigation guard lives in nav.js (it must run before the hash
// moves); both paths consult the same sensitiveCleanup machinery, so
// dismissing the key removes the guard.
window.addEventListener("beforeunload", (event) => {
  if (state.sensitiveCleanup.size > 0) {
    event.preventDefault();
    event.returnValue = "";
  }
});

function providerName(providerId) {
  return state.providers.find((provider) => provider.id === providerId)?.name || "Unknown provider";
}

const accountCreateForm = byId("account-create-form");
clearMessageOnInput(accountCreateForm, byId("account-create-message"));
accountCreateForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const submit = form.querySelector('[type="submit"]');
  const message = byId("account-create-message");
  withBusy(submit, async () => {
    showMessage(message);
    try {
      const created = await api("/api/service-accounts", { method: "POST", body: JSON.stringify({ name: form.elements.name.value, enabled: true }) });
      form.reset();
      announceSuccess(message, "Service account added. Grant Models and create a key when ready.");
      await loadAccounts();
      if (created?.id) scrollCardIntoView(byId("account-list").querySelector(`[data-account-id="${CSS.escape(created.id)}"]`));
    } catch (error) {
      showMessage(message, formErrorText(error, { kind: "service-account", action: "create", name: form.elements.name.value }), "error");
    }
  });
});

byId("rejections-refresh").addEventListener("click", loadRejections);
