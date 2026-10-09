// Application entry point: logout wiring and boot. Loaded as a deferred ES
// module from index.html; every form/page wiring lives in the page modules.
import { api } from "./api.js";
import { byId, showMessage } from "./dom.js";
import { navigate, renderRoute } from "./nav.js";

byId("logout-button").addEventListener("click", async () => {
  try {
    await api("/api/auth/logout", { method: "POST", body: JSON.stringify({}) });
  } catch { /* Clearing the cookie matters more than the error. */ }
  window.location.assign("/login");
});

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
  renderRoute();
}
window.addEventListener("hashchange", renderRoute);
