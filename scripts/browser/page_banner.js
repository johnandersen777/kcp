// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Page-wide banners under the header (#banners): state the person must see
// rather than find in #log (memory-only state, unreadable saved state, kcp
// running in another tab). One banner per id; showing an id again replaces it.

/**
 * @param {Element} container
 * @param {Document} doc
 */
export function createBanners(container, doc = globalThis.document) {
  const shown = new Map();
  return {
    /**
     * @param {string} id
     * @param {{text: string, level?: "warn"|"error"|"info",
     *   actions?: {label: string, onClick: () => void}[]}} banner
     */
    show(id, { text, level = "warn", actions = [] }) {
      const el = doc.createElement("div");
      el.className = `banner banner-${level}`;
      el.setAttribute("role", level === "info" ? "status" : "alert");
      el.dataset.banner = id;
      const message = doc.createElement("span");
      message.className = "banner-text";
      message.textContent = text;
      el.appendChild(message);
      for (const action of actions) {
        const button = doc.createElement("button");
        button.type = "button";
        button.textContent = action.label;
        button.addEventListener("click", () => action.onClick());
        el.appendChild(button);
      }
      const old = shown.get(id);
      if (old) old.replaceWith(el);
      else container.appendChild(el);
      shown.set(id, el);
      container.hidden = false;
      return el;
    },
    hide(id) {
      shown.get(id)?.remove();
      shown.delete(id);
      container.hidden = shown.size === 0;
    },
    text(id) {
      return shown.get(id)?.textContent ?? null;
    },
    ids() {
      return [...shown.keys()];
    },
  };
}
