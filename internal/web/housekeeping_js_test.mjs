// Runs static/housekeeping.js against a false page with node alone: `make
// quality` calls it when node is on PATH. It holds that a tab shows its panel
// and hides the others, that the choice is kept under kipp.housekeeping.tab
// and restored on load, that an unknown value or storage that throws falls
// back to Recommendations, that a hash naming a tab wins and is kept, and
// that the arrow keys, Home and End move between the tabs.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/housekeeping.js", import.meta.url), "utf8");
const names = ["recommendations", "parts", "database", "jobs"];

class El {
  constructor(attrs = {}) {
    this.attrs = { ...attrs };
    this.listeners = {};
    this.hidden = false;
    this.focused = false;
    this.classes = new Set();
    this.classList = { add: (c) => this.classes.add(c), contains: (c) => this.classes.has(c) };
  }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] ?? null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  focus() { this.focused = true; }
}

function page({ stored, hash = "", throws = false } = {}) {
  const tabs = names.map((n) => new El({ role: "tab", "data-tab": n, "aria-controls": "tab-" + n }));
  const panels = names.map(() => new El({ role: "tabpanel" }));
  const list = new El({ role: "tablist" });
  list.hidden = true;
  const area = new El();
  area.querySelector = (sel) => (sel === '[role="tablist"]' ? list : null);
  list.querySelectorAll = (sel) => (sel === '[role="tab"]' ? tabs : []);
  const store = new Map(stored === undefined ? [] : [["kipp.housekeeping.tab", stored]]);
  globalThis.localStorage = {
    getItem: (k) => {
      if (throws) throw new Error("denied");
      return store.get(k) ?? null;
    },
    setItem: (k, v) => {
      if (throws) throw new Error("denied");
      store.set(k, v);
    },
  };
  globalThis.location = { hash };
  globalThis.document = {
    getElementById: (id) => (id === "tabs" ? area : (panels[names.findIndex((n) => "tab-" + n === id)] ?? null)),
  };
  new Function(script)();
  const key = (tab, k) => {
    const e = { key: k, target: tab, preventDefault() { this.prevented = true; } };
    list.listeners.keydown(e);
    return e;
  };
  return { tabs, panels, list, area, store, key };
}

function active(p) {
  const on = p.tabs.map((t, i) => (t.attrs["aria-selected"] === "true" ? i : -1)).filter((i) => i >= 0);
  assert.equal(on.length, 1, "exactly one tab is selected");
  p.tabs.forEach((t, i) => {
    assert.equal(t.attrs.tabindex, i === on[0] ? "0" : "-1", "only the active tab is in the tab order");
    assert.equal(p.panels[i].hidden, i !== on[0], "only the active panel shows");
  });
  return names[on[0]];
}

{
  const p = page();
  assert.equal(p.list.hidden, false, "the tablist shows once the script runs");
  assert.ok(p.area.classList.contains("tabbed"), "the page is marked as tabbed");
  assert.equal(active(p), "recommendations", "nothing remembered: Recommendations");
  p.tabs[3].listeners.click();
  assert.equal(active(p), "jobs");
  assert.equal(p.store.get("kipp.housekeeping.tab"), "jobs", "the choice is stored");
}
{
  assert.equal(active(page({ stored: "database" })), "database", "a stored tab is restored");
  assert.equal(active(page({ stored: "nonsense" })), "recommendations", "an unknown value falls back");
  assert.equal(active(page({ throws: true })), "recommendations", "storage that throws falls back");
  const p = page({ throws: true });
  p.tabs[1].listeners.click();
  assert.equal(active(p), "parts", "the choice holds for the page when storage throws");
}
{
  const p = page({ stored: "database", hash: "#jobs" });
  assert.equal(active(p), "jobs", "a hash naming a tab wins");
  assert.equal(p.store.get("kipp.housekeeping.tab"), "jobs", "and is kept");
  assert.equal(active(page({ stored: "database", hash: "#compact" })), "database", "other hashes leave the choice");
}
{
  const p = page();
  let e = p.key(p.tabs[0], "ArrowRight");
  assert.ok(e.prevented);
  assert.equal(active(p), "parts");
  assert.ok(p.tabs[1].focused, "focus follows");
  p.key(p.tabs[1], "End");
  assert.equal(active(p), "jobs");
  p.key(p.tabs[3], "ArrowRight");
  assert.equal(active(p), "recommendations", "Right wraps");
  p.key(p.tabs[0], "ArrowLeft");
  assert.equal(active(p), "jobs", "Left wraps");
  p.key(p.tabs[3], "Home");
  assert.equal(active(p), "recommendations");
  e = p.key(p.tabs[0], "a");
  assert.ok(!e.prevented, "other keys are left alone");
}
console.log("housekeeping.js: ok");
