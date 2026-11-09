// The grants form: build and cancel need view, because a job a user may not
// see answers them 404 and the grant would do nothing. The server refuses
// the combination, so this only saves the admin the round trip: ticking
// build or cancel ticks view, and view stays ticked while either is.
(() => {
  for (const table of document.querySelectorAll("table[data-needs-view]")) {
    table.addEventListener("change", (event) => {
      const box = event.target;
      const row = box.closest("tr");
      if (!row) return;
      const view = row.querySelector('input[name="view"]');
      const others = ["build", "cancel"].map((name) => row.querySelector('input[name="' + name + '"]'));
      if (view && others.some((other) => other && other.checked)) view.checked = true;
    });
  }
})();
