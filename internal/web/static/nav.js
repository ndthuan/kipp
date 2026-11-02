// The header's menus and the Build button's form are <details>, which open
// and close without this script. It adds what <details> does not do: one
// open at a time, closed again by a click elsewhere or by Escape, and on a
// narrow screen the groups folded behind one button. Without it the groups
// stay unfolded, and every menu still opens.
(() => {
  const top = document.querySelector(".top");
  const toggle = document.querySelector(".menu-toggle");

  const fold = (open) => {
    toggle.setAttribute("aria-expanded", String(open));
    top.classList.toggle("open", open);
  };
  if (top && toggle) {
    toggle.hidden = false;
    top.classList.add("folding");
    toggle.addEventListener("click", () => fold(toggle.getAttribute("aria-expanded") !== "true"));
  }

  const closeAll = (except) => {
    for (const d of document.querySelectorAll("details.drop[open]")) {
      if (d !== except) d.open = false;
    }
  };

  document.addEventListener("click", (e) => closeAll(e.target.closest?.("details.drop") ?? null));

  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    const inside = e.target.closest?.("details.drop[open]");
    closeAll(null);
    if (inside) {
      inside.querySelector("summary").focus();
    } else if (toggle && top.classList.contains("open")) {
      fold(false);
      toggle.focus();
    }
  });
})();
