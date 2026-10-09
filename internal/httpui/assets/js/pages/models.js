// Models page: the create/edit form (with provider dropdown and upstream
// catalog loader) and the Model route cards.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";

export async function loadModelsAndProviders() {
  const list = byId("model-list");
  list.replaceChildren(element("p", "muted", "Loading Models…"));
  try {
    const [models, providers] = await Promise.all([api("/api/models"), api("/api/providers")]);
    state.models = models;
    state.providers = providers;
    renderProviderOptions();
    renderModels();
  } catch (error) {
    list.replaceChildren(element("p", "empty-state", `Could not load Models: ${error.message}`));
  }
}

function renderProviderOptions() {
  const select = byId("model-provider");
  const selected = select.value;
  select.replaceChildren(new Option("Choose a provider", ""));
  for (const provider of state.providers) {
    const option = new Option(`${provider.name} · ${provider.status}`, provider.id);
    select.add(option);
  }
  if (state.providers.some((provider) => provider.id === selected)) select.value = selected;
  select.disabled = state.providers.length === 0;
  resetProviderModelCatalog();
}

function syncModelCatalogControl() {
  const provider = state.providers.find((item) => item.id === byId("model-provider").value);
  byId("model-catalog-load").disabled = provider?.status !== "connected";
}

function resetProviderModelCatalog() {
  state.catalogRequest += 1;
  byId("provider-model-catalog").replaceChildren();
  byId("model-catalog-load").textContent = "Load models";
  showMessage(byId("model-catalog-message"));
  syncModelCatalogControl();
}

async function loadProviderModelCatalog() {
  const providerId = byId("model-provider").value;
  const provider = state.providers.find((item) => item.id === providerId);
  const load = byId("model-catalog-load");
  const catalog = byId("provider-model-catalog");
  const message = byId("model-catalog-message");
  const requestId = ++state.catalogRequest;
  catalog.replaceChildren();
  showMessage(message);
  syncModelCatalogControl();
  if (!providerId || provider?.status !== "connected") {
    showMessage(message, "Connect the selected provider to load its account model list.", "error");
    return;
  }
  load.disabled = true;
  load.textContent = "Loading…";
  try {
    const models = await api(`/api/providers/${encodeURIComponent(providerId)}/models`);
    if (requestId !== state.catalogRequest || byId("model-provider").value !== providerId) return;
    for (const model of models) {
      const option = document.createElement("option");
      option.value = model.id;
      option.label = model.displayName;
      catalog.append(option);
    }
    showMessage(message, models.length ? `${models.length} models loaded for ${provider.name}.` : "No displayable models were returned for this account.", models.length ? "success" : "");
  } catch (error) {
    if (requestId !== state.catalogRequest || byId("model-provider").value !== providerId) return;
    showMessage(message, `${error.message} You can still enter an upstream model ID.`, "error");
  } finally {
    if (requestId === state.catalogRequest && byId("model-provider").value === providerId) {
      load.textContent = "Load models";
      syncModelCatalogControl();
    }
  }
}

function renderModels() {
  const list = byId("model-list");
  list.replaceChildren();
  if (state.models.length === 0) {
    list.append(element("div", "empty-state", "No Models configured. Add a provider, then create a Model route."));
    return;
  }
  for (const model of state.models) list.append(renderModel(model));
}

function renderModel(model) {
  const card = element("article", "resource-card model-card");
  const header = element("div", "resource-header");
  const titleBlock = element("div", "resource-title");
  titleBlock.append(element("h4", "", model.name));
  titleBlock.append(element("span", "tag tag-muted", `Revision ${model.version}`));
  const controls = element("div", "card-actions");
  controls.append(button("Edit", "quiet small", () => beginModelEdit(model)));
  controls.append(button("Delete", "danger quiet small", async () => {
    if (!window.confirm(`Delete Model “${model.name}”? Service account grants for this Model will be removed.`)) return;
    try {
      await api(`/api/models/${encodeURIComponent(model.id)}`, { method: "DELETE" });
      if (state.editingModel?.id === model.id) cancelModelEdit();
      await loadModelsAndProviders();
    } catch (error) {
      const message = element("p", "inline-error", error.message);
      message.setAttribute("role", "alert");
      card.append(message);
    }
  }));
  header.append(titleBlock, controls);
  card.append(header);
  const target = element("dl", "target-details");
  const provider = state.providers.find((item) => item.id === model.providerId);
  const providerLine = element("div");
  providerLine.append(element("dt", "", "Provider"), element("dd", "", provider?.name || "Unknown provider"));
  const upstreamLine = element("div");
  upstreamLine.append(element("dt", "", "Upstream model"), element("dd", "", model.upstreamModel || "Not set"));
  target.append(providerLine, upstreamLine);
  card.append(target);
  return card;
}

function fillModelMetadataEditor(metadata) {
  const source = metadata && typeof metadata === "object" && !Array.isArray(metadata) ? metadata : {};
  byId("model-context-window").value = source.contextWindow ?? "";
  byId("model-max-tokens").value = source.maxTokens ?? "";
  byId("model-reasoning").value = typeof source.reasoning === "boolean" ? String(source.reasoning) : "";
  const modalities = Array.isArray(source.input) ? source.input : [];
  byId("model-input-text").checked = modalities.includes("text");
  byId("model-input-image").checked = modalities.includes("image");
  const efforts = Array.isArray(source.supportedReasoningEfforts) ? source.supportedReasoningEfforts : [];
  document.querySelectorAll('input[name="model-effort"]').forEach((input) => {
    input.checked = efforts.includes(input.value);
  });
  const hasKnownMetadata = ["contextWindow", "maxTokens", "reasoning", "input", "supportedReasoningEfforts"].some((key) => Object.hasOwn(source, key));
  byId("model-metadata-editor").open = hasKnownMetadata;
}

function readModelMetadata(editing) {
  const knownFields = ["contextWindow", "maxTokens", "input", "reasoning", "supportedReasoningEfforts"];
  const metadata = editing?.metadata && typeof editing.metadata === "object" && !Array.isArray(editing.metadata)
    ? { ...editing.metadata }
    : {};
  for (const field of knownFields) delete metadata[field];

  for (const [key, id] of [["contextWindow", "model-context-window"], ["maxTokens", "model-max-tokens"]]) {
    const raw = byId(id).value.trim();
    if (!raw) continue;
    const value = Number(raw);
    if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${key} must be a positive whole number.`);
    metadata[key] = value;
  }
  const input = [];
  if (byId("model-input-text").checked) input.push("text");
  if (byId("model-input-image").checked) input.push("image");
  if (input.length) metadata.input = input;
  const reasoning = byId("model-reasoning").value;
  if (reasoning) metadata.reasoning = reasoning === "true";
  const efforts = [...document.querySelectorAll('input[name="model-effort"]:checked')].map((input) => input.value);
  if (efforts.length) metadata.supportedReasoningEfforts = efforts;
  return Object.keys(metadata).length ? metadata : undefined;
}

function beginModelEdit(model) {
  state.editingModel = model;
  byId("model-name").value = model.name;
  byId("model-provider").value = model.providerId || "";
  byId("model-upstream").value = model.upstreamModel || "";
  fillModelMetadataEditor(model.metadata);
  resetProviderModelCatalog();
  byId("model-form-heading").textContent = `Edit ${model.name}`;
  byId("model-submit").textContent = "Save changes";
  byId("model-cancel").hidden = false;
  byId("model-metadata-note").hidden = !model.metadata;
  showMessage(byId("model-form-message"));
  byId("model-name").focus();
  byId("model-form-heading").scrollIntoView({ block: "nearest", behavior: "smooth" });
}

function cancelModelEdit() {
  state.editingModel = null;
  byId("model-form").reset();
  byId("model-metadata-editor").open = false;
  byId("model-form-heading").textContent = "Add a Model";
  byId("model-submit").textContent = "Add Model";
  byId("model-cancel").hidden = true;
  byId("model-metadata-note").hidden = true;
  resetProviderModelCatalog();
  showMessage(byId("model-form-message"));
}

async function submitModel(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const submit = byId("model-submit");
  const message = byId("model-form-message");
  const editing = state.editingModel;
  const payload = {
    name: byId("model-name").value,
    providerId: byId("model-provider").value,
    upstreamModel: byId("model-upstream").value,
  };
  try {
    const metadata = readModelMetadata(editing);
    if (metadata !== undefined) payload.metadata = metadata;
  } catch (error) {
    showMessage(message, error.message, "error");
    return;
  }
  submit.disabled = true;
  showMessage(message);
  try {
    const path = editing ? `/api/models/${encodeURIComponent(editing.id)}` : "/api/models";
    await api(path, { method: editing ? "PUT" : "POST", body: JSON.stringify(payload) });
    cancelModelEdit();
    await loadModelsAndProviders();
  } catch (error) {
    showMessage(message, error.message, "error");
  } finally {
    submit.disabled = false;
  }
}

byId("model-form").addEventListener("submit", submitModel);
byId("model-cancel").addEventListener("click", cancelModelEdit);
byId("model-provider").addEventListener("change", resetProviderModelCatalog);
byId("model-catalog-load").addEventListener("click", loadProviderModelCatalog);
