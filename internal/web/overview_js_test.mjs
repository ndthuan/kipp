import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/overview.js", import.meta.url), "utf8");
const names = ["now", "failing", "unstable", "flaky"];

class El {
  constructor(attrs = {}) {
    this.attrs = { ...attrs };
    this.listeners = {};
    this.focused = false;
    this.clicks = 0;
  }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  focus() { this.focused = true; }
  click() { this.clicks++; }
}

function page({ open = "failing", range = "7d" } = {}) {
  const tabs = names.map((n) => new El({ role: "tab", "data-tab": n, "aria-selected": String(n === open) }));
  const panel = new El({ "data-tab": open });
  const current = new El({ "data-range": range });
  const root = new El({ id: "overview" });
  root.id = "overview";
  root.querySelectorAll = (sel) => (sel === '[role="tab"]' ? tabs : []);
  root.querySelector = (sel) => (sel === ".charts [aria-current]" ? current : null);
  const body = new El();
  globalThis.document = {
    body,
    getElementById: (id) => (id === "overview" ? root : id === "overview-panel" ? panel : null),
  };
  new Function(script)();
  return { tabs, panel, root, body };
}

{
  const p = page({ open: "failing" });
  p.panel.setAttribute("data-tab", "flaky");
  p.body.listeners["htmx:afterSettle"]();
  assert.deepEqual(p.tabs.map((t) => t.getAttribute("aria-selected")), ["false", "false", "false", "true"]);
  assert.deepEqual(p.tabs.map((t) => t.getAttribute("tabindex")), ["-1", "-1", "-1", "0"]);
}

{
  const p = page({ open: "failing" });
  p.root.listeners.click({ target: { closest: (sel) => (sel === '[role="tab"]' ? p.tabs[3] : null) }, preventDefault: () => assert.fail("a tab click is htmx's") });
  assert.deepEqual(p.tabs.map((t) => t.getAttribute("aria-selected")), ["false", "false", "false", "true"]);
}

{
  const p = page({ open: "unstable", range: "7d" });
  const detail = { elt: p.root, parameters: {} };
  p.body.listeners["htmx:configRequest"]({ detail });
  assert.deepEqual(detail.parameters, { tab: "unstable", window: "7d" });
  const other = { elt: p.tabs[0], parameters: {} };
  p.body.listeners["htmx:configRequest"]({ detail: other });
  assert.deepEqual(other.parameters, {});
}

{
  const p = page();
  const press = (from, key) => {
    let prevented = false;
    p.root.listeners.keydown({ target: p.tabs[from], key, preventDefault: () => (prevented = true) });
    return prevented;
  };
  assert.equal(press(1, "ArrowRight"), true);
  assert.equal(p.tabs[2].focused && p.tabs[2].clicks === 1, true);
  press(0, "ArrowLeft");
  assert.equal(p.tabs[3].focused && p.tabs[3].clicks === 1, true);
  press(2, "Home");
  assert.equal(p.tabs[0].focused && p.tabs[0].clicks === 1, true);
  press(0, "End");
  assert.equal(p.tabs[3].clicks, 2);
  assert.equal(press(0, "a"), false);
  assert.equal(p.root.listeners.keydown({ target: new El(), key: "ArrowRight", preventDefault: () => assert.fail("outside the tabs") }), undefined);
}

console.log("overview.js: ok");
