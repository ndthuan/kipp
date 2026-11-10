// What runs and what waits, kept current. The server sends an event called
// changed down /api/v1/queue/events whenever the queue the viewer may see
// changes; this script turns it into a "kipp-changed" event on the body,
// and every part of the page that shows the queue (hx-trigger="kipp-changed
// from:body") asks for itself again. While the stream is down, as when the
// server restarts, it asks every 30 seconds instead, and once more when the
// stream is back. It also keeps every <time class="since"> and <time class="when"> counting.
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


  // A <time class="when"> reads relative within 24 hours of now, past as
  // "3h 5m ago" and future as "in 9m", and as its data-abs text beyond that;
  // the server writes the same choice (whenIn in pages.go).
  const DAY_MS = 86400000;
  const whenText = (t) => {
    const at = Date.parse(t.getAttribute("datetime"));
    if (Number.isNaN(at)) return null;
    const d = at - Date.now();
    if (d <= -DAY_MS || d >= DAY_MS) return t.getAttribute("data-abs");
    return d < 0 ? `${short(-d)} ago` : `in ${short(d)}`;
  };

  const tick = () => {
    for (const t of document.querySelectorAll("time.since")) {
      const at = Date.parse(t.getAttribute("datetime"));
      if (!Number.isNaN(at)) t.textContent = short(Date.now() - at);
    }
    for (const t of document.querySelectorAll("time.when")) {
      const text = whenText(t);
      if (text !== null) t.textContent = text;
    }
  };
  setInterval(tick, 1000);

  if (typeof EventSource === "undefined") return;

  // Several changes in a row, as a build that starts, are one refresh.
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
    // What changed while the stream was down was told to nobody.
    if (opened) refresh();
    opened = true;
  });
  stream.addEventListener("error", () => {
    if (poll === null) poll = setInterval(refresh, POLL_MS);
  });
})();
