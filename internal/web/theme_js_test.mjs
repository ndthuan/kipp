// Runs static/theme.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that the picker shows once the
// script runs, that Light and Dark hold the page to one theme and keep it in
// the cookie, that System hands it back to the system and drops the cookie,
// that one pick is pressed at a time, and that a pick updates the button's
// icon (data-now) and closes the menu.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/theme.js", import.meta.url), "utf8");

class El {
  constructor(attrs = {}) {
    this.attrs = { ...attrs };
    this.hidden = true;
    this.listeners = {};
  }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return this.attrs[k] ?? null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  closest(sel) { return sel === "[data-theme-pick]" && "data-theme-pick" in this.attrs ? this : null; }
}

function page(protocol, theme) {
  const root = new El(theme ? { "data-theme": theme } : {});
  const picks = ["system", "light", "dark"].map((p) => new El({ "data-theme-pick": p, "aria-pressed": String(p === (theme || "system")) }));
  const group = new El();
  group.open = true;
  const summary = new El({ "data-now": theme || "system" });
  group.querySelector = (sel) => (sel === "summary" ? summary : null);
  group.querySelectorAll = (sel) => (sel === "[data-theme-pick]" ? picks : []);
  const cookies = [];
  globalThis.location = { protocol };
  globalThis.document = {
    documentElement: root,
    querySelector: (sel) => (sel === ".theme" ? group : null),
    set cookie(v) { cookies.push(v); },
  };
  new Function(script)();
  const click = (p) => group.listeners.click({ target: picks.find((b) => b.getAttribute("data-theme-pick") === p) });
  const pressed = () => picks.filter((b) => b.getAttribute("aria-pressed") === "true").map((b) => b.getAttribute("data-theme-pick"));
  return { root, group, summary, cookies, click, pressed };
}

{
  const { root, group, summary, cookies, click, pressed } = page("https:");
  assert.equal(group.hidden, false, "the picker shows once the script runs");
  click("dark");
  assert.equal(summary.getAttribute("data-now"), "dark", "the button shows the theme that holds");
  assert.equal(group.open, false, "a pick closes the menu");
  assert.equal(root.getAttribute("data-theme"), "dark", "Dark holds the page dark");
  assert.deepEqual(pressed(), ["dark"]);
  assert.equal(cookies.at(-1), "kipp_theme=dark; Path=/; Max-Age=31536000; SameSite=Lax; Secure", "and keeps it, Secure over https");
  click("light");
  assert.equal(root.getAttribute("data-theme"), "light");
  assert.deepEqual(pressed(), ["light"]);
  assert.equal(cookies.at(-1), "kipp_theme=light; Path=/; Max-Age=31536000; SameSite=Lax; Secure");
}

{
  const { root, summary, cookies, click, pressed } = page("http:", "dark");
  click("system");
  assert.equal(summary.getAttribute("data-now"), "system");
  assert.equal(root.getAttribute("data-theme"), null, "System hands the page back to the system");
  assert.deepEqual(pressed(), ["system"]);
  assert.equal(cookies.at(-1), "kipp_theme=; Path=/; Max-Age=0; SameSite=Lax", "and drops the cookie, not Secure over http");
}

{
  // A click inside the group but on no pick changes nothing.
  const { root, cookies, group, summary } = page("https:", "light");
  group.listeners.click({ target: new El() });
  assert.equal(root.getAttribute("data-theme"), "light");
  assert.equal(summary.getAttribute("data-now"), "light");
  assert.equal(group.open, true, "and leaves the menu open");
  assert.equal(cookies.length, 0);
}

{
  // A page with no picker: the script asks nothing of it.
  globalThis.document = { querySelector: () => null };
  new Function(script)();
}

console.log("theme.js: ok");
