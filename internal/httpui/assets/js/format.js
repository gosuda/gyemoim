// Formatting and display helpers: plain-string formatters plus the small
// presentational cell/tag builders shared by more than one page.
import { element } from "./dom.js";

export function formatUTC(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "Unknown time";
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium", timeZone: "UTC" }).format(date) + " UTC";
}

export function formatBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes < 1024) return `${formatNumber(bytes)} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let scaled = bytes / 1024;
  let unit = units[0];
  for (let index = 1; scaled >= 1024 && index < units.length; index++) {
    scaled /= 1024;
    unit = units[index];
  }
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(scaled)} ${unit}`;
}

export function formatNumber(value) {
  return Number.isFinite(Number(value)) ? Number(value).toLocaleString() : "Unknown";
}

export function formatOffsetNS(value) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return "Unknown (not recorded)";
  return `${(Number(value) / 1e6).toFixed(Number(value) % 1e6 === 0 ? 0 : 2)} ms`;
}

export function formatDurationNS(value) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return "Unknown";
  const ns = Number(value);
  if (ns < 1e6) return `${(ns / 1e3).toFixed(1)} µs`;
  if (ns < 1e9) return `${(ns / 1e6).toFixed(ns < 1e8 ? 1 : 0)} ms`;
  return `${(ns / 1e9).toFixed(ns < 6e10 ? 2 : 1)} s`;
}

export function formatDate(value) {
  if (!value) return "Unknown";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? String(value) : date.toLocaleString();
}

export function identity(primary, id) {
  const label = primary || "Historical name unavailable";
  return { label, id: id || "Historical ID unavailable" };
}

export function identityCell(primary, id) {
  const cell = element("span", "identity-cell");
  const info = identity(primary, id);
  cell.append(element("strong", "", info.label), element("small", "", info.id));
  return cell;
}

// Outcome enums are internal values (review B7); these shared helpers give
// them human labels and one-line definitions so Overview and Requests render
// the same words and can attach the same legend.
const outcomeLabels = {
  active: "Active",
  interrupted: "Interrupted",
  completed: "Completed",
  failed: "Failed",
  cancelled: "Cancelled",
  incomplete: "Incomplete",
};

const outcomeDescriptions = {
  active: "The request was admitted and is still being recorded.",
  interrupted: "The connection ended before the request finished; the partial record is kept.",
  completed: "The request finished and a full response was recorded.",
  failed: "The request ended with an upstream or provider error.",
  cancelled: "The client cancelled the request before completion.",
  incomplete: "The request ended without a complete response.",
};

export function outcomeLabel(outcome) {
  return outcomeLabels[outcome] || "Unknown";
}

export function outcomeDescription(outcome) {
  return outcomeDescriptions[outcome] || "The outcome of this request could not be determined.";
}

export function outcomeTag(outcome) {
  const kind = outcome === "completed"
    ? "tag-success"
    : ["failed", "interrupted"].includes(outcome)
      ? "tag-danger"
      : ["cancelled", "incomplete"].includes(outcome)
        ? "tag-muted"
        : "";
  const tag = element("span", `tag ${kind}`, outcomeLabel(outcome));
  tag.title = outcomeDescription(outcome);
  return tag;
}

// Collapsed legend for the outcome enums; attach wherever outcomes are listed
// so the definitions are one click away (review B7).
export function outcomeLegend() {
  const details = element("details", "outcome-legend");
  details.append(element("summary", "", "What the outcomes mean"));
  const list = element("dl");
  for (const name of Object.keys(outcomeLabels)) {
    list.append(element("dt", "", outcomeLabels[name]), element("dd", "", outcomeDescriptions[name]));
  }
  details.append(list);
  return details;
}

// Freshness stamp for status-line slots (review B3 / decision 4): the load
// success feedback, e.g. "Updated 14:03:27 UTC".
export function updatedStamp() {
  return `Updated ${new Date().toISOString().slice(11, 19)} UTC`;
}

export function historyErrorMessage(error) {
  if (error?.name === "AbortError") return "";
  return error?.message || "History could not be loaded.";
}
