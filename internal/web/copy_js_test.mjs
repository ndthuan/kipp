// Runs static/copy.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that the button shows only where
// there is a clipboard, that a click copies the value it names and says so,
// and that a refused copy says to copy by hand.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/copy.js", import.meta.url), "utf8");

class El {
  constructor(attrs = {}, text = "") {
    this.attrs = attrs;
    this.textContent = text;
    this.hidden = true;
    this.listeners = {};
  }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
}

function page(clipboard) {
  const value = new El({}, "kipp_the-token-value");
  const status = new El();
  const button = new El({ "data-copy": "once-value", "data-copy-status": "once-copied" });
  const byID = { "once-value": value, "once-copied": status };
  Object.defineProperty(globalThis, "navigator", { value: clipboard ? { clipboard } : {}, configurable: true, writable: true });
  globalThis.document = {
    querySelectorAll: (sel) => (sel === "[data-copy]" ? [button] : []),
    getElementById: (id) => byID[id] ?? null,
  };
  new Function(script)();
  return { value, status, button };
}

{
  const { button } = page(null);
  assert.equal(button.hidden, true, "without a clipboard the button stays hidden");
}

{
  const copied = [];
  const { button, status } = page({ writeText: async (t) => { copied.push(t); } });
  assert.equal(button.hidden, false, "with a clipboard the button shows");
  await button.listeners.click();
  assert.deepEqual(copied, ["kipp_the-token-value"], "a click copies the value it names");
  assert.equal(status.textContent, "Copied.");
}

{
  const { button, status } = page({ writeText: async () => { throw new Error("denied"); } });
  await button.listeners.click();
  assert.match(status.textContent, /by hand/, "a refused copy says to copy it by hand");
}

console.log("copy.js: ok");
