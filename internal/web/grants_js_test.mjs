// Runs static/grants.js against a false page with node alone: `make quality`
// calls it when node is on PATH. It holds that ticking build or cancel ticks
// view, that view cannot be unticked while either is ticked, and that it can
// once both are not.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/grants.js", import.meta.url), "utf8");

class Box {
  constructor(name) { this.name = name; this.checked = false; }
}

function page() {
  const row = { boxes: { view: new Box("view"), build: new Box("build"), cancel: new Box("cancel") } };
  row.closest = () => row;
  row.querySelector = (sel) => row.boxes[/name="(\w+)"/.exec(sel)[1]];
  for (const box of Object.values(row.boxes)) box.closest = () => row;
  let listener;
  const table = { addEventListener: (type, fn) => { assert.equal(type, "change"); listener = fn; } };
  globalThis.document = { querySelectorAll: (sel) => { assert.equal(sel, "table[data-needs-view]"); return [table]; } };
  new Function(script)();
  return { row, change: (box) => listener({ target: box }) };
}

{
  const { row, change } = page();
  row.boxes.build.checked = true;
  change(row.boxes.build);
  assert.equal(row.boxes.view.checked, true, "build ticks view");
}
{
  const { row, change } = page();
  row.boxes.cancel.checked = true;
  change(row.boxes.cancel);
  assert.equal(row.boxes.view.checked, true, "cancel ticks view");
  row.boxes.view.checked = false;
  change(row.boxes.view);
  assert.equal(row.boxes.view.checked, true, "view stays ticked while cancel is");
  row.boxes.cancel.checked = false;
  row.boxes.view.checked = false;
  change(row.boxes.view);
  assert.equal(row.boxes.view.checked, false, "view can be cleared once nothing needs it");
}
{
  const { row, change } = page();
  row.boxes.view.checked = true;
  change(row.boxes.view);
  assert.equal(row.boxes.build.checked, false, "view alone ticks nothing else");
}
console.log("grants.js: ok");
