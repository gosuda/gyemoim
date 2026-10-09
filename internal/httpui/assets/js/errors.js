// Error humanization (overhaul decision 3): translates a small, known set of
// generic management-API responses into labeled, actionable sentences.
//
// The mapping is keyed by status + error code (plus message patterns for the
// JSON-field leaks); the caller supplies context ({kind, name, action}) so one
// table serves create/rename/delete across every resource kind.
//
// Explicit-errors invariant: only known responses are mapped. Everything else
// returns null (or passes through verbatim in formErrorText) — never swallow,
// never auto-retry, never mask a 5xx.

// Human labels for the resource kinds callers may pass as context.kind. Model
// keeps the capital M (overhaul decision 7: capital-M Model is the route alias).
const kindLabels = {
  provider: "provider",
  "service-account": "service account",
  model: "Model",
  user: "user",
};

// 409 "conflict" sentences for the actions where the server's conflict is
// always a name collision. The service account uses the same sentence shape
// as providers; a user "name" is a username, so its sentence says taken.
const nameConflictSentences = {
  provider: (name) => `A provider named “${name}” already exists. Choose another name.`,
  "service-account": (name) => `A service account named “${name}” already exists. Choose another name.`,
  model: (name) => `A Model named “${name}” already exists. Choose another name.`,
  user: (name) => `Username “${name}” is already taken.`,
};

// JSON-field leaks: patterns over error.message mapping the server's field
// names to the labels the forms actually show. The trailing period and the
// "metadata." prefix are optional because both the server and client-side
// validators phrase these slightly differently.
const fieldMessagePatterns = [
  {
    pattern: /^providerId and upstreamModel are required\.?$/,
    sentence: () => "Choose a provider and an upstream model.",
  },
  {
    pattern: /^(?:metadata\.)?(contextWindow|maxTokens) must be a positive (?:whole number|integer)\.?$/,
    sentence: (field) => `${field === "contextWindow" ? "Context window" : "Max tokens"} must be a positive whole number of tokens.`,
  },
];

// humanizeApiError maps one failed management-API response to a human
// sentence, or returns null when the response is not a known offender.
// body is the parsed JSON envelope ({error: {message, code}}) or null.
export function humanizeApiError(status, body, context = {}) {
  const code = body?.error?.code || "";
  const message = body?.error?.message || "";
  if (status === 409 && code === "conflict") {
    if (context.action === "delete") {
      const label = kindLabels[context.kind];
      // The server does not say what references the resource, so the sentence
      // stays honest and generic instead of guessing the referencing kind.
      return label ? `This ${label} is still in use. Remove the items that point to it first.` : null;
    }
    const sentence = nameConflictSentences[context.kind];
    const name = typeof context.name === "string" ? context.name.trim() : "";
    return sentence && name ? sentence(name) : null;
  }
  if (status === 400) {
    for (const { pattern, sentence } of fieldMessagePatterns) {
      const match = message.match(pattern);
      if (match) return sentence(match[1]);
    }
  }
  return null;
}

// Convenience wrapper for form handlers: given the Error thrown by api()
// (which carries .status and .body) return the humanized sentence when the
// error is a known one, otherwise today's server message verbatim.
export function formErrorText(error, context) {
  const humanized = error && typeof error.status === "number"
    ? humanizeApiError(error.status, error.body, context)
    : null;
  return humanized ?? (error?.message || "Request failed.");
}
