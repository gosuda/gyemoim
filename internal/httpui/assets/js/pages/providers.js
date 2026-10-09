// Providers page: provider cards, the enrollment (connect) panel with its
// countdown and poll, create/rename/delete.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formErrorText } from "../errors.js";
import { announceSuccess, clearMessageOnInput, scrollCardIntoView, withBusy } from "../feedback.js";
import { providerStatusLabel, providerStatusTagClass } from "../format.js";

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

// findProviderCard locates a provider's card after a list re-render (cards
// carry their provider id as a data attribute) so outcomes and confirmations
// can be placed on the fresh card.
function findProviderCard(providerID) {
  return byId("provider-list").querySelector(`.resource-card[data-provider-id="${CSS.escape(providerID)}"]`);
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

function providerStatusDescription(status) {
  if (status === "connected") return "This OpenAI account is connected and ready for direct inference.";
  if (status === "plan_usage_disabled") return "Plan usage disabled — enable API access on the ChatGPT plan, then reconnect this account.";
  if (status === "require_reauthentication") return "Re-authentication required. The saved refresh grant can no longer renew access; reconnect before using this account.";
  if (status === "failed") return "The last connection attempt failed. Start the connect flow again.";
  return "Connect an OpenAI account with Sign in with ChatGPT.";
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
    let finished = null;
    if (card) {
      finished = renderProvider(updated);
      card.replaceWith(finished);
    }
    const kind = updated.status === "connected" ? "success" : "error";
    const text = updated.status === "connected"
      ? "OpenAI account connected. Direct inference access is ready."
      : (() => {
          const label = providerStatusLabel(updated.status);
          const description = providerStatusDescription(updated.status);
          return `Connection attempt finished: ${description.startsWith(label) ? description : `${label}. ${description}`}`;
        })();
    const notice = byId("provider-oauth-message");
    notice.hidden = false;
    showMessage(notice, text, kind);
    // The page-level notice sits above the card grid and is off-screen for
    // cards at the end of the list, so the outcome also lands on the
    // finished card's own message and the card scrolls into view.
    if (finished) {
      showMessage(finished.providerMessage, text, kind);
      scrollCardIntoView(finished);
    }
  };
  const finishExpired = () => {
    if (stopped) return;
    stop();
    // A dead code must not invite copying: dim it and disable the copy
    // affordance, keeping the recovery message.
    code.classList.add("connect-code-expired");
    code.setAttribute("aria-disabled", "true");
    copyCommand.disabled = true;
    showMessage(panelMessage, "The enrollment code expired without a completed connection. Close this panel and run Connect again to issue a new code.", "error");
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
  const instructions = element("p", "muted", "Run the script on the machine with your web browser — where you sign in to ChatGPT. The script opens the browser; after signing in, return here and this card updates automatically. Requires python3.");
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
  const copyCommand = button("Copy command", "quiet small", async () => {
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
  });
  actions.append(
    download,
    copyCommand,
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
  showMessage(panelMessage, "Waiting for the sign-in to finish… If the script printed an error in your terminal, close this panel and run Connect again — nothing is saved until the flow completes.");

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
  card.dataset.providerId = provider.id;
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
    // An open panel means a code was already issued (live or recently
    // expired). Clicking again must reveal that panel instead of silently
    // invalidating its code with a new issue — there is deliberately no
    // reissue path from this button; the panel keeps its own Close, after
    // which Connect can issue a fresh code.
    const existing = card.querySelector(".connect-panel");
    if (existing) {
      scrollCardIntoView(card);
      return;
    }
    byId("provider-oauth-message").hidden = true;
    showMessage(byId("provider-oauth-message"));
    showMessage(actionMessage);
    await withBusy(connect, async () => {
      const enrollment = await api(`/api/providers/${encodeURIComponent(provider.id)}/connect/start`, {
        method: "POST", body: JSON.stringify({}),
      });
      card.append(buildConnectPanel(provider, enrollment));
    }).catch((error) => {
      showMessage(actionMessage, error.message, "error");
    });
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
  // The re-rendered replacement card (finishWithStatus, rename) exposes its
  // message slot so outcomes can be placed on the fresh card.
  card.providerMessage = actionMessage;
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
  clearMessageOnInput(renameForm, renameMessage);
  renameForm.addEventListener("submit", (event) => {
    event.preventDefault();
    withBusy(renameSave, async () => {
      showMessage(renameMessage);
      await api(`/api/providers/${encodeURIComponent(provider.id)}`, {
        method: "PUT", body: JSON.stringify({ name: renameInput.value }),
      });
      await loadProviders();
      // The card was rebuilt by the reload, so the confirmation lands on the
      // fresh card's message slot.
      const fresh = findProviderCard(provider.id);
      if (fresh?.providerMessage) announceSuccess(fresh.providerMessage, "Name saved.");
    }).catch((error) => {
      showMessage(renameMessage, formErrorText(error, { kind: "provider", action: "rename", name: renameInput.value }), "error");
    });
  });
  card.append(renameForm);
  return card;
}

async function deleteProvider(provider) {
  if (!window.confirm(`Delete provider “${provider.name}”? Models targeting it must be removed first.`)) return;
  try {
    await api(`/api/providers/${encodeURIComponent(provider.id)}`, { method: "DELETE" });
    await loadProviders();
  } catch (error) {
    const card = findProviderCard(provider.id);
    const message = element("p", "inline-error", formErrorText(error, { kind: "provider", action: "delete", name: provider.name }));
    message.setAttribute("role", "alert");
    if (card) card.append(message);
  }
}

// Provider create form (step-6): withBusy + a message slot cleared on input,
// success announcing the next step, and errors humanized through errors.js.
const providerCreateForm = byId("provider-create-form");
const providerCreateMessage = byId("provider-create-message");
clearMessageOnInput(providerCreateForm, providerCreateMessage);
providerCreateForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const submit = form.querySelector('[type="submit"]');
  withBusy(submit, async () => {
    showMessage(providerCreateMessage);
    const created = await api("/api/providers", { method: "POST", body: JSON.stringify({ name: form.elements.name.value, type: "openai" }) });
    form.reset();
    announceSuccess(providerCreateMessage, "Provider added. Open its card and run the enrollment script to connect — Models can target it once it shows Connected.");
    await loadProviders();
    // The new card is at the end of the list; bring it into view.
    scrollCardIntoView(findProviderCard(created?.id));
  }).catch((error) => {
    showMessage(providerCreateMessage, formErrorText(error, { kind: "provider", action: "create", name: form.elements.name.value }), "error");
  });
});
