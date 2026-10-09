// Users page: user cards with the inline password-reset form and
// create/enable/disable/delete.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formatDate } from "../format.js";

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
  card.append(element("p", "resource-copy", `Created ${formatDate(user.createdAt)}`));
  if (resetForm) card.append(resetForm);
  return card;
}

// buildPasswordResetForm renders the expandable inline password reset form
// for one user card. Setting a password signs the user out everywhere and
// forces a password change at their next sign-in.
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
  const message = element("p", "form-message");
  message.setAttribute("aria-live", "polite");
  form.append(input, save, cancel, message);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    save.disabled = true;
    showMessage(message);
    try {
      await api(`/api/users/${encodeURIComponent(user.id)}/password`, {
        method: "POST", body: JSON.stringify({ newPassword: input.value }),
      });
      await loadUsers();
    } catch (error) {
      showMessage(message, error.message, "error");
    } finally {
      save.disabled = false;
    }
  });
  return form;
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

async function deleteUser(user) {
  if (!window.confirm(`Delete user “${user.username}”? Their sessions are signed out immediately.`)) return;
  try {
    await api(`/api/users/${encodeURIComponent(user.id)}`, { method: "DELETE" });
    await loadUsers();
  } catch (error) {
    window.alert(error.message);
  }
}

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
