// Runs static/metrics.js against a false page with node alone: `make
// quality` calls it when node is on PATH. It holds that a sparkline draws
// every value inside its box, the oldest at the left and the highest at
// the top, and that sorting keeps rows with equal keys in the item order
// the page came in, whichever way it sorts.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/metrics.js", import.meta.url), "utf8");

// element is a false DOM element: attributes, children and listeners.
function element(tag) {
  return {
    tag,
    attrs: {},
    children: [],
    listeners: {},
    textContent: "",
    setAttribute(k, v) { this.attrs[k] = v; },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    appendChild(c) { this.children.push(c); c.parentNode = this; },
    replaceChildren(...cs) { this.children = []; for (const c of cs) this.appendChild(c); },
    addEventListener(type, fn) { this.listeners[type] = fn; },
  };
}

function run(cells = [], tables = []) {
  const document = {
    createElementNS: (_, tag) => element(tag),
    createElement: (tag) => element(tag),
    querySelectorAll: (sel) => (sel.startsWith("td.spark-cell") ? cells : sel.startsWith("table") ? tables : []),
  };
  new Function("document", script)(document);
  return globalThis.kippMetrics;
}

// sparkline draws 50 points scaled to its box
{
  const values = Array.from({ length: 50 }, (_, i) => (i % 7) * 3 + i / 10);
  const cell = element("td");
  cell.setAttribute("data-values", values.join(" "));
  const { points } = run([cell]);
  const at = points(values, 100, 20, 2);
  assert.equal(at.length, 50, "a point for each value");
  for (const [x, y] of at) assert.ok(x >= 2 && x <= 98 && y >= 2 && y <= 18, `(${x}, ${y}) is inside the box`);
  assert.equal(at[0][0], 2, "the oldest at the left edge");
  assert.equal(at[49][0], 98, "the newest at the right edge");
  const hi = values.indexOf(Math.max(...values));
  const lo = values.indexOf(Math.min(...values));
  assert.equal(at[hi][1], 2, "the highest at the top");
  assert.equal(at[lo][1], 18, "the lowest at the bottom");

  const [svg] = cell.children;
  assert.equal(svg.tag, "svg");
  assert.equal(svg.attrs.viewBox, "0 0 100 20");
  const line = svg.children.find((c) => c.tag === "polyline");
  assert.equal(line.attrs.points.split(" ").length, 50, "the line has every point");
  assert.equal(svg.attrs["aria-label"], `50 values, from ${values[0]} to ${values[49]}`);

  assert.deepEqual(points([5, 5, 5], 100, 20, 2).map((p) => p[1]), [10, 10, 10], "a flat series runs across the middle");
  assert.deepEqual(points([7], 100, 20, 2), [[50, 10]], "one value is a point in the middle");
  assert.deepEqual(points([], 100, 20, 2), []);
}

// sorting by value keeps item order for equal values
{
  const { order } = run();
  const rows = [
    { item: "a", value: "2" }, { item: "b", value: "1" }, { item: "c", value: "2" },
    { item: "d", value: "" }, { item: "e", value: "1" }, { item: "f", value: "10" },
  ];
  const by = (r) => r.value;
  const items = (rs) => rs.map((r) => r.item).join("");
  assert.equal(items(order(rows, by, true, false)), "beacfd", "ascending, as numbers, ties in item order, empty last");
  assert.equal(items(order(rows, by, true, true)), "facbed", "descending, ties still in item order, empty last");
  assert.equal(items(order(rows, (r) => r.item, false, true)), "fedcba", "text the other way");
  assert.equal(items(rows), "abcdef", "the rows given are left as they were");
}

// A heading pressed sorts its table, and pressed again the other way.
{
  const row = (item, value) => {
    const tr = element("tr");
    const a = element("td");
    a.textContent = item;
    const b = element("td");
    b.setAttribute("data-value", value);
    tr.appendChild(a);
    tr.appendChild(b);
    return tr;
  };
  const head = element("tr");
  const thItem = element("th");
  thItem.textContent = "Item";
  thItem.setAttribute("data-sort", "text");
  const thValue = element("th");
  thValue.textContent = "Value";
  thValue.setAttribute("data-sort", "number");
  head.appendChild(thItem);
  head.appendChild(thValue);
  const body = element("tbody");
  for (const r of [row("a", "3"), row("b", "1"), row("c", "3")]) body.appendChild(r);
  const table = element("table");
  table.querySelectorAll = () => [thItem, thValue];
  table.querySelector = () => body;
  run([], [table]);
  const button = thValue.children[0];
  assert.equal(button.tag, "button");
  assert.equal(button.textContent, "Value", "the heading's words are on its button");
  const items = () => body.children.map((tr) => tr.children[0].textContent).join("");
  button.listeners.click();
  assert.equal(items(), "bac");
  assert.equal(thValue.attrs["aria-sort"], "ascending");
  button.listeners.click();
  assert.equal(items(), "acb");
  assert.equal(thValue.attrs["aria-sort"], "descending");
  thItem.children[0].listeners.click();
  assert.equal(thValue.attrs["aria-sort"], undefined, "one column is sorted at a time");
  assert.equal(items(), "abc");
}

console.log("metrics.js: ok");
