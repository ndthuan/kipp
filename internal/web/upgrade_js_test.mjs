// Runs static/upgrade.js against a false page, clock and server with node
// alone: `make quality` calls it when node is on PATH. It holds that
// "Remind me later" puts the banner off for 7 days in a cookie, that a run
// being watched reloads the page when its status changes, that a restart is
// waited out by asking the health route and the page reloads once the
// status no longer says "switching", and that the waiting ends with a
// message after 35 minutes instead of going on for ever.
import fs from "node:fs";
import assert from "node:assert/strict";

const script = fs.readFileSync(process.argv[2] || new URL("static/upgrade.js", import.meta.url), "utf8");

class El {
  constructor() {
    this.attrs = {};
    this.listeners = {};
    this.hidden = true;
    this.textContent = "";
    this.children = {};
  }
  getAttribute(k) { return this.attrs[k] ?? null; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  querySelector(sel) { return this.children[sel] ?? null; }
}

const flush = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

function page({ state = "", running = "0", queued = "0", snooze = true, protocol = "https:", noWatch = false } = {}) {
  let now = Date.parse("2026-11-08T10:00:00Z");
  const timers = [];
  const cookies = [];
  const log = { reloads: 0, fetched: [] };
  const server = { health: () => ({ ok: true }), status: () => ({ ok: true, status: 200, json: async () => ({ state, running: Number(running), queued: Number(queued) }) }) };

  const watch = new El();
  watch.attrs = { "data-state": state, "data-running": running, "data-queued": queued };
  const live = new El();
  const banner = new El();
  banner.hidden = false;
  const button = new El();
  button.attrs["data-snooze"] = "v1.2.0";
  watch.children = { "#upgrade-live": live, "#upgrade-banner": banner };
  if (snooze) watch.children["[data-snooze]"] = button;

  const context = {
    Date: { now: () => now },
    setTimeout: (fn, ms) => timers.push({ fn, at: now + ms }),
    document: {
      querySelector: (sel) => (sel === "#upgrade-watch" && !noWatch ? watch : null),
      set cookie(v) { cookies.push(v); },
    },
    location: { protocol, reload: () => { log.reloads++; } },
    fetch: async (url) => {
      log.fetched.push(url);
      if (url === "/api/v1/health") return server.health();
      return server.status();
    },
  };
  new Function(...Object.keys(context), script)(...Object.values(context));

  const advance = async (ms) => {
    const end = now + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      if (!timers.length || timers[0].at > end) break;
      const t = timers.shift();
      now = t.at;
      t.fn();
      await flush();
    }
    now = end;
  };
  return { watch, live, banner, button, cookies, log, server, advance };
}

{
  const p = page({ state: "idle" });
  assert.equal(p.button.hidden, false, "the snooze button shows once the script runs");
  p.button.listeners.click();
  assert.equal(p.cookies.length, 1);
  assert.equal(p.cookies[0], "kipp_upgrade_snooze=v1.2.0; Path=/; Max-Age=604800; SameSite=Lax; Secure", "the release is put off for 7 days, Secure over https");
  assert.equal(p.banner.hidden, true, "and the banner goes at once");
}

{
  const p = page({ state: "idle", protocol: "http:" });
  p.button.listeners.click();
  assert.equal(p.cookies[0], "kipp_upgrade_snooze=v1.2.0; Path=/; Max-Age=604800; SameSite=Lax", "not Secure over http");
}

{
  page({ noWatch: true });
  const p = page({ state: "idle" });
  await p.advance(60000);
  assert.deepEqual(p.log.fetched, [], "an idle page asks nothing");
}

{
  const p = page({ state: "waiting", running: "2", queued: "1" });
  await p.advance(10000);
  assert.equal(p.log.reloads, 0, "a run that has not changed is not reloaded");
  assert.ok(p.log.fetched.every((u) => u === "/api/v1/admin/upgrade"));
  p.server.status = () => ({ ok: true, status: 200, json: async () => ({ state: "waiting", running: 1, queued: 1 }) });
  await p.advance(4000);
  assert.equal(p.log.reloads, 1, "the counts moving reload the page");
}

{
  const p = page({ state: "waiting", running: "0", queued: "0" });
  p.server.status = () => ({ ok: true, status: 200, json: async () => ({ state: "backup", running: 0, queued: 0 }) });
  await p.advance(4000);
  assert.equal(p.log.reloads, 1, "a new step reloads the page");
}

{
  // The restart: the status says switching, the server goes away and comes back.
  const p = page({ state: "waiting" });
  p.server.status = () => ({ ok: true, status: 200, json: async () => ({ state: "switching", running: 0, queued: 0 }) });
  await p.advance(4000);
  assert.equal(p.log.reloads, 0, "switching does not reload the page, the server is about to go");
  assert.equal(p.live.hidden, false);
  assert.match(p.live.textContent, /reconnects by itself/);
  p.server.health = () => { throw new TypeError("connection refused"); };
  await p.advance(20000);
  assert.equal(p.log.reloads, 0, "no reload while the server is down");
  p.server.health = () => ({ ok: true });
  await p.advance(2500);
  assert.equal(p.log.reloads, 0, "the old server still answering, with the status still switching, is not the new one");
  p.server.status = () => ({ ok: true, status: 200, json: async () => ({ state: "done", running: 0, queued: 0 }) });
  await p.advance(2500);
  assert.equal(p.log.reloads, 1, "the new version answering reloads the page");
  await p.advance(60000);
  assert.equal(p.log.reloads, 1, "once");
}

{
  // A page drawn while the restart is on goes straight to reconnecting.
  const p = page({ state: "switching" });
  p.server.health = () => { throw new TypeError("down"); };
  await p.advance(6000);
  p.server.health = () => ({ ok: true });
  p.server.status = () => ({ ok: false, status: 401, json: async () => ({}) });
  await p.advance(2500);
  assert.equal(p.log.reloads, 1, "a login asked for after the restart reloads the page to it");
}

{
  // The server never returns: it stops after 35 minutes, with a message.
  const p = page({ state: "switching" });
  p.server.health = () => { throw new TypeError("down"); };
  await p.advance(34 * 60 * 1000);
  const before = p.log.fetched.length;
  assert.equal(p.live.hidden, true, "no message before the cap");
  await p.advance(2 * 60 * 1000);
  assert.equal(p.live.hidden, false);
  assert.match(p.live.textContent, /did not come back within 35 minutes/);
  const after = p.log.fetched.length;
  assert.ok(after > before, "it kept asking until the cap");
  await p.advance(10 * 60 * 1000);
  assert.equal(p.log.fetched.length, after, "and asks no more");
  assert.equal(p.log.reloads, 0);
}

console.log("upgrade.js: ok");
