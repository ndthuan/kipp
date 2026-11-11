// Runs static/queue.js against a false page, clock and EventSource with
// node alone: `make quality` calls it when node is on PATH. It holds that
// an event refreshes the page once however many come together, that a
// stream that is down is made up for by polling and by one refresh when it
// is back, and that the times count.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/queue.js", import.meta.url), "utf8");

function page() {
  let now = Date.parse("2026-10-14T10:00:00Z");
  const timers = new Map();
  let next = 1;
  const add = (fn, ms, repeat) => {
    const id = next++;
    timers.set(id, { fn, at: now + ms, ms, repeat });
    return id;
  };
  const advance = (ms) => {
    const end = now + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      const [id, t] = due;
      now = t.at;
      if (t.repeat) t.at += t.ms;
      else timers.delete(id);
      t.fn();
    }
    now = end;
  };
  const refreshed = [];
  const el = (datetime, ago) => ({ attrs: { datetime, "data-ago": ago }, textContent: "", getAttribute(k) { return this.attrs[k] ?? null; } });
  const elapsed = el("2026-10-14T09:56:48Z");
  const since = el("2026-10-14T06:54:00Z");
  const ended = el("2026-10-14T09:02:00Z", "1");
  const mk = (datetime, abs) => ({ attrs: { datetime, "data-abs": abs }, textContent: "", getAttribute(k) { return this.attrs[k]; } });
  const past = mk("2026-10-13T10:30:00Z", "2026-10-13 17:30");
  const future = mk("2026-10-15T10:20:00Z", "2026-10-15 17:20");
  let stream = null;
  class FakeEventSource {
    constructor(url) {
      this.url = url;
      this.listeners = {};
      stream = this;
    }
    addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); }
    emit(type) { for (const fn of this.listeners[type] ?? []) fn({}); }
  }
  const context = {
    Date: { now: () => now, parse: Date.parse },
    setTimeout: (fn, ms) => add(fn, ms, false),
    setInterval: (fn, ms) => add(fn, ms, true),
    clearInterval: (id) => timers.delete(id),
    EventSource: FakeEventSource,
    CustomEvent: class { constructor(type) { this.type = type; } },
    document: {
      querySelectorAll: (sel) => ({ "time.elapsed": [elapsed], "time.since": [since], "time.past": [ended], "time.when": [past, future] })[sel] ?? [],
      body: { dispatchEvent: (e) => refreshed.push(e.type) },
    },
  };
  new Function(...Object.keys(context), script)(...Object.values(context));
  return { stream, refreshed, elapsed, since, ended, past, future, advance };
}

{
  const { stream, refreshed } = page();
  assert.equal(stream.url, "/api/v1/queue/events", "it listens to the queue's events");
  assert.equal(refreshed.length, 0, "nothing is refreshed at first");
}

{
  const { stream, refreshed, advance } = page();
  stream.emit("open");
  stream.emit("changed");
  stream.emit("changed");
  stream.emit("changed");
  advance(1000);
  assert.deepEqual(refreshed, ["kipp-changed"], "three events together are one refresh");
  stream.emit("changed");
  advance(1000);
  assert.equal(refreshed.length, 2, "a later event is another");
}

{
  const { stream, refreshed, advance } = page();
  stream.emit("open");
  stream.emit("error");
  advance(29000);
  assert.equal(refreshed.length, 0, "no poll before 30 seconds");
  advance(2000);
  assert.equal(refreshed.length, 1, "a stream that is down is made up for every 30 seconds");
  stream.emit("error");
  advance(30500);
  assert.equal(refreshed.length, 2, "one poll at a time, however many errors");
  stream.emit("open");
  advance(1000);
  assert.equal(refreshed.length, 3, "the stream back is one refresh");
  advance(120000);
  assert.equal(refreshed.length, 3, "and the polling stops");
}

{
  const { elapsed, since, ended, advance } = page();
  advance(1000);
  assert.equal(elapsed.textContent, "3m 13s", "a live elapsed time counts on its own, with seconds");
  advance(1000);
  assert.equal(elapsed.textContent, "3m 14s", "every second");
  assert.equal(since.textContent, "", "a past moment is not touched before the minute is up");
  assert.equal(ended.textContent, "");
  advance(58000);
  assert.equal(since.textContent, "3h 7m", "a state that holds is a duration with no seconds");
  assert.equal(ended.textContent, "59m ago", "a moment that is over says ago");
  advance(59000);
  assert.equal(since.textContent, "3h 7m", "and stays until the next minute");
  advance(3600 * 1000);
  assert.equal(elapsed.textContent, "1h 5m", "an elapsed time past the hour");
  assert.equal(since.textContent, "4h 7m");
  advance(7 * 86400 * 1000);
  assert.equal(since.textContent, "7d", "from a day on it is days");
  assert.equal(ended.textContent, "7d ago");
  assert.doesNotMatch(since.textContent + ended.textContent, /\ds\b/, "never seconds");
}

{
  const { past, future, advance } = page();
  advance(60000);
  assert.equal(past.textContent, "23h 31m ago", "a time within a day past is relative, with no seconds");
  assert.equal(future.textContent, "2026-10-15 17:20", "a time more than a day ahead stays absolute");
  advance(31 * 60 * 1000);
  assert.equal(past.textContent, "2026-10-13 17:30", "a past time a day old turns absolute");
  assert.equal(future.textContent, "in 23h 48m", "a future time within a day turns relative");
  advance(23 * 3600 * 1000 + 40 * 60 * 1000);
  assert.equal(future.textContent, "in 8m");
  advance(8 * 60 * 1000);
  assert.equal(future.textContent, "in <1m", "under a minute reads in <1m");
}

console.log("queue.js: ok");
