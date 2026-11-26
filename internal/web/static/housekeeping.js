const KEY = "kipp.housekeeping.tab";

const area = document.getElementById("tabs");
const list = area && area.querySelector('[role="tablist"]');
if (list) {
  const tabs = Array.from(list.querySelectorAll('[role="tab"]'));
  const names = tabs.map((t) => t.getAttribute("data-tab"));

  const show = (i) => {
    tabs.forEach((tab, j) => {
      tab.setAttribute("aria-selected", j === i);
      tab.setAttribute("tabindex", j === i ? "0" : "-1");
      document.getElementById(tab.getAttribute("aria-controls")).hidden = j !== i;
    });
  };

  const remember = (name) => {
    try {
      localStorage.setItem(KEY, name);
    } catch {
    }
  };

  const remembered = () => {
    try {
      return localStorage.getItem(KEY);
    } catch {
      return null;
    }
  };

  const fromHash = names.indexOf(location.hash.slice(1));
  if (fromHash >= 0) remember(names[fromHash]);
  const kept = names.indexOf(remembered());
  show(fromHash >= 0 ? fromHash : Math.max(kept, 0));

  tabs.forEach((tab, i) => {
    tab.addEventListener("click", () => {
      show(i);
      remember(names[i]);
    });
  });

  list.addEventListener("keydown", (e) => {
    const at = tabs.indexOf(e.target);
    const to = { ArrowRight: at + 1, ArrowLeft: at - 1, Home: 0, End: tabs.length - 1 }[e.key];
    if (at < 0 || to === undefined) return;
    e.preventDefault();
    const i = (to + tabs.length) % tabs.length;
    show(i);
    remember(names[i]);
    tabs[i].focus();
  });

  area.classList.add("tabbed");
  list.hidden = false;
}
