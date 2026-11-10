// The grants form of a user's page. Without this script the page is the form
// as the server sends it; the group boxes, the filter and the count of
// changes are made here and exist only where they work.
//
// Build and cancel need view, because a job a user may not see answers them
// 404 and the grant would do nothing. The server refuses the combination, so
// this only saves the admin the round trip: ticking build or cancel ticks
// view, and view stays ticked while either is.
//
// A group's box ticks or clears one column for the rows of the group that
// are shown, so a filter narrows what a group box reaches. The filter only
// hides rows: a hidden row is still in the form, with its boxes as they are,
// and is sent with it.
(() => {
  const COLS = ["view", "build", "cancel"];
  const NAMES = { view: "View", build: "Build", cancel: "Cancel" };

  for (const table of document.querySelectorAll("table[data-needs-view]")) {
    const form = table.closest("form");
    const scroll = table.closest(".scroll");
    const bar = form && form.querySelector(".savebar");
    const count = bar && bar.querySelector(".savebar-count");

    const rows = new Map();
    for (const tr of table.querySelectorAll("tr[data-job]")) {
      const box = {};
      for (const col of COLS) box[col] = tr.querySelector('input[name="' + col + '"]');
      rows.set(tr, { tr, name: tr.getAttribute("data-job"), box });
    }

    const groups = [];
    for (const body of table.querySelectorAll("tbody")) {
      const head = body.querySelector("tr[data-group-row]");
      const title = body.querySelector("th[data-group]");
      if (!head || !title) continue;
      const group = { body, head, rows: [], box: {} };
      for (const tr of body.querySelectorAll("tr[data-job]")) group.rows.push(rows.get(tr));
      const label = title.textContent.trim();
      title.setAttribute("colspan", "1");
      for (const col of COLS) {
        const cell = document.createElement("td");
        cell.className = "tick";
        const wrap = document.createElement("label");
        const box = document.createElement("input");
        box.type = "checkbox";
        box.setAttribute("data-col", col);
        box.setAttribute("aria-label", NAMES[col] + " every job in " + label);
        wrap.appendChild(box);
        cell.appendChild(wrap);
        head.appendChild(cell);
        group.box[col] = box;
      }
      groups.push(group);
    }

    const needsView = (row) => {
      if (row.box.view && (row.box.build.checked || row.box.cancel.checked)) row.box.view.checked = true;
    };

    const shown = (group) => group.rows.filter((row) => !row.tr.hidden);

    const refresh = () => {
      for (const group of groups) {
        const rows = shown(group);
        for (const col of COLS) {
          const ticked = rows.filter((row) => row.box[col].checked).length;
          group.box[col].checked = ticked > 0 && ticked === rows.length;
          group.box[col].indeterminate = ticked > 0 && ticked < rows.length;
        }
      }
      if (!bar || !count) return;
      let changed = 0;
      for (const row of rows.values()) {
        if (COLS.some((col) => row.box[col].checked !== row.box[col].defaultChecked)) changed++;
      }
      bar.classList.toggle("dirty", changed > 0);
      count.hidden = changed === 0;
      count.textContent = changed + (changed === 1 ? " job changed" : " jobs changed");
    };

    table.addEventListener("change", (event) => {
      const box = event.target;
      const col = box.getAttribute("data-col");
      if (col) {
        const group = groups.find((g) => g.body === box.closest("tbody"));
        if (group) {
          for (const row of shown(group)) {
            row.box[col].checked = box.checked;
            needsView(row);
          }
        }
      } else {
        const row = rows.get(box.closest("tr"));
        if (row) needsView(row);
      }
      refresh();
    });

    if (scroll) {
      const input = document.createElement("input");
      input.type = "search";
      input.className = "entry";
      input.placeholder = "Filter by name";
      input.setAttribute("aria-label", "Filter jobs by name");
      input.setAttribute("autocomplete", "off");
      const none = document.createElement("p");
      none.className = "sub";
      none.setAttribute("role", "status");
      none.textContent = "No job matches";
      none.hidden = true;
      scroll.before(input, none);

      const apply = () => {
        const text = input.value.trim().toLowerCase();
        const hit = new Map();
        let any = 0;
        for (const row of rows.values()) {
          const ok = row.name.toLowerCase().includes(text);
          hit.set(row.name, ok);
          row.tr.hidden = !ok;
          if (ok) any++;
        }
        for (const warn of table.querySelectorAll("tr[data-warn-of]")) warn.hidden = !hit.get(warn.getAttribute("data-warn-of"));
        for (const group of groups) group.head.hidden = shown(group).length === 0;
        none.hidden = any > 0;
        refresh();
      };
      input.addEventListener("input", apply);
      input.addEventListener("keydown", (event) => {
        if (event.key !== "Escape" || input.value === "") return;
        event.preventDefault();
        input.value = "";
        apply();
      });
    }

    refresh();
  }
})();
