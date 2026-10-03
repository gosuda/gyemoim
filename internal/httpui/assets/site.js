(() => {
  const details = document.querySelector("#runtime-details");
  const stateLabel = document.querySelector("#status-label");
  const fields = [
    ["State", (status) => status.state],
    ["Listener", (status) => `127.0.0.1:${status.port}`],
    ["Data directory", (status) => status.dataDirectory],
    ["SQLite", (status) => status.sqliteState],
    ["Started", (status) => new Date(status.startedAt).toLocaleString()],
    ["Runtime", (status) => status.goVersion],
    ["Platform", (status) => `${status.operatingSystem} / ${status.architecture}`],
  ];

  async function loadStatus() {
    try {
      const response = await fetch("/api/status", { credentials: "same-origin", headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`Status request failed (${response.status})`);
      const status = await response.json();
      details.replaceChildren(...fields.map(([label, read]) => {
        const row = document.createElement("div");
        const term = document.createElement("dt");
        const value = document.createElement("dd");
        term.textContent = label;
        value.textContent = String(read(status));
        row.append(term, value);
        return row;
      }));
      stateLabel.textContent = status.state === "ready" ? "Ready" : status.state;
    } catch {
      stateLabel.textContent = "Unavailable";
      details.textContent = "Runtime status could not be loaded.";
    }
  }

  loadStatus();
})();
