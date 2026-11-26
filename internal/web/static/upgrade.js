(() => {
  const POLL_MS = 3000;
  const RECONNECT_MS = 2000;
  const GIVE_UP_MS = 35 * 60 * 1000;
  const SNOOZE_S = 7 * 24 * 3600;
  const STATUS = "/api/v1/admin/upgrade";
  const HEALTH = "/api/v1/health";
  const WORKING = ["preflight", "downloading", "verifying", "waiting", "backup"];

  const watch = document.querySelector("#upgrade-watch");
  if (!watch) return;

  const snooze = watch.querySelector("[data-snooze]");
  if (snooze) {
    snooze.hidden = false;
    snooze.addEventListener("click", () => {
      const secure = location.protocol === "https:" ? "; Secure" : "";
      document.cookie = `kipp_upgrade_snooze=${snooze.getAttribute("data-snooze")}; Path=/; Max-Age=${SNOOZE_S}; SameSite=Lax${secure}`;
      const banner = watch.querySelector("#upgrade-banner");
      if (banner) banner.hidden = true;
    });
  }

  const shown = {
    state: watch.getAttribute("data-state"),
    running: watch.getAttribute("data-running"),
    queued: watch.getAttribute("data-queued"),
  };
  const live = watch.querySelector("#upgrade-live");
  const say = (text) => {
    if (!live) return;
    live.textContent = text;
    live.hidden = false;
  };
  const status = async () => {
    const r = await fetch(STATUS, { credentials: "same-origin", headers: { Accept: "application/json" } });
    if (r.status === 401 || r.status === 403) return { gone: true };
    if (!r.ok) throw new Error(`status ${r.status}`);
    return r.json();
  };

  let since = null;
  const reconnect = () => {
    if (since !== null) return;
    since = Date.now();
    if (shown.state !== "switching") say("Restarting, this page reconnects by itself.");
    const ask = async () => {
      if (Date.now() - since > GIVE_UP_MS) {
        say("kipp did not come back within 35 minutes. Look at the server and its log, then reload this page.");
        return;
      }
      try {
        const h = await fetch(HEALTH, { cache: "no-store" });
        if (h.ok) {
          const s = await status().catch(() => null);
          if (s && (s.gone || s.state !== "switching")) {
            location.reload();
            return;
          }
        }
      } catch {
      }
      setTimeout(ask, RECONNECT_MS);
    };
    setTimeout(ask, RECONNECT_MS);
  };

  const follow = () => {
    setTimeout(async () => {
      let s;
      try {
        s = await status();
      } catch {
        reconnect();
        return;
      }
      if (s.gone) {
        location.reload();
      } else if (s.state === "switching") {
        reconnect();
      } else if (s.state !== shown.state || String(s.running) !== shown.running || String(s.queued) !== shown.queued) {
        location.reload();
      } else {
        follow();
      }
    }, POLL_MS);
  };

  if (shown.state === "switching") reconnect();
  else if (WORKING.includes(shown.state)) follow();
})();
