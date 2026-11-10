// Runs static/grants.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds the rule that build or cancel
// brings view, the group boxes (a column of a group, indeterminate when
// mixed, the same rule as a row), the filter (rows and an empty group's
// heading hidden, state kept, a refused job's warning going with it) and the
// save bar (it appears on the first change, counts the jobs changed and goes
// when the form is back to what was loaded).
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/grants.js", import.meta.url), "utf8");

// A selector here is a tag, classes and [attr] or [attr="value"] tests.
function matches(el, sel) {
  const tag = /^[a-z]+/.exec(sel);
  if (tag && el.tagName !== tag[0]) return false;
  for (const cls of sel.matchAll(/\.([\w-]+)/g)) if (!el.className.split(/\s+/).includes(cls[1])) return false;
  for (const attr of sel.matchAll(/\[([\w-]+)(?:="([^"]*)")?\]/g)) {
    if (!(attr[1] in el.attrs)) return false;
    if (attr[2] !== undefined && el.attrs[attr[1]] !== attr[2]) return false;
  }
  return true;
}

class El {
  constructor(tagName, attrs = {}) {
    this.tagName = tagName;
    this.attrs = { ...attrs };
    this.children = [];
    this.parent = null;
    this.className = attrs.class || "";
    this.hidden = false;
    this.checked = false;
    this.defaultChecked = false;
    this.indeterminate = false;
    this.textContent = "";
    this.listeners = {};
    const set = new Set();
    this.classList = { toggle: (name, on) => { on ? set.add(name) : set.delete(name); }, has: (name) => set.has(name) };
  }
  add(...kids) { for (const k of kids) { k.parent = this; this.children.push(k); } return this; }
  appendChild(k) { return this.add(k); }
  before(...kids) { const at = this.parent.children.indexOf(this); for (const k of kids) k.parent = this.parent; this.parent.children.splice(at, 0, ...kids); }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return name in this.attrs ? this.attrs[name] : null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  fire(type, event = {}) { this.listeners[type]({ target: this, preventDefault() {}, ...event }); }
  closest(sel) { for (let el = this; el; el = el.parent) if (matches(el, sel)) return el; return null; }
  querySelectorAll(sel) {
    const out = [];
    for (const kid of this.children) { if (matches(kid, sel)) out.push(kid); out.push(...kid.querySelectorAll(sel)); }
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

function checkbox(name, job, checked) {
  const box = new El("input", { type: "checkbox", name, value: job });
  box.checked = box.defaultChecked = checked;
  return box;
}

// groups: [[label, [[job, "vbc" letters ticked, warn?], ...]], ...]
function page(groups) {
  const table = new El("table", { "data-needs-view": "" });
  const jobs = {};
  for (const [label, list] of groups) {
    const body = new El("tbody");
    const title = new El("th", { colspan: "4", "data-group": "", scope: "rowgroup" });
    title.textContent = label;
    body.add(new El("tr", { "data-group-row": "" }).add(title));
    for (const [job, ticked, warn] of list) {
      const tr = new El("tr", { "data-job": job });
      const boxes = {};
      for (const [letter, col] of [["v", "view"], ["b", "build"], ["c", "cancel"]]) {
        boxes[col] = checkbox(col, job, ticked.includes(letter));
        tr.add(new El("td", { class: "tick" }).add(new El("label").add(boxes[col])));
      }
      body.add(tr);
      jobs[job] = { tr, boxes };
      if (warn) { jobs[job].warn = new El("tr", { "data-warn-of": job }); body.add(jobs[job].warn); }
    }
    table.add(body);
  }
  const count = new El("span", { class: "savebar-count" });
  count.hidden = true;
  const bar = new El("div", { class: "savebar" }).add(count, new El("button"));
  const scroll = new El("div", { class: "scroll" }).add(table);
  const form = new El("form").add(scroll, bar);
  const root = new El("body").add(form);
  globalThis.document = {
    querySelectorAll: (sel) => root.querySelectorAll(sel),
    createElement: (tag) => new El(tag),
  };
  new Function(script)();
  const [filter] = form.children.filter((k) => k.tagName === "input");
  const heads = table.querySelectorAll("tr[data-group-row]");
  const groupBox = (n, col) => heads[n].querySelectorAll("input").find((b) => b.attrs["data-col"] === col);
  return { jobs, bar, count, filter, none: form.children.find((k) => k.className === "sub"), heads, groupBox, table,
    tick(job, col, on = true) { jobs[job].boxes[col].checked = on; table.fire("change", { target: jobs[job].boxes[col] }); },
    tickGroup(n, col, on = true) { const box = groupBox(n, col); box.checked = on; table.fire("change", { target: box }); },
    type(text) { filter.value = text; filter.fire("input"); } };
}

const two = () => [["backend", [["ask", "", false], ["smoke", "v", true], ["slow", "vb", false]]], ["web", [["site", "", false]]]];

{
  const p = page(two());
  p.tick("ask", "build");
  assert.equal(p.jobs.ask.boxes.view.checked, true, "build ticks view");
  p.tick("ask", "view", false);
  assert.equal(p.jobs.ask.boxes.view.checked, true, "view stays ticked while build is");
  p.tick("ask", "build", false);
  p.tick("ask", "view", false);
  assert.equal(p.jobs.ask.boxes.view.checked, false, "view can be cleared once nothing needs it");
  p.tick("site", "cancel");
  assert.equal(p.jobs.site.boxes.view.checked, true, "cancel ticks view");
}
{
  const p = page(two());
  assert.equal(p.groupBox(0, "view").indeterminate, true, "mixed column is indeterminate");
  assert.equal(p.groupBox(0, "build").indeterminate, true);
  assert.equal(p.groupBox(0, "cancel").checked, false, "an empty column is clear");
  assert.equal(p.groupBox(0, "cancel").indeterminate, false);
  assert.equal(p.groupBox(0, "view").attrs["aria-label"], "View every job in backend", "the box says what it ticks");
  assert.equal(p.heads[0].querySelector("th").attrs.colspan, "1", "the heading makes room for the boxes");
  p.tickGroup(0, "view");
  assert.ok(["ask", "smoke", "slow"].every((j) => p.jobs[j].boxes.view.checked), "the group box ticks the column");
  assert.equal(p.groupBox(0, "view").checked, true);
  assert.equal(p.groupBox(0, "view").indeterminate, false);
  assert.equal(p.jobs.site.boxes.view.checked, false, "another group is left alone");
  p.tickGroup(0, "build");
  assert.ok(["ask", "smoke", "slow"].every((j) => p.jobs[j].boxes.build.checked), "and the build column");
  p.tickGroup(0, "view", false);
  assert.ok(["ask", "smoke", "slow"].every((j) => p.jobs[j].boxes.view.checked), "clearing view leaves it where build needs it");
  p.tickGroup(0, "build", false);
  p.tickGroup(0, "view", false);
  assert.ok(["ask", "smoke", "slow"].every((j) => !p.jobs[j].boxes.view.checked), "view clears once nothing needs it");
}
{
  const p = page(two());
  p.tickGroup(1, "cancel");
  assert.equal(p.jobs.site.boxes.view.checked, true, "a group's cancel brings view like a row's");
}
{
  const p = page(two());
  p.type("SM");
  assert.deepEqual(Object.keys(p.jobs).filter((j) => !p.jobs[j].tr.hidden), ["smoke"], "the name narrows the rows, in any case");
  assert.equal(p.jobs.smoke.warn.hidden, false, "a shown job keeps its warning");
  assert.equal(p.heads[1].hidden, true, "web holds no match");
  p.type("slow");
  assert.equal(p.jobs.smoke.warn.hidden, true, "a hidden job's warning goes with it");
  assert.equal(p.heads[1].hidden, true, "a group left with no row loses its heading");
  assert.equal(p.heads[0].hidden, false);
  p.tick("slow", "cancel");
  p.type("");
  assert.equal(p.jobs.ask.boxes.view.checked, false, "a hidden row keeps its state");
  assert.equal(p.jobs.slow.boxes.cancel.checked, true);
  assert.equal(p.jobs.smoke.warn.hidden, false);
  assert.equal(p.heads[1].hidden, false);
  p.type("zzz");
  assert.equal(p.none.hidden, false, "no match says so");
  p.filter.fire("keydown", { key: "Escape" });
  assert.equal(p.filter.value, "", "Escape empties the field");
  assert.equal(p.none.hidden, true);
  assert.equal(p.jobs.ask.tr.hidden, false);
}
{
  const p = page(two());
  p.type("ask");
  p.tickGroup(0, "build");
  assert.equal(p.jobs.ask.boxes.build.checked, true, "a group box reaches the rows shown");
  assert.equal(p.jobs.smoke.boxes.build.checked, false, "and not the ones the filter hides");
  assert.equal(p.groupBox(0, "build").checked, true, "it reflects the rows shown");
  p.type("");
  assert.equal(p.groupBox(0, "build").indeterminate, true, "and the whole group again once they are back");
}
{
  const p = page(two());
  assert.equal(p.bar.classList.has("dirty"), false, "a form as loaded has no save bar");
  assert.equal(p.count.hidden, true);
  p.tick("ask", "build");
  assert.equal(p.bar.classList.has("dirty"), true, "the first change shows it");
  assert.equal(p.count.hidden, false);
  assert.equal(p.count.textContent, "1 job changed", "ticking build also ticked view, still one job");
  p.tick("site", "view");
  assert.equal(p.count.textContent, "2 jobs changed");
  p.tick("site", "view", false);
  p.tick("ask", "build", false);
  assert.equal(p.bar.classList.has("dirty"), true, "ask still has view ticked");
  p.tick("ask", "view", false);
  assert.equal(p.bar.classList.has("dirty"), false, "back to what was loaded, it goes");
  assert.equal(p.count.hidden, true);
  p.tickGroup(0, "cancel");
  assert.equal(p.count.textContent, "3 jobs changed", "a group change counts every job it touched");
}
console.log("grants.js: ok");
