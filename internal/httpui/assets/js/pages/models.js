// Models page: the create/edit form (with provider dropdown and upstream
// catalog loader) and the Model route cards.
import { api } from "../api.js";
import { state } from "../state.js";
import { button, byId, element, showMessage } from "../dom.js";
import { formErrorText } from "../errors.js";
import { announceSuccess, scrollCardIntoView } from "../feedback.js";
import { providerStatusLabel, providerStatusTagClass } from "../format.js";
import { pageLink } from "../nav.js";
import { modelRuntime } from "../routing-status.js";

const maxTargets = 128;
let rows = [];
let nextRowID = 0;
let modelsRequest = 0;
let reloadRequest = 0;
let dirty = false;
let draggedRow = null;

export async function loadModelsAndProviders(afterSave = false) {
  if (state.modelSaving && !afterSave) return;
  const request = ++modelsRequest;
  const list = byId("model-list");
  list.replaceChildren(element("p", "muted", "Loading Models…"));
  try {
    const [models, providers] = await Promise.all([api("/api/models"), api("/api/providers")]);
    if (request !== modelsRequest) return;
    state.models = models;
    state.providers = providers;
    for (const row of rows) renderProviderOptions(row);
    renderModels();
  } catch (error) {
    if (request !== modelsRequest) return;
    list.replaceChildren(element("p", "empty-state", `Could not load Models: ${error.message}`));
  }
}

// Logout must ask before revoking the session, not at beforeunload after the
// cookie is already gone. Ordinary SPA navigation keeps this mounted draft.
export function confirmModelDocumentExit() {
  if (state.modelSaving || !mayDiscardDraft()) return false;
  dirty = false;
  return true;
}

function renderProviderOptions(row) {
  const select = row.provider;
  const selected = select.value;
  select.replaceChildren(new Option("Choose a provider", ""));
  for (const provider of state.providers) {
    const option = new Option(`${provider.name} · ${providerStatusLabel(provider.status)}`, provider.id);
    select.add(option);
  }
  // A deleted provider is still part of the draft, never silently changed to
  // another selection by a card refresh or a round trip through another page.
  if (selected && !state.providers.some((provider) => provider.id === selected)) {
    select.add(new Option("Unavailable provider — choose a replacement", selected));
  }
  select.value = selected;
}

function resetProviderModelCatalog(row) {
  row.request += 1;
  row.catalog.replaceChildren();
  row.loading = false;
  row.load.textContent = "Load models";
  showMessage(row.message);
  syncEditorControls();
}

async function loadProviderModelCatalog(row) {
  if (state.modelSaving || row.loading) return;
  const providerId = row.provider.value;
  const provider = state.providers.find((item) => item.id === providerId);
  const { load, catalog, message } = row;
  const requestId = ++row.request;
  const current = () => rows.includes(row) && requestId === row.request && row.provider.value === providerId;
  catalog.replaceChildren();
  showMessage(message);
  if (!providerId) {
    showMessage(message, "Choose a provider to load its account model list.", "error");
    return;
  }
  if (provider?.status !== "connected") {
    showMessage(message, "Connect the selected provider to load its account model list.", "error");
    return;
  }
  row.loading = true;
  syncEditorControls();
  load.textContent = "Loading…";
  try {
    const models = await api(`/api/providers/${encodeURIComponent(providerId)}/models`);
    if (!current()) return;
    for (const model of models) {
      const option = document.createElement("option");
      option.value = model.id;
      option.label = model.displayName;
      catalog.append(option);
    }
    showMessage(message, models.length ? `${models.length} models loaded for ${provider.name}.` : "No displayable models were returned for this account.", models.length ? "success" : "");
  } catch (error) {
    if (!current()) return;
    showMessage(message, `${error.message} You can still enter an upstream model ID.`, "error");
  } finally {
    if (current()) {
      row.loading = false;
      load.textContent = "Load models";
      syncEditorControls();
    }
  }
}

function markDirty() {
  if (state.modelSaving) return;
  dirty = true;
  // Keep a conflict actionable even after the next draft edit.
  if (byId("model-conflict-reload").hidden) showMessage(byId("model-form-message"));
}

function announceTargets(text) {
  byId("model-target-announcement").textContent = text;
}

function syncEditorControls() {
  const saving = state.modelSaving;
  byId("model-form").setAttribute("aria-busy", String(saving));
  byId("model-form").querySelectorAll("input, select, button").forEach((control) => { control.disabled = saving; });
  byId("model-metadata-editor").inert = saving;
  byId("model-strategy").querySelector('[value="single"]').disabled = rows.length > 1;
  const fallback = byId("model-strategy").value === "fallback";
  byId("model-target-add").disabled = saving || !fallback || rows.length >= maxTargets;
  byId("model-target-count").textContent = `${rows.length} / ${maxTargets} targets${fallback ? " · New targets append at the end." : " · Choose Fallback to add targets."}`;
  rows.forEach((row, index) => {
    row.priority.textContent = `Priority ${index + 1}`;
    row.node.dataset.priority = String(index + 1);
    row.node.setAttribute("aria-label", `Target priority ${index + 1}`);
    row.up.disabled = saving || index === 0;
    row.down.disabled = saving || index === rows.length - 1;
    row.remove.disabled = saving || rows.length === 1;
    row.load.disabled = saving || row.loading;
    row.handle.disabled = saving || rows.length === 1;
    row.handle.draggable = !saving && rows.length > 1;
    for (const [control, action] of [[row.up, "Move up"], [row.down, "Move down"], [row.remove, "Remove"], [row.load, "Load models for"], [row.handle, "Drag to reorder"]]) {
      control.setAttribute("aria-label", `${action} target priority ${index + 1}`);
    }
    row.providerLabel.textContent = `Provider · target ${index + 1}`;
    row.upstreamLabel.textContent = `Upstream model · target ${index + 1}`;
  });
  byId("model-list").querySelectorAll("button").forEach((control) => { control.disabled = saving; });
  document.querySelectorAll('[data-refresh="models"]').forEach((control) => { control.disabled = saving; });
}

function moveRow(row, destination, focusControl = row.handle) {
  if (state.modelSaving) return;
  const index = rows.indexOf(row);
  if (index < 0 || destination < 0 || destination >= rows.length || destination === index) return;
  rows.splice(index, 1);
  rows.splice(destination, 0, row);
  // Move existing nodes: typed values, row-local catalogs and request tokens
  // stay with the target, not its former numeric priority.
  for (const target of rows) byId("model-targets").append(target.node);
  markDirty();
  syncEditorControls();
  (focusControl.disabled ? row.handle : focusControl).focus();
  announceTargets(`Target moved from priority ${index + 1} to ${destination + 1}. Not saved yet.`);
}

function createTargetRow(target = {}) {
  const id = `model-target-${++nextRowID}`;
  const row = { request: 0, loading: false };
  row.node = element("li", "model-target-row");
  row.node.dataset.targetId = id;
  row.priority = element("strong", "target-priority");
  const heading = element("div", "target-row-heading");
  const controls = element("div", "card-actions");
  row.handle = button("↕ Drag", "quiet target-drag-handle", () => {
    announceTargets("Use Up and Down to reorder with a keyboard or touch. Changes stay in the draft until saved.");
  });
  row.handle.setAttribute("aria-describedby", "model-targets-hint");
  row.up = button("Up", "quiet", () => moveRow(row, rows.indexOf(row) - 1, row.up));
  row.down = button("Down", "quiet", () => moveRow(row, rows.indexOf(row) + 1, row.down));
  row.remove = button("Remove", "danger quiet", () => {
    if (state.modelSaving || rows.length <= 1) return;
    const index = rows.indexOf(row);
    if (index < 0) return;
    row.request += 1;
    rows.splice(index, 1);
    row.node.remove();
    markDirty();
    syncEditorControls();
    rows[Math.min(index, rows.length - 1)].provider.focus();
    announceTargets(`Removed target priority ${index + 1}. ${rows.length} targets remain. Not saved yet.`);
  });
  controls.append(row.handle, row.up, row.down, row.remove);
  heading.append(row.priority, controls);
  const fields = element("div", "target-row-fields");
  const providerField = element("div", "field");
  row.providerLabel = element("label");
  row.providerLabel.htmlFor = `${id}-provider`;
  row.provider = element("select");
  row.provider.id = row.providerLabel.htmlFor;
  row.provider.required = true;
  renderProviderOptions(row);
  if (target.providerId && !state.providers.some((provider) => provider.id === target.providerId)) {
    row.provider.add(new Option("Unavailable provider — choose a replacement", target.providerId));
  }
  row.provider.value = target.providerId || "";
  row.provider.addEventListener("change", () => resetProviderModelCatalog(row));
  providerField.append(row.providerLabel, row.provider);
  const upstreamField = element("div", "field");
  row.upstreamLabel = element("label");
  row.upstreamLabel.htmlFor = `${id}-upstream`;
  row.upstream = element("input");
  row.upstream.id = row.upstreamLabel.htmlFor;
  row.upstream.type = "text";
  row.upstream.required = true;
  row.upstream.maxLength = 1024;
  row.upstream.autocomplete = "off";
  row.upstream.placeholder = "Enter or choose an upstream model ID";
  row.upstream.value = target.upstreamModel || "";
  row.upstream.setAttribute("list", `${id}-catalog`);
  row.catalog = element("datalist");
  row.catalog.id = `${id}-catalog`;
  row.load = button("Load models", "quiet", () => loadProviderModelCatalog(row));
  row.message = element("p", "form-message");
  row.message.id = `${id}-message`;
  row.message.setAttribute("aria-live", "polite");
  row.upstream.setAttribute("aria-describedby", row.message.id);
  const catalogControls = element("div", "model-catalog-controls");
  catalogControls.append(row.upstream, row.load);
  upstreamField.append(row.upstreamLabel, catalogControls, row.catalog, row.message);
  fields.append(providerField, upstreamField);
  row.node.append(heading, fields);
  row.handle.addEventListener("dragstart", (event) => {
    if (state.modelSaving || rows.length < 2) { event.preventDefault(); return; }
    draggedRow = row;
    event.dataTransfer.effectAllowed = "move";
    event.dataTransfer.setData("text/plain", id);
    row.node.classList.add("dragging");
  });
  row.handle.addEventListener("dragend", () => {
    draggedRow = null;
    rows.forEach((target) => target.node.classList.remove("dragging", "drag-over"));
  });
  row.node.addEventListener("dragover", (event) => {
    if (state.modelSaving || !draggedRow || draggedRow === row) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
    row.node.classList.add("drag-over");
  });
  row.node.addEventListener("dragleave", (event) => {
    if (!row.node.contains(event.relatedTarget)) row.node.classList.remove("drag-over");
  });
  row.node.addEventListener("drop", (event) => {
    if (state.modelSaving || !draggedRow) return;
    event.preventDefault();
    const source = draggedRow;
    draggedRow = null;
    rows.forEach((target) => target.node.classList.remove("dragging", "drag-over"));
    moveRow(source, rows.indexOf(row));
  });
  return row;
}

function replaceTargets(targets) {
  rows.forEach((row) => { row.request += 1; });
  draggedRow = null;
  rows = targets.map(createTargetRow);
  byId("model-targets").replaceChildren(...rows.map((row) => row.node));
  announceTargets("");
  syncEditorControls();
}

function modelTargets(model) {
  return model.targets?.length ? model.targets : [{ providerId: model.providerId, upstreamModel: model.upstreamModel }];
}

function renderModels() {
  const list = byId("model-list");
  list.replaceChildren();
  if (state.models.length === 0) {
    list.append(element("div", "empty-state", "No Models configured. Add a provider, then create a Model route."));
    return;
  }
  for (const model of state.models) list.append(renderModel(model));
  syncEditorControls();
}

// findModelCard locates a Model's card after a list re-render (cards carry
// their model id as a data attribute) so feedback can be placed on the fresh
// card, mirroring the provider/account card convention.
function findModelCard(modelID) {
  return byId("model-list").querySelector(`.resource-card[data-model-id="${CSS.escape(modelID)}"]`);
}

function renderModel(model) {
  const card = element("article", "resource-card model-card");
  card.dataset.modelId = model.id;
  card.dataset.strategy = model.strategy || "single";
  card.dataset.version = String(model.version);
  const header = element("div", "resource-header");
  const titleBlock = element("div", "resource-title");
  titleBlock.append(element("h4", "", model.name));
  titleBlock.append(element("span", "tag tag-muted", `Revision ${model.version}`));
  titleBlock.append(element("span", "tag", model.strategy === "fallback" ? "Fallback" : "Single"));
  const controls = element("div", "card-actions");
  controls.append(button("Edit", "quiet small", () => beginModelEdit(model)));
  controls.append(button("Delete", "danger quiet small", async () => {
    if (state.modelSaving) return;
    if (state.editingModel?.id === model.id && dirty && !window.confirm("Delete this Model and discard its unsaved draft?")) return;
    if (!window.confirm(`Delete Model “${model.name}”? Service account grants must be removed first.`)) return;
    try {
      await api(`/api/models/${encodeURIComponent(model.id)}`, { method: "DELETE" });
      if (state.editingModel?.id === model.id && !state.modelSaving) resetModelEditor();
      await loadModelsAndProviders();
    } catch (error) {
      const message = element("p", "inline-error", formErrorText(error, { kind: "model", action: "delete", name: model.name }));
      message.setAttribute("role", "alert");
      card.append(message);
    }
  }));
  header.append(titleBlock, controls);
  card.append(header);
  const targets = element("ol", "model-card-targets");
  targets.setAttribute("aria-label", "Targets in priority order");
  modelTargets(model).forEach((route, index) => {
    const item = element("li");
    item.dataset.targetIndex = String(index);
    item.dataset.providerId = route.providerId;
    const target = element("dl", "target-details");
    const provider = state.providers.find((item) => item.id === route.providerId);
    const providerLine = element("div");
    const providerValue = element("dd");
    providerValue.append(provider?.name || "Unknown provider");
    if (provider) {
      const statusTag = element("span", `tag model-target-provider-status ${providerStatusTagClass(provider.status)}`, providerStatusLabel(provider.status));
      if (provider.status !== "connected") {
        statusTag.title = `This target cannot serve until ${provider.name} is connected.`;
      }
      providerValue.append(" ", statusTag);
    }
    providerLine.append(element("dt", "", `Priority ${index + 1} · Provider`), providerValue);
    const upstreamLine = element("div");
    upstreamLine.append(element("dt", "", "Upstream model"), element("dd", "", route.upstreamModel || "Not set"));
    target.append(providerLine, upstreamLine);
    item.append(target);
    targets.append(item);
  });
  card.append(targets, modelRuntime(model, [...targets.children]));
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
  if (state.modelSaving || !mayDiscardDraft()) return;
  fillModelEditor(model);
}

function mayDiscardDraft() {
  return !dirty || window.confirm("Discard the unsaved Model draft? This cannot be undone.");
}

function fillModelEditor(model) {
  reloadRequest += 1;
  // Keep the original revision/metadata snapshot, even when cards refresh.
  state.editingModel = structuredClone(model);
  byId("model-name").value = model.name;
  byId("model-strategy").value = model.strategy || "single";
  replaceTargets(modelTargets(model));
  fillModelMetadataEditor(model.metadata);
  byId("model-form-heading").textContent = `Edit ${model.name}`;
  byId("model-submit").textContent = "Save changes";
  byId("model-cancel").hidden = false;
  byId("model-metadata-note").hidden = !model.metadata;
  byId("model-conflict-reload").hidden = true;
  dirty = false;
  showMessage(byId("model-form-message"));
  byId("model-name").focus();
  byId("model-form-heading").scrollIntoView({ block: "nearest", behavior: "smooth" });
}

function cancelModelEdit() {
  if (state.modelSaving || !mayDiscardDraft()) return;
  resetModelEditor();
  byId("model-name").focus();
}

function resetModelEditor() {
  reloadRequest += 1;
  state.editingModel = null;
  byId("model-form").reset();
  byId("model-strategy").value = "single";
  replaceTargets([{}]);
  byId("model-metadata-editor").open = false;
  byId("model-form-heading").textContent = "Add a Model";
  byId("model-submit").textContent = "Add Model";
  byId("model-cancel").hidden = true;
  byId("model-metadata-note").hidden = true;
  byId("model-conflict-reload").hidden = true;
  dirty = false;
  showMessage(byId("model-form-message"));
}

async function reloadCurrentModel() {
  if (state.modelSaving || !state.editingModel) return;
  const model = state.editingModel;
  const request = ++reloadRequest;
  const reload = byId("model-conflict-reload");
  reload.disabled = true;
  try {
    const current = await api(`/api/models/${encodeURIComponent(model.id)}`);
    // Never replace a different/new draft after an old reload finishes. Ask
    // after fetching, so the confirmation includes edits made while waiting.
    if (request !== reloadRequest || state.editingModel !== model || state.modelSaving) return;
    if (!window.confirm(`Reload saved revision ${current.version} of “${current.name}” and discard this draft? Cancel keeps your draft and its original revision.`)) return;
    fillModelEditor(current);
    showMessage(byId("model-form-message"), `Loaded saved revision ${current.version}. Review it before saving.`, "success");
  } catch (error) {
    if (request === reloadRequest) showMessage(byId("model-form-message"), `Could not reload: ${error.message} Your draft is unchanged.`, "error");
  } finally {
    if (request === reloadRequest && !state.modelSaving) reload.disabled = false;
  }
}

async function submitModel(event) {
  event.preventDefault();
  if (state.modelSaving) return;
  const message = byId("model-form-message");
  const editing = state.editingModel;
  const name = byId("model-name").value.trim();
  const payload = {
    name,
    strategy: byId("model-strategy").value,
    targets: rows.map((row) => ({ providerId: row.provider.value, upstreamModel: row.upstream.value.trim() })),
  };
  if (editing) payload.expectedVersion = editing.version;
  try {
    if (!name) throw new Error("Enter a Model name.");
    if (payload.strategy === "single" && rows.length !== 1) throw new Error("Single requires exactly one target. Remove extra targets or choose Fallback.");
    if (rows.length < 1 || rows.length > maxTargets) throw new Error("Choose 1 to 128 targets.");
    const pairs = new Set();
    for (const [index, target] of payload.targets.entries()) {
      if (!target.providerId || !target.upstreamModel) throw new Error(`Choose a provider and upstream model for target priority ${index + 1}.`);
      if (new TextEncoder().encode(target.upstreamModel).length > 1024) throw new Error(`Upstream model at priority ${index + 1} must be at most 1024 UTF-8 bytes.`);
      const pair = JSON.stringify(target);
      if (pairs.has(pair)) throw new Error(`Target priority ${index + 1} duplicates another provider/upstream model pair. Remove it or choose a different target.`);
      pairs.add(pair);
    }
    const metadata = readModelMetadata(editing);
    if (metadata !== undefined) payload.metadata = metadata;
  } catch (error) {
    showMessage(message, error.message, "error");
    return;
  }
  state.modelSaving = true;
  reloadRequest += 1;
  modelsRequest += 1;
  syncEditorControls();
  showMessage(message, "Saving Model…");
  let savedID;
  try {
    const path = editing ? `/api/models/${encodeURIComponent(editing.id)}` : "/api/models";
    const saved = await api(path, { method: editing ? "PUT" : "POST", body: JSON.stringify(payload) });
    savedID = editing ? editing.id : saved?.id;
    resetModelEditor();
    await loadModelsAndProviders(true);
    announceSuccess(message, editing ? "Changes saved." : `Model “${name}” added.`);
    scrollCardIntoView(findModelCard(savedID));
  } catch (error) {
    byId("model-conflict-reload").hidden = !(editing && error.status === 409);
    showMessage(message, formErrorText(error, { kind: "model", action: editing ? "edit" : "create", name }), "error");
  } finally {
    state.modelSaving = false;
    syncEditorControls();
    // A failed save never swaps in a newer expectedVersion. On success the
    // fresh Edit button is a useful next focus; otherwise return to Save.
    const focus = savedID ? findModelCard(savedID)?.querySelector("button") : byId("model-submit");
    (focus || byId("model-name")).focus({ preventScroll: true });
  }
}

byId("model-form").addEventListener("submit", submitModel);
byId("model-cancel").addEventListener("click", cancelModelEdit);
byId("model-conflict-reload").addEventListener("click", reloadCurrentModel);
byId("model-form").addEventListener("input", markDirty);
byId("model-form").addEventListener("change", markDirty);
byId("model-target-add").addEventListener("click", () => {
  if (state.modelSaving || byId("model-strategy").value !== "fallback" || rows.length >= maxTargets) return;
  const row = createTargetRow();
  rows.push(row);
  byId("model-targets").append(row.node);
  markDirty();
  syncEditorControls();
  row.provider.focus();
  announceTargets(`Added target priority ${rows.length}. Not saved yet.`);
});
byId("model-strategy").addEventListener("change", () => {
  // Single is disabled until extra rows are removed; no destructive implicit
  // conversion and no restoration guesswork after a cancelled confirmation.
  if (rows.length > 1 && byId("model-strategy").value === "single") byId("model-strategy").value = "fallback";
  syncEditorControls();
});
window.addEventListener("beforeunload", (event) => {
  if (!dirty && !state.modelSaving) return;
  event.preventDefault();
  event.returnValue = "";
});
replaceTargets([{}]);

// Review S1: the grant note bridges to the other half of the key+grant
// invariant — the Model form's "not automatically granted" warning links to
// the page where grants are made. The link is a pageLink button because the
// router is hash-based.
{
  const note = byId("model-form-heading").parentElement.querySelector(".muted");
  note.append(" Grant access on the ", pageLink("Service accounts", "service-accounts"), " page.");
}

// Review M4: the Pi metadata fields point at the Pi setup panel where it
// actually lives (each service account's detail view).
byId("model-metadata-hint").append(
  " The Pi setup panel is on the ",
  pageLink("Service accounts", "service-accounts"),
  " page.",
);
