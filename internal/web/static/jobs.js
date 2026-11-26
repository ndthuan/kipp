(() => {
  const bar = document.getElementById("jobs-bar");
  if (!bar) return;

  const input = document.createElement("input");
  input.type = "search";
  input.className = "entry";
  input.placeholder = "Filter by name (/)";
  input.setAttribute("aria-label", "Filter jobs by name");
  input.setAttribute("autocomplete", "off");
  const none = document.createElement("p");
  none.className = "sub";
  none.setAttribute("role", "status");
  none.textContent = "No job matches";
  none.hidden = true;
  bar.prepend(input);
  bar.after(none);

  const apply = () => {
    const text = input.value.trim().toLowerCase();
    const list = document.getElementById("jobs");
    if (!list) return;
    let shown = 0;
    for (const section of list.querySelectorAll("section")) {
      let rows = 0;
      for (const row of section.querySelectorAll("tr[data-name]")) {
        const hit = row.getAttribute("data-name").toLowerCase().includes(text);
        row.hidden = !hit;
        if (hit) rows++;
      }
      section.hidden = rows === 0;
      shown += rows;
    }
    none.hidden = text === "" || shown > 0;
  };

  const typing = (el) => !!el && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName));

  input.addEventListener("input", apply);
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || input.value === "") return;
    e.preventDefault();
    input.value = "";
    apply();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey || typing(document.activeElement)) return;
    e.preventDefault();
    input.focus();
  });
  const tabs = () => Array.from(bar.querySelectorAll('[role="tab"]'));

  const select = (status) => {
    for (const tab of tabs()) {
      const on = tab.getAttribute("data-status") === status;
      tab.setAttribute("aria-selected", on);
      tab.setAttribute("tabindex", on ? "0" : "-1");
    }
  };

  const mark = () => {
    const list = document.getElementById("jobs");
    if (list) select(list.getAttribute("data-status") ?? "");
  };

  bar.addEventListener("click", (e) => {
    const tab = e.target.closest('[role="tab"]');
    if (tab) select(tab.getAttribute("data-status"));
  });

  bar.addEventListener("keydown", (e) => {
    const all = tabs();
    const at = all.indexOf(e.target);
    const to = { ArrowRight: at + 1, ArrowLeft: at - 1, Home: 0, End: all.length - 1 }[e.key];
    if (at < 0 || to === undefined) return;
    e.preventDefault();
    const next = all[(to + all.length) % all.length];
    next.focus();
    next.click();
  });

  document.addEventListener("htmx:afterSwap", () => {
    mark();
    apply();
  });
  apply();
})();
