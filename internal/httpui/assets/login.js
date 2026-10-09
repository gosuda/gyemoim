// Login and forced password-change pages. These forms cannot send custom
// headers natively, so submissions go through fetch, matching the main UI's
// api() helper.
(() => {
  "use strict";

  async function postJSON(path, body) {
    const response = await fetch(path, {
      method: "POST",
      headers: {
        "Accept": "application/json",
        "Content-Type": "application/json",
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

  // Review C1: failures must carry the .form-message.error styling from the
  // shared stylesheet, not the neutral gray default.
  const showMessage = (node, message, kind = "") => {
    node.textContent = message;
    node.className = kind ? `form-message ${kind}` : "form-message";
  };

  // C1: wrap the server's raw message in a consistent sentence ("Sign-in
  // failed: invalid credentials."), adding the period the server omits.
  const failedSentence = (prefix, detail) => `${prefix}${detail}${/[.!?…]$/.test(detail) ? "" : "."}`;

  const loginForm = document.getElementById("login-form");
  if (loginForm) {
    const message = document.getElementById("login-message");
    loginForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const submit = loginForm.querySelector('[type="submit"]');
      submit.disabled = true;
      showMessage(message, "Signing in…");
      try {
        const result = await postJSON("/api/auth/login", {
          username: document.getElementById("login-username").value,
          password: document.getElementById("login-password").value,
        });
        if (result.ok) {
          window.location.assign("/");
          return;
        }
        const detail = result.data?.error?.message || `request failed (${result.status})`;
        showMessage(message, failedSentence("Sign-in failed: ", detail), "error");
      } finally {
        submit.disabled = false;
      }
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
        showMessage(message, "New password entries do not match.", "error");
        return;
      }
      showMessage(message, "Setting the new password…");
      const result = await postJSON("/api/auth/password", {
        currentPassword: document.getElementById("change-current-password").value,
        newPassword,
      });
      if (result.ok) {
        window.location.assign("/");
        return;
      }
      // Review C2: a wrong current password is the expected failure here (the
      // temporary password mistyped), so it gets a recovery hint instead of
      // the generic server message.
      if (result.status === 401 && result.data?.error?.code === "invalid_credentials") {
        showMessage(message, "The current password is incorrect. Use the temporary password from your admin.", "error");
        return;
      }
      const detail = result.data?.error?.message || `request failed (${result.status})`;
      showMessage(message, failedSentence("Password change failed: ", detail), "error");
    });
  }

  // Review C3: the forced-change page must not be a trap — sign out clears
  // the session and lands on /login. The logout endpoint accepts stale
  // sessions, and landing on /login is correct even if the call fails.
  const signOut = document.getElementById("change-password-signout");
  if (signOut) {
    signOut.addEventListener("click", async () => {
      try {
        await postJSON("/api/auth/logout", {});
      } catch { /* The sign-in page is the right destination regardless. */ }
      window.location.assign("/login");
    });
  }
})();
