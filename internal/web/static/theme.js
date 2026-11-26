(() => {
  const group = document.querySelector(".theme");
  if (!group) return;
  const summary = group.querySelector("summary");
  const root = document.documentElement;
  const picks = group.querySelectorAll("[data-theme-pick]");
  const secure = location.protocol === "https:" ? "; Secure" : "";

  const pick = (theme) => {
    const held = theme === "light" || theme === "dark";
    if (held) {
      root.setAttribute("data-theme", theme);
      document.cookie = `kipp_theme=${theme}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
    } else {
      root.removeAttribute("data-theme");
      document.cookie = `kipp_theme=; Path=/; Max-Age=0; SameSite=Lax${secure}`;
    }
    summary.setAttribute("data-now", held ? theme : "system");
    group.open = false;
    for (const b of picks) b.setAttribute("aria-pressed", String(b.getAttribute("data-theme-pick") === theme));
  };

  group.hidden = false;
  group.addEventListener("click", (e) => {
    const b = e.target.closest?.("[data-theme-pick]");
    if (b) pick(b.getAttribute("data-theme-pick"));
  });
})();
