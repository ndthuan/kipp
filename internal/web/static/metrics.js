(() => {
  const SVG = "http://www.w3.org/2000/svg";
  const WIDTH = 100;
  const HEIGHT = 20;
  const PAD = 2;

  const points = (values, width, height, pad) => {
    const n = values.length;
    if (n === 0) return [];
    const lo = Math.min(...values);
    const hi = Math.max(...values);
    const step = n > 1 ? (width - 2 * pad) / (n - 1) : 0;
    return values.map((v, i) => {
      const x = n > 1 ? pad + i * step : width / 2;
      const y = hi === lo ? height / 2 : pad + ((hi - v) / (hi - lo)) * (height - 2 * pad);
      return [Math.round(x * 100) / 100, Math.round(y * 100) / 100];
    });
  };

  const order = (rows, key, numeric, descending) => {
    const sign = descending ? -1 : 1;
    const read = (row) => {
      const raw = key(row);
      if (raw === "" || raw === undefined || raw === null) return null;
      if (!numeric) return String(raw);
      const v = Number(raw);
      return Number.isNaN(v) ? null : v;
    };
    const keyed = rows.map((row) => ({ row, k: read(row) }));
    keyed.sort((a, b) => {
      if (a.k === null || b.k === null) return (a.k === null) - (b.k === null);
      if (numeric) return sign * (a.k - b.k);
      return sign * a.k.localeCompare(b.k);
    });
    return keyed.map((e) => e.row);
  };

  globalThis.kippMetrics = { points, order };
  if (typeof document === "undefined") return;

  const draw = (cell) => {
    const values = cell.getAttribute("data-values").split(" ").filter((s) => s !== "").map(Number);
    if (values.length === 0) return;
    const svg = document.createElementNS(SVG, "svg");
    svg.setAttribute("class", "spark");
    svg.setAttribute("viewBox", `0 0 ${WIDTH} ${HEIGHT}`);
    svg.setAttribute("width", String(WIDTH));
    svg.setAttribute("height", String(HEIGHT));
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", `${values.length} values, from ${values[0]} to ${values[values.length - 1]}`);
    const at = points(values, WIDTH, HEIGHT, PAD);
    if (at.length > 1) {
      const line = document.createElementNS(SVG, "polyline");
      line.setAttribute("points", at.map((p) => p.join(",")).join(" "));
      svg.appendChild(line);
    }
    const [x, y] = at[at.length - 1];
    const last = document.createElementNS(SVG, "circle");
    last.setAttribute("cx", String(x));
    last.setAttribute("cy", String(y));
    last.setAttribute("r", "2");
    svg.appendChild(last);
    cell.replaceChildren(svg);
  };

  const sortable = (table) => {
    const heads = [...table.querySelectorAll("th[data-sort]")];
    const body = table.querySelector("tbody");
    for (const th of heads) {
      const column = [...th.parentNode.children].indexOf(th);
      const button = document.createElement("button");
      button.type = "button";
      button.className = "sort";
      button.textContent = th.textContent;
      th.replaceChildren(button);
      button.addEventListener("click", () => {
        const descending = th.getAttribute("aria-sort") === "ascending";
        for (const other of heads) other.removeAttribute("aria-sort");
        th.setAttribute("aria-sort", descending ? "descending" : "ascending");
        const cell = (row) => row.children[column];
        const sorted = order([...body.children], (row) => cell(row).getAttribute("data-value") ?? cell(row).textContent.trim(),
          th.getAttribute("data-sort") === "number", descending);
        body.replaceChildren(...sorted);
      });
    }
  };

  for (const cell of document.querySelectorAll("td.spark-cell[data-values]")) draw(cell);
  for (const table of document.querySelectorAll("table[data-sortable]")) sortable(table);
})();
