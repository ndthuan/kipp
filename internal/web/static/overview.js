const root = document.getElementById("overview");

if (root) {
  const tabs = () => Array.from(root.querySelectorAll('[role="tab"]'));

  const select = (name) => {
    tabs().forEach((tab) => {
      const on = tab.getAttribute("data-tab") === name;
      tab.setAttribute("aria-selected", on);
      tab.setAttribute("tabindex", on ? "0" : "-1");
    });
  };

  const mark = () => {
    const panel = document.getElementById("overview-panel");
    if (panel) select(panel.getAttribute("data-tab"));
  };

  document.body.addEventListener("htmx:afterSettle", mark);

  document.body.addEventListener("htmx:configRequest", (e) => {
    if (e.detail.elt.id !== "overview") return;
    const open = tabs().find((tab) => tab.getAttribute("aria-selected") === "true");
    const range = root.querySelector(".charts [aria-current]");
    if (open) e.detail.parameters.tab = open.getAttribute("data-tab");
    if (range) e.detail.parameters.window = range.getAttribute("data-range");
  });

  root.addEventListener("click", (e) => {
    const tab = e.target.closest('[role="tab"]');
    if (tab) select(tab.getAttribute("data-tab"));
  });

  root.addEventListener("keydown", (e) => {
    const all = tabs();
    const at = all.indexOf(e.target);
    const to = { ArrowRight: at + 1, ArrowLeft: at - 1, Home: 0, End: all.length - 1 }[e.key];
    if (at < 0 || to === undefined) return;
    e.preventDefault();
    const next = all[(to + all.length) % all.length];
    next.focus();
    next.click();
  });
}
