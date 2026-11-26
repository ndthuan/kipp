(() => {
  const POLL_MS = 30000;
  const SETTLE_MS = 300;

  const short = (ms) => {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
    if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
    return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
  };

  const coarse = (ms) => {
    const m = Math.floor(Math.max(0, ms) / 60000);
    if (m < 1) return "<1m";
    if (m < 60) return `${m}m`;
    if (m < 1440) return `${Math.floor(m / 60)}h ${m % 60}m`;
    return `${Math.floor(m / 1440)}d`;
  };

  const DAY_MS = 86400000;
  const whenText = (t) => {
    const at = Date.parse(t.getAttribute("datetime"));
    if (Number.isNaN(at)) return null;
    const d = at - Date.now();
    if (d <= -DAY_MS || d >= DAY_MS) return t.getAttribute("data-abs");
    return d < 0 ? `${coarse(-d)} ago` : `in ${coarse(d)}`;
  };

  const sinceText = (t) => {
    const at = Date.parse(t.getAttribute("datetime"));
    if (Number.isNaN(at)) return null;
    const text = coarse(Date.now() - at);
    return t.getAttribute("data-ago") === null ? text : `${text} ago`;
  };

  const each = (sel, text) => {
    for (const t of document.querySelectorAll(sel)) {
      const v = text(t);
      if (v !== null) t.textContent = v;
    }
  };

  setInterval(() => each("time.elapsed", (t) => {
    const at = Date.parse(t.getAttribute("datetime"));
    return Number.isNaN(at) ? null : short(Date.now() - at);
  }), 1000);
  setInterval(() => {
    each("time.since", sinceText);
    each("time.past", sinceText);
    each("time.when", whenText);
  }, 60000);

  if (typeof EventSource === "undefined") return;

  let pending = null;
  const refresh = () => {
    if (pending !== null) return;
    pending = setTimeout(() => {
      pending = null;
      document.body.dispatchEvent(new CustomEvent("kipp-changed"));
    }, SETTLE_MS);
  };

  let poll = null;
  let opened = false;
  const stream = new EventSource("/api/v1/queue/events");
  stream.addEventListener("changed", refresh);
  stream.addEventListener("open", () => {
    if (poll !== null) {
      clearInterval(poll);
      poll = null;
    }
    if (opened) refresh();
    opened = true;
  });
  stream.addEventListener("error", () => {
    if (poll === null) poll = setInterval(refresh, POLL_MS);
  });
})();
