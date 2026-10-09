// DOM construction helpers shared by every page module.
export const byId = (id) => document.getElementById(id);

export const element = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = String(text);
  return node;
};

export const button = (label, className, action) => {
  const node = element("button", `button ${className}`, label);
  node.type = "button";
  if (action) node.addEventListener("click", action);
  return node;
};

export const showMessage = (node, message = "", kind = "") => {
  node.textContent = message;
  node.className = kind ? `form-message ${kind}` : "form-message";
};
