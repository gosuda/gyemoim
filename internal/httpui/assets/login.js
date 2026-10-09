// Login and forced password-change pages. These forms cannot send custom
// headers natively, so submissions go through fetch with the CSRF token that
// the server injected into the page, matching the main UI's api() helper.
(() => {
  "use strict";

  const csrfToken = document.querySelector('meta[name="csrf-token"]')?.content ?? "";

  async function postJSON(path, body) {
    const response = await fetch(path, {
      method: "POST",
      headers: {
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-Gyemoim-CSRF": csrfToken,
      },
      credentials: "same-origin",
      cache: "no-store",
      body: JSON.stringify(body),
    });
    const text = await response.text();
    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch { /* Fall back to the status code. */ }
    }
    return { ok: response.ok, status: response.status, data };
  }

  const showMessage = (node, message) => { node.textContent = message; };

  const loginForm = document.getElementById("login-form");
  if (loginForm) {
    const message = document.getElementById("login-message");
    loginForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      showMessage(message, "Signing in…");
      const result = await postJSON("/api/auth/login", {
        username: document.getElementById("login-username").value,
        password: document.getElementById("login-password").value,
      });
      if (result.ok) {
        window.location.assign("/");
        return;
      }
      showMessage(message, result.data?.error?.message || `Sign-in failed (${result.status})`);
    });
  }

  const changeForm = document.getElementById("change-password-form");
  if (changeForm) {
    const label = document.getElementById("change-password-user");
    // /api/auth/me stays reachable while the forced change is pending, so the
    // page can show which user is signed in.
    fetch("/api/auth/me", { headers: { Accept: "application/json" }, credentials: "same-origin", cache: "no-store" })
      .then(async (response) => {
        if (!response.ok) return;
        const me = await response.json();
        if (label && me?.username) {
          label.textContent = `Signed in as ${me.username}.`;
          label.hidden = false;
        }
      })
      .catch(() => { /* The username line simply stays hidden. */ });
    const message = document.getElementById("change-password-message");
    changeForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const newPassword = document.getElementById("change-new-password").value;
      if (newPassword !== document.getElementById("change-confirm-password").value) {
        showMessage(message, "New password entries do not match.");
        return;
      }
      showMessage(message, "Changing password…");
      const result = await postJSON("/api/auth/password", {
        currentPassword: document.getElementById("change-current-password").value,
        newPassword,
      });
      if (result.ok) {
        window.location.assign("/");
        return;
      }
      showMessage(message, result.data?.error?.message || `Password change failed (${result.status})`);
    });
  }
})();
