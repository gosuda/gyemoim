// Providers page: provider cards, the enrollment (connect) panel with its
// countdown and poll, create/rename/delete.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formErrorText } from "../errors.js";
import { announceSuccess, clearMessageOnInput, withBusy } from "../feedback.js";

export async function loadProviders() {
  const list = byId("provider-list");
  // Snapshot the open enrollment panels before the list DOM is replaced so
  // they can be re-attached after the refresh.
  const open = collectConnectPanels(list);
  list.replaceChildren(element("p", "muted", "Loading providers…"));
  try {
    state.providers = await api("/api/providers");
    renderProviders(open);
  } catch (error) {
    list.replaceChildren(element("p", "empty-state", `Could not load providers: ${error.message}`));
  }
}

// collectConnectPanels remembers each open enrollment panel's provider and
// remaining code lifetime so renderProviders can rebuild the panels on the
// fresh cards.
function collectConnectPanels(list) {
  const open = new Map();
  list.querySelectorAll(".connect-panel").forEach((panel) => {
    const data = panel.connectData;
    panel.connectStop?.();
    if (data && data.expiresAt > Date.now()) open.set(data.providerID, data);
  });
  return open;
}

function renderProviders(open = new Map()) {
  const list = byId("provider-list");
  list.replaceChildren();
  if (state.providers.length === 0) {
    list.append(element("div", "empty-state", "No providers yet. Add one to prepare a Model route."));
    return;
  }
  for (const provider of state.providers) {
    const card = renderProvider(provider);
    const data = open.get(provider.id);
    if (data) {
      card.append(buildConnectPanel(provider, {
        ...data.enrollment,
        expiresInSeconds: Math.ceil((data.expiresAt - Date.now()) / 1000),
      }));
    }
    list.append(card);
  }
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
  panel.connectData = { providerID: provider.id, enrollment, expiresAt: expiryAt };

  // finishWithStatus swaps out only this provider's card so that other
  // open enrollment panels keep their codes and polls; a full re-render
  // here would silently destroy them.
  const finishWithStatus = (updated) => {
    const card = panel.closest(".resource-card");
    stop();
    panel.remove();
    if (card) card.replaceWith(renderProvider(updated));
    const notice = byId("provider-oauth-message");
    notice.hidden = false;
    if (updated.status === "connected") {
      showMessage(notice, "OpenAI account connected. Direct inference access is ready.", "success");
    } else {
      const label = providerStatusLabel(updated.status);
      const description = providerStatusDescription(updated.status);
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
      if (Array.isArray(providers)) state.providers = providers;
      const current = Array.isArray(providers) ? providers.find((item) => item.id === provider.id) : null;
      if (current && current.status !== provider.status) {
        finishWithStatus(current);
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
  const countdown = element("span", "muted", "");
  codeRow.append(element("strong", "connect-label", "Enrollment code (single use)"), countdown);
  const code = element("code", "connect-code", enrollment.code);
  code.tabIndex = 0;

  const rawCommand = String(enrollment.command || "");
  const command = rawCommand.includes("SERVER_URL")
    ? rawCommand.replaceAll("SERVER_URL", window.location.origin)
    : "";
  const commandLine = element("pre", "connect-command inline-error", command
    || "The enrollment command is unavailable: the script template is missing its SERVER_URL placeholder. Download the script and follow its instructions.");
  const actions = element("div", "card-actions");
  const download = element("a", "button primary small", "Download script");
  download.href = enrollment.scriptUrl || "/api/connect/script";
  download.download = "gyemoim-connect.py";
  const copyMessage = element("span", "muted", "");
  actions.append(
    download,
    button("Copy command", "quiet small", async () => {
      if (!command) {
        copyMessage.textContent = "No enrollment command is available to copy.";
        return;
      }
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

// Representative application of the step-5 shared layer (duplicate-name 409
// humanization + withBusy). Other forms keep today's behavior until their own
// page steps wire them.
const providerCreateForm = byId("provider-create-form");
const providerCreateMessage = byId("provider-create-message");
clearMessageOnInput(providerCreateForm, providerCreateMessage);
providerCreateForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const submit = form.querySelector('[type="submit"]');
  withBusy(submit, async () => {
    showMessage(providerCreateMessage);
    await api("/api/providers", { method: "POST", body: JSON.stringify({ name: form.elements.name.value, type: "openai" }) });
    form.reset();
    announceSuccess(providerCreateMessage, "Provider added. Sign in when ready.");
    await loadProviders();
  }).catch((error) => {
    showMessage(providerCreateMessage, formErrorText(error, { kind: "provider", action: "create", name: form.elements.name.value }), "error");
  });
});
