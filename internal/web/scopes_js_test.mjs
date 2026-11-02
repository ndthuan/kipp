// Runs static/scopes.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that the picked scopes become
// tags, that only scopes not picked are offered to add, that adding and
// removing change what the hidden select sends, and that nothing can be
// typed in.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/scopes.js", import.meta.url), "utf8");

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.textContent = "";
    this.hidden = false;
    this.disabled = false;
    this.label = "";
    this.value = "";
    this.selected = false;
    this.focused = false;
  }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  append(...kids) { this.children.push(...kids); }
  replaceChildren(...kids) { this.children = kids; }
  after(...els) { this.siblings = els; }
  focus() { this.focused = true; }
  all() { return this.children.flatMap((c) => [c, ...c.all()]); }
  querySelectorAll(sel) { return this.all().filter((c) => c.tagName === sel.toUpperCase()); }
  get options() { return this.querySelectorAll("option"); }
}

globalThis.Option = class extends El {
  constructor(text, value) { super("option"); this.textContent = text; this.value = value; }
};

function opt(value, selected) {
  const o = new globalThis.Option(value, value);
  o.selected = selected;
  return o;
}

function group(label, ...options) {
  const g = new El("optgroup");
  g.label = label;
  g.append(...options);
  return g;
}

const field = new El("select");
field.append(
  group("Every job", opt("global", false)),
  group("Groups", opt("group:web", true)),
  group("Jobs", opt("job:app", false), opt("job:site", true)),
);
globalThis.document = {
  querySelectorAll: (sel) => (sel === "select[data-tags]" ? [field] : []),
  createElement: (tag) => new El(tag),
};
new Function(script)();

const [tags, add] = field.siblings;
const shown = () => tags.children.map((t) => t.children[0].textContent);
const offered = () => add.options.map((o) => o.value).filter((v) => v !== "");
const picked = () => field.options.filter((o) => o.selected).map((o) => o.value);

assert.equal(field.hidden, true, "the select the form sends is hidden");
assert.equal(add.tagName, "SELECT", "a scope is added from a list, never typed");
assert.deepEqual(shown(), ["group:web", "job:site"], "each picked scope is a tag");
assert.deepEqual(offered(), ["global", "job:app"], "only the scopes not picked are offered");

add.value = "job:app";
add.listeners.change();
assert.deepEqual(picked(), ["group:web", "job:app", "job:site"], "adding picks it in the select");
assert.deepEqual(shown(), ["group:web", "job:app", "job:site"]);
assert.deepEqual(offered(), ["global"]);

const remove = tags.children[0].children[1];
assert.equal(remove.textContent, "x");
assert.equal(remove.getAttribute("aria-label"), "Remove group:web", "the x says what it removes");
let stopped = false;
remove.listeners.click({ stopPropagation: () => { stopped = true; } });
assert.deepEqual(picked(), ["job:app", "job:site"], "the x unpicks it");
assert.equal(stopped, true, "the click on a tag that is gone does not close the form");
assert.deepEqual(offered(), ["global", "group:web"], "a removed scope is offered again");
assert.equal(add.focused, true, "the keyboard lands on the list after a removal");

for (const v of ["global", "group:web"]) { add.value = v; add.listeners.change(); }
assert.equal(add.disabled, true, "with every scope picked there is nothing to add");

console.log("scopes.js: ok");
