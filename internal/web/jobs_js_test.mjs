// Runs static/jobs.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that the field is made only where
// the page has its bar, that typing narrows the rows by name in any case,
// that a group left with no row is hidden and a message says when none is
// left, that Escape empties the field, that "/" focuses it unless focus is
// in an input, and that the text is applied again to the rows an htmx swap
// brings in.
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

function load({ withBar = true } = {}) {
  active = null;
  const bar = withBar ? new El("div") : null;
  const body = new El("body");
  const doc = new El("document");
  const list = { sections: [] };
  const jobs = new El("div");
  jobs.querySelectorAll = (sel) => (sel === "section" ? list.sections : []);
  list.sections = [section("deploy-web", "Deploy-API"), section("lint", "smoke"), section("nightly")];
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
console.log("jobs.js ok");
