// A name filter for the job list. The page lists every job as it is without
// this script, so the field is made here and exists only where it works.
// Typing narrows the rows to the jobs whose name holds the text (any case)
// and hides a group left with none; "/" goes to the field and Escape empties
// it. #jobs is swapped for a fresh copy every minute and when the queue
// changes, which has every row visible again, so the text is applied to each
// copy as it arrives.
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
  document.addEventListener("htmx:afterSwap", apply);
  apply();
})();
