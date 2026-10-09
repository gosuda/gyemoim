// Shared feedback helpers (overhaul decision 4): busy states, success
// messages, clear-on-input, and the scroll+highlight used when a new or
// updated card should announce itself. The message slots themselves stay
// owned by the pages; these helpers only fill and clear them.
import { showMessage } from "./dom.js";

// withBusy disables the button for the duration of an async action and
// restores it afterwards (in finally), so a double submit cannot fire twice
// while keeping aria-busy honest. Resolves with fn's result, rethrows fn's
// error after the button is back.
export async function withBusy(buttonEl, fn) {
  if (buttonEl) {
    buttonEl.disabled = true;
    buttonEl.setAttribute("aria-busy", "true");
  }
  try {
    return await fn();
  } finally {
    if (buttonEl) {
      buttonEl.disabled = false;
      buttonEl.removeAttribute("aria-busy");
    }
  }
}

// announceSuccess puts a success message into an existing form-message slot.
// showMessage already supports the "success" kind (styled by
// .form-message.success); slots that were not given an aria-live attribute in
// the markup get one here so screen readers announce the outcome.
export function announceSuccess(messageEl, text) {
  if (!messageEl) return;
  if (!messageEl.hasAttribute("aria-live")) messageEl.setAttribute("aria-live", "polite");
  showMessage(messageEl, text, "success");
}

// clearMessageOnInput wires the form so a stale error/success message clears
// as soon as the user edits anything in it (decision 4: messages clear on
// input). It only ever clears — it never writes content.
export function clearMessageOnInput(form, messageEl) {
  if (!form || !messageEl) return;
  const clear = () => {
    if (messageEl.textContent) showMessage(messageEl);
  };
  form.addEventListener("input", clear);
  form.addEventListener("change", clear);
}

const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)");

// scrollCardIntoView brings a card into view and gives it a brief highlight
// so the updated element announces itself (decision 4). With
// prefers-reduced-motion the scroll is instant and the CSS falls back to a
// static, non-animated outline.
export function scrollCardIntoView(cardEl) {
  if (!cardEl) return;
  const reduced = Boolean(reduceMotion?.matches);
  cardEl.scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "nearest" });
  cardEl.classList.remove("card-highlight");
  // Force a reflow so a repeated call restarts the highlight from the start.
  void cardEl.offsetWidth;
  cardEl.classList.add("card-highlight");
  window.setTimeout(() => cardEl.classList.remove("card-highlight"), reduced ? 1200 : 1500);
}
