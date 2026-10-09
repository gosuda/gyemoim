// Shared mutable UI state. Modules mutate properties on these objects;
// the objects themselves are never reassigned.
export const state = {
  page: "overview",
  providers: [],
  accounts: [],
  models: [],
  users: [],
  currentUserID: "",
  // Step 8 (U1): temporary passwords awaiting handoff, keyed by user id.
  // Entries render a copyable handoff block on the user's card and survive
  // list re-renders until dismissed or the Users page is left (cleared in
  // nav.js showPage, like the service-accounts key-reveal cleanup).
  userHandoffs: new Map(),
  editingModel: null,
  catalogRequest: 0,
  usersRequest: 0,
  sensitiveCleanup: new Set(),
  requestCursors: [""],
  requestPageIndex: 0,
  appliedRequestFilters: null,
  selectedRequestID: "",
};

// Fetch-control state for the bounded history queries (Overview, Requests,
// Storage): one parent controller per page load plus per-slot child loads.
export const historyState = { generation: 0, controller: null, childControllers: new Set(), selectionToken: null };
