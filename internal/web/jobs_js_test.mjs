import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/jobs.js", import.meta.url), "utf8");

class El {
  constructor(tag, attrs = {}) {
    this.tagName = tag.toUpperCase();
    this.attrs = { ...attrs };
    this.children = [];
    this.listeners = {};
    this.hidden = false;
    this.value = "";
    this.focused = false;
    this.isContentEditable = false;
    this.className = "";
    this.textContent = "";
  }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); }
  prepend(el) { this.children.unshift(el); }
  after(el) { this.next = el; }
  focus() { this.focused = true; active = this; }
  fire(type, event = {}) {
    const e = { preventDefault() { this.prevented = true; }, ...event };
    for (const fn of this.listeners[type] ?? []) fn(e);
    return e;
  }
}

let active = null;

function section(...names) {
  const s = new El("section");
  s.rows = names.map((n) => new El("tr", { "data-name": n }));
  s.querySelectorAll = (sel) => (sel === "tr[data-name]" ? s.rows : []);
  return s;
}

const STATUSES = ["", "failing", "unstable", "running", "disabled"];

function load({ withBar = true } = {}) {
  active = null;
  const bar = withBar ? new El("div") : null;
  if (bar) {
    bar.tabs = STATUSES.map((v, i) => new El("a", { role: "tab", "data-status": v, "aria-selected": i === 0, tabindex: i === 0 ? "0" : "-1" }));
    bar.querySelectorAll = (sel) => (sel === '[role="tab"]' ? bar.tabs : []);
    for (const tab of bar.tabs) {
      tab.clicks = 0;
      tab.click = () => { tab.clicks++; bar.fire("click", { target: { closest: () => tab } }); };
    }
  }
  const body = new El("body");
  const doc = new El("document");
  const list = { sections: [] };
  const jobs = new El("div");
  jobs.querySelectorAll = (sel) => (sel === "section" ? list.sections : []);
  list.sections = [section("deploy-web", "Deploy-API"), section("lint", "smoke"), section("nightly")];
  jobs.attrs["data-status"] = "";
  doc.getElementById = (id) => (id === "jobs-bar" ? bar : id === "jobs" ? jobs : null);
  doc.createElement = (tag) => new El(tag);
  Object.defineProperty(doc, "activeElement", { get: () => active ?? body });
  globalThis.document = doc;
  new Function(script)();
  return { bar, doc, list, jobs, input: bar?.children[0], none: bar?.next };
}

const type = (p, text) => { p.input.value = text; p.input.fire("input"); };
const visible = (p) => p.list.sections.filter((s) => !s.hidden).flatMap((s) => s.rows.filter((r) => !r.hidden).map((r) => r.getAttribute("data-name")));

{
  const p = load({ withBar: false });
  assert.equal(p.input, undefined, "without the bar the page is left as it is");
}

{
  const p = load();
  assert.equal(p.input.getAttribute("aria-label"), "Filter jobs by name", "a field is made in the bar, named for a reader that cannot see it");
  assert.equal(p.none.hidden, true);
  type(p, "DEPLOY");
  assert.deepEqual(visible(p), ["deploy-web", "Deploy-API"], "any case");
  assert.equal(p.list.sections[1].hidden, true, "a group with no row left is hidden");
  assert.equal(p.list.sections[2].hidden, true);
  assert.equal(p.none.hidden, true);
  type(p, "  smo ");
  assert.deepEqual(visible(p), ["smoke"], "the text is trimmed");
  type(p, "zzz");
  assert.deepEqual(visible(p), []);
  assert.equal(p.none.hidden, false, "none left says so");
  type(p, "");
  assert.equal(visible(p).length, 5, "empty text shows every row");
  assert.equal(p.none.hidden, true);
  assert.equal(p.list.sections.some((s) => s.hidden), false);
}

{
  const p = load();
  type(p, "light");
  p.input.fire("keydown", { key: "Escape" });
  assert.equal(p.input.value, "", "Escape empties the field");
  assert.equal(visible(p).length, 5);
}

{
  const p = load();
  const slash = p.doc.fire("keydown", { key: "/" });
  assert.equal(p.input.focused, true, "/ goes to the field");
  assert.equal(slash.prevented, true, "and is not typed into it");
  const p2 = load();
  const other = new El("input");
  other.focus();
  const typed = p2.doc.fire("keydown", { key: "/" });
  assert.equal(p2.input.focused, false, "/ typed in another input stays there");
  assert.equal(typed.prevented, undefined);
  const p3 = load();
  p3.doc.fire("keydown", { key: "/", ctrlKey: true });
  assert.equal(p3.input.focused, false, "a chord is the browser's");
}

{
  const p = load();
  type(p, "deploy");
  p.list.sections = [section("deploy-web", "lint-x"), section("other")];
  p.doc.fire("htmx:afterSwap");
  assert.deepEqual(visible(p), ["deploy-web"], "the text is applied again to the rows of a swap");
}

const open = (p) => p.bar.tabs.filter((t) => t.getAttribute("aria-selected") === true).map((t) => t.getAttribute("data-status"));

{
  const p = load();
  assert.deepEqual(open(p), [""], "All is open to begin with");
  p.bar.tabs[2].click();
  assert.deepEqual(open(p), ["unstable"], "a click selects its tab at once");
  assert.equal(p.bar.tabs[2].getAttribute("tabindex"), "0");
  assert.equal(p.bar.tabs[0].getAttribute("tabindex"), "-1", "only the open tab is in the tab order");
}

{
  const p = load();
  const press = (tab, key) => p.bar.fire("keydown", { key, target: tab });
  const e = press(p.bar.tabs[0], "ArrowRight");
  assert.equal(e.prevented, true);
  assert.equal(p.bar.tabs[1].clicks, 1, "ArrowRight opens the next tab");
  assert.equal(p.bar.tabs[1].focused, true);
  press(p.bar.tabs[1], "ArrowLeft");
  assert.equal(p.bar.tabs[0].clicks, 1, "ArrowLeft opens the previous one");
  press(p.bar.tabs[0], "ArrowLeft");
  assert.equal(p.bar.tabs[4].clicks, 1, "ArrowLeft on the first wraps to the last");
  press(p.bar.tabs[4], "Home");
  assert.equal(p.bar.tabs[0].clicks, 2, "Home opens the first");
  press(p.bar.tabs[0], "End");
  assert.equal(p.bar.tabs[4].clicks, 2, "End opens the last");
  const other = press(new El("input"), "ArrowRight");
  assert.equal(other.prevented, undefined, "a key from outside the tabs is left alone");
}

{
  const p = load();
  type(p, "deploy");
  p.bar.tabs[1].click();
  p.jobs.attrs["data-status"] = "failing";
  p.list.sections = [section("deploy-web", "lint-x"), section("other")];
  p.doc.fire("htmx:afterSwap");
  assert.deepEqual(open(p), ["failing"], "the open tab is the status of the list that arrived");
  assert.deepEqual(visible(p), ["deploy-web"], "the filter text is applied to the list of the new tab");
  assert.equal(p.input.value, "deploy", "and survives the switch");
}
console.log("jobs.js ok");
