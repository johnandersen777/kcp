// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { createBanners } from "./page_banner.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

// Just enough DOM for the banners.
class FakeElement {
  constructor(tag) {
    this.tag = tag;
    this.children = [];
    this.parent = null;
    this.attrs = {};
    this.dataset = {};
    this.listeners = {};
    this.hidden = false;
    this.className = "";
    this.type = "";
    this._text = "";
  }
  set textContent(value) {
    this._text = value;
  }
  get textContent() {
    return this._text + this.children.map((c) => c.textContent).join("");
  }
  setAttribute(name, value) {
    this.attrs[name] = value;
  }
  appendChild(child) {
    child.parent = this;
    this.children.push(child);
    return child;
  }
  remove() {
    if (!this.parent) return;
    this.parent.children = this.parent.children.filter((c) => c !== this);
    this.parent = null;
  }
  replaceWith(other) {
    const i = this.parent.children.indexOf(this);
    other.parent = this.parent;
    this.parent.children[i] = other;
    this.parent = null;
  }
  addEventListener(type, fn) {
    this.listeners[type] = fn;
  }
  click() {
    this.listeners.click?.();
  }
}
const doc = { createElement: (tag) => new FakeElement(tag) };

Deno.test("banners show, replace by id, act on buttons and hide", () => {
  const container = new FakeElement("div");
  container.hidden = true;
  const banners = createBanners(container, doc);
  banners.show("memory", { text: "This browser is not saving state" });
  assertEquals([container.hidden, container.children.length], [false, 1]);
  assertEquals(container.children[0].attrs.role, "alert");
  let retried = 0;
  banners.show("lock", { text: "kcp is running in another tab", actions: [{ label: "Retry", onClick: () => retried++ }, { label: "Take over", onClick: () => {} }] });
  banners.show("lock", { text: "kcp is running in another tab", actions: [{ label: "Retry", onClick: () => retried++ }] });
  assertEquals(container.children.length, 2, "same id replaced");
  assertEquals(banners.text("lock"), "kcp is running in another tabRetry");
  container.children[1].children[1].click();
  assertEquals(retried, 1);
  banners.hide("lock");
  banners.hide("memory");
  assertEquals([container.hidden, container.children.length, banners.ids()], [true, 0, []]);
});
