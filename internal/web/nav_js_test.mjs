// Runs static/nav.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that one menu is open at a time,
// that a click elsewhere or Escape closes them, and that the groups fold
// behind their button and unfold on its click.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/nav.js", import.meta.url), "utf8");

class El {
  constructor(name, parent = null) {
    this.name = name;
    this.parent = parent;
    this.attrs = {};
    this.listeners = {};
    this.hidden = true;
    this.open = false;
    this.focused = false;
    this.classes = new Set();
    this.classList = {
      add: (c) => this.classes.add(c),
      contains: (c) => this.classes.has(c),
      toggle: (c, on) => (on ? this.classes.add(c) : this.classes.delete(c)),
    };
  }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  focus() { this.focused = true; }
  // Only the selectors the script asks for.
  closest(sel) {
    for (let el = this; el; el = el.parent) {
      if (el.name === "drop" && (sel === "details.drop" || (sel === "details.drop[open]" && el.open))) return el;
    }
    return null;
  }
  querySelector(sel) { return sel === "summary" ? this.summary : null; }
}

function page() {
  const top = new El("top");
  const toggle = new El("toggle", top);
  toggle.setAttribute("aria-expanded", "false");
  const menus = [new El("drop", top), new El("drop", top)];
  for (const m of menus) m.summary = new El("summary", m);
  const body = new El("body");
  const listeners = {};
  globalThis.document = {
    querySelector: (sel) => ({ ".top": top, ".menu-toggle": toggle })[sel] ?? null,
    querySelectorAll: (sel) => (sel === "details.drop[open]" ? menus.filter((m) => m.open) : []),
    addEventListener: (type, fn) => { listeners[type] = fn; },
  };
  new Function(script)();
  return { top, toggle, menus, body, fire: (type, e) => listeners[type](e) };
}

{
  const { top, toggle } = page();
  assert.equal(toggle.hidden, false, "the button shows once the script runs");
  assert.ok(top.classList.contains("folding"), "the groups fold");
  toggle.listeners.click();
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  assert.ok(top.classList.contains("open"), "a click unfolds them");
  toggle.listeners.click();
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  assert.ok(!top.classList.contains("open"), "a second click folds them");
}

{
  const { menus, body, fire } = page();
  menus[0].open = true;
  menus[1].open = true; // the browser has just opened the second
  fire("click", { target: menus[1].summary });
  assert.deepEqual(menus.map((m) => m.open), [false, true], "opening one closes the other");
  fire("click", { target: body });
  assert.deepEqual(menus.map((m) => m.open), [false, false], "a click elsewhere closes it");
}

{
  const { menus, fire } = page();
  menus[0].open = true;
  fire("keydown", { key: "Escape", target: menus[0].summary });
  assert.equal(menus[0].open, false, "Escape closes the menu");
  assert.ok(menus[0].summary.focused, "and gives the focus back to its name");
}

{
  const { top, toggle, body, fire } = page();
  toggle.listeners.click();
  fire("keydown", { key: "Escape", target: body });
  assert.ok(!top.classList.contains("open"), "Escape folds the groups");
  assert.ok(toggle.focused, "and gives the focus back to the button");
  fire("keydown", { key: "Enter", target: body });
}

{
  // A page with no groups has no button, and the script asks nothing of it.
  globalThis.document = { querySelector: () => null, querySelectorAll: () => [], addEventListener: () => {} };
  new Function(script)();
}

console.log("nav.js: ok");
