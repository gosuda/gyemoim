// Users page: user cards with the inline password-reset form and
// create/enable/disable/delete, plus the temporary-password handoff block
// (step 8, review U1–U6).
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formatDate, formatUTC } from "../format.js";
import { formErrorText } from "../errors.js";
import { announceSuccess, clearMessageOnInput, scrollCardIntoView, withBusy } from "../feedback.js";

// Step 8 (U2): reset-success messages must survive the loadUsers() re-render,
// because a re-render rebuilds every card (and its form) from scratch. The
// map is consumed once by buildPasswordResetForm while rendering the fresh
// card, which keeps the form open and announces the consequence there.
const pendingResetMessages = new Map();

export async function loadUsers() {
  const list = byId("user-list");
  const requestId = ++state.usersRequest;
  list.replaceChildren(element("p", "muted", "Loading users…"));
  try {
    const [users, me] = await Promise.all([api("/api/users"), api("/api/auth/me")]);
    if (requestId !== state.usersRequest) return;
    state.users = users;
    state.currentUserID = me.id;
    renderUsers();
  } catch (error) {
    if (requestId !== state.usersRequest) return;
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
  card.dataset.userId = user.id;
  const header = element("div", "resource-header");
  const titleBlock = element("div", "resource-title");
  titleBlock.append(element("h4", "", user.username));
  const badges = element("div", "badge-row");
  const disabled = Boolean(user.disabledAt);
  badges.append(element("span", `tag ${disabled ? "tag-muted" : "tag-success"}`, disabled ? "Disabled" : "Active"));
  if (user.mustChangePassword) badges.append(element("span", "tag tag-muted", "Must change password at next sign-in"));
  if (user.id === state.currentUserID) badges.append(element("span", "tag", "You"));
  titleBlock.append(badges);
  const controls = element("div", "card-actions");
  let resetForm = null;
  if (user.id !== state.currentUserID) {
    controls.append(button(disabled ? "Enable" : "Disable", "quiet small", () => setUserEnabled(user, !disabled)));
    resetForm = buildPasswordResetForm(user);
    controls.append(button("Reset password", "quiet small", () => {
      resetForm.hidden = !resetForm.hidden;
      if (!resetForm.hidden) resetForm.querySelector("input").focus();
    }));
    controls.append(button("Delete", "danger quiet small", () => deleteUser(user)));
  }
  header.append(titleBlock, controls);
  card.append(header);
  // U4/U6: created + last-sign-in share one muted line so the card reads as
  // one fact row; "Never signed in" covers the null lastLoginAt.
  card.append(element("p", "resource-copy",
    `Created ${formatDate(user.createdAt)} · ${user.lastLoginAt ? `Last signed in ${formatUTC(user.lastLoginAt)}` : "Never signed in"}`));
  if (user.id === state.currentUserID) {
    card.append(element("p", "muted", "You are signed in with this account."));
  }
  const handoff = state.userHandoffs.get(user.id);
  if (handoff) card.append(buildPasswordHandoff(user, handoff));
  if (resetForm) card.append(resetForm);
  return card;
}

// buildPasswordHandoff renders the one-time copyable handoff block for a
// freshly created user (U1), reusing the service-accounts show-once visual
// pattern (key-reveal/key-value). The password itself lives in
// state.userHandoffs keyed by user id, so the block survives list re-renders
// (Refresh, other mutations) until dismissed, the user is deleted, or the
// Users page is left (nav.js showPage clears the map).
function buildPasswordHandoff(user, password) {
  const block = element("section", "key-reveal");
  block.setAttribute("aria-label", `Temporary password for ${user.username}`);
  const heading = element("p", "key-warning", `Temporary password for ${user.username}:`);
  heading.setAttribute("role", "alert");
  const secret = element("code", "key-value", password);
  secret.tabIndex = 0;
  const row = element("div", "handoff-row");
  const copyMessage = element("span", "muted", "");
  row.append(secret, button("Copy", "primary small", async () => {
    try {
      await navigator.clipboard.writeText(secret.textContent);
      copyMessage.textContent = "Copied to clipboard.";
    } catch {
      copyMessage.textContent = "Clipboard access is unavailable. Select and copy the password above.";
    }
  }));
  const note = element("p", "muted", "Hand this to the user; they must change it at first sign-in.");
  const clear = () => {
    state.userHandoffs.delete(user.id);
    secret.textContent = "";
    block.replaceChildren(element("p", "muted", "The temporary password has been cleared from this page."));
    block.classList.add("key-cleared");
  };
  const actions = element("div", "card-actions");
  actions.append(button("Dismiss and clear", "quiet small", clear));
  block.append(heading, row, note, actions, copyMessage);
  return block;
}

// buildPasswordResetForm renders the expandable inline password reset form
// for one user card. Setting a password signs the user out everywhere and
// forces a password change at their next sign-in (U2: the form now says so
// before submission and stays open with the consequence after success).
function buildPasswordResetForm(user) {
  const form = element("form", "inline-form");
  form.hidden = true;
  const input = element("input");
  input.type = "password";
  input.name = "newPassword";
  input.autocomplete = "new-password";
  input.required = true;
  input.setAttribute("aria-label", `New password for ${user.username}`);
  const save = element("button", "button primary small", "Set password");
  save.type = "submit";
  const cancel = button("Cancel", "quiet small", () => {
    input.value = "";
    showMessage(message);
    form.hidden = true;
  });
  const hint = element("p", "muted inline-hint", "Signing them out everywhere; they must change it at next sign-in.");
  const message = element("p", "form-message");
  message.setAttribute("aria-live", "polite");
  form.append(input, save, cancel, hint, message);
  clearMessageOnInput(form, message);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    await withBusy(save, async () => {
      showMessage(message);
      try {
        await api(`/api/users/${encodeURIComponent(user.id)}/password`, {
          method: "POST", body: JSON.stringify({ newPassword: input.value }),
        });
        input.value = "";
        pendingResetMessages.set(user.id, "Password set. All their sessions were signed out; they must change it at next sign-in.");
        await loadUsers();
        scrollCardIntoView(findUserCard(user.id));
      } catch (error) {
        showMessage(message, error.message, "error");
      }
    });
  });
  // U2: a pending success re-opens the freshly rebuilt form with its message.
  const pending = pendingResetMessages.get(user.id);
  if (pending) {
    pendingResetMessages.delete(user.id);
    form.hidden = false;
    announceSuccess(message, pending);
  }
  return form;
}

function findUserCard(userId) {
  return document.querySelector(`[data-user-id="${CSS.escape(userId)}"]`);
}

async function setUserEnabled(user, disable) {
  // U5: disabling signs the user out everywhere, so it gets the same confirm
  // treatment as delete; enabling is safe and stays instant.
  if (disable && !window.confirm(`Disable “${user.username}”? Their sessions are signed out immediately.`)) return;
  try {
    await api(`/api/users/${encodeURIComponent(user.id)}/${disable ? "disable" : "enable"}`, {
      method: "POST", body: JSON.stringify({}),
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
    state.userHandoffs.delete(user.id);
    await loadUsers();
  } catch (error) {
    window.alert(error.message);
  }
}

const userCreateForm = byId("user-create-form");
const userCreateUsername = byId("user-create-username");
const userCreatePassword = byId("user-create-password");
const userCreateMessage = byId("user-create-message");
const userCreateSubmit = userCreateForm.querySelector('[type="submit"]');

// U6: a stale error must not survive a natively blocked (empty-field) submit,
// so the message clears as soon as the admin edits the form.
clearMessageOnInput(userCreateForm, userCreateMessage);
// U3: a duplicate-username 409 marks the username field until it is edited.
userCreateUsername.addEventListener("input", () => userCreateUsername.removeAttribute("aria-invalid"));

// U1: accessible show/hide for the temporary password, so the admin can read
// it back before handing it over. No inline JS (strict CSP); state is mirrored
// in aria-pressed and the accessible name.
const passwordToggle = byId("user-create-password-toggle");
passwordToggle.addEventListener("click", () => {
  const showing = userCreatePassword.type === "text";
  userCreatePassword.type = showing ? "password" : "text";
  passwordToggle.textContent = showing ? "Show" : "Hide";
  passwordToggle.setAttribute("aria-label", showing ? "Show password" : "Hide password");
  passwordToggle.setAttribute("aria-pressed", String(!showing));
  userCreatePassword.focus();
});

userCreateForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const username = userCreateUsername.value;
  const password = userCreatePassword.value;
  await withBusy(userCreateSubmit, async () => {
    showMessage(userCreateMessage);
    try {
      const user = await api("/api/users", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      // U1: record the handoff before anything clears the field, then clear
      // the form — the block on the new card is now the only copy.
      state.userHandoffs.set(user.id, password);
      userCreateUsername.removeAttribute("aria-invalid");
      userCreateForm.reset();
      announceSuccess(userCreateMessage, "User added. Hand over the temporary password shown on their card — they must change it at first sign-in.");
      await loadUsers();
      scrollCardIntoView(findUserCard(user.id));
    } catch (error) {
      const text = formErrorText(error, { kind: "user", action: "create", name: username });
      showMessage(userCreateMessage, text, "error");
      if (error.status === 409 && error.body?.error?.code === "conflict") {
        userCreateUsername.setAttribute("aria-invalid", "true");
        userCreateUsername.focus();
      }
    }
  });
});
