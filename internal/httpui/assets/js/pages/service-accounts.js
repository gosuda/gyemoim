// Service accounts page: account cards, the keys/grants detail (with the
// show-once key pattern), the Pi setup export, and create/rename/enable/delete.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";

export async function loadAccounts() {
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
      if (key.label) {
        keyInfo.append(element("strong", "", key.label));
        keyInfo.append(element("span", "muted", `Key ending ${key.displayHint}`));
      } else {
        keyInfo.append(element("strong", "", key.displayHint));
      }
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
