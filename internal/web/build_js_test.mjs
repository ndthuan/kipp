import fs from "node:fs";

const script = process.argv[2] || new URL("static/build.js", import.meta.url);

const opened = [];
class FakeEventSource {
  static CLOSED = 2;
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.listeners = {};
    opened.push(this);
  }
  addEventListener(name, fn) {
    this.listeners[name] = fn;
  }
  close() {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function fakePre(url) {
  const pre = { dataset: { log: url }, childNodes: [], scrollHeight: 0, scrollTop: 0, clientHeight: 0, classes: new Set() };
  pre.classList = { toggle: (name, on) => (on ? pre.classes.add(name) : pre.classes.delete(name)) };
  pre.append = (node) => {
    node.remove = () => pre.childNodes.splice(pre.childNodes.indexOf(node), 1);
    pre.childNodes.push(node);
  };
  Object.defineProperty(pre, "firstChild", { get: () => pre.childNodes[0] || null });
  Object.defineProperty(pre, "lastChild", { get: () => pre.childNodes[pre.childNodes.length - 1] || null });
  pre.text = () => pre.childNodes.map((n) => n.textContent).join("");
  pre.times = () => pre.childNodes.map((n) => n.dataset.t || "-").join(" ");
  return pre;
}

function fakeNode() {
  const node = { dataset: {}, className: "", children: [], text: "" };
  Object.defineProperty(node, "textContent", {
    get: () => (node.children.length ? node.children.map((c) => c.textContent).join("") : node.text),
    set: (value) => {
      node.text = String(value);
      node.children = [];
    },
  });
  node.append = (...parts) => node.children.push(...parts);
  node.focus = (how) => focused.push({ node, how });
  node.scrollIntoView = (how) => scrolled.push({ node, how });
  return node;
}

const focused = [];
const scrolled = [];

function fakeLog(name) {
  const pre = fakePre("/api/v1/builds/1/stages/" + name + "/log");
  const cut = { hidden: true, textContent: "" };
  const details = {
    scrollIntoView: (how) => revealed.push(name + " " + how.block),
    id: "log-" + name,
    open: true,
    pre,
    cut,
    chip: { className: "", textContent: "" },
    problems: { hidden: true },
    count: { textContent: "" },
    buttons: [-1, 1].map((step) => ({ dataset: { problem: String(step) }, addEventListener: (_, fn) => (details["click" + step] = fn) })),
    querySelector: (sel) =>
      ({ "pre[data-log]": pre, "[data-cut]": cut, "[data-chip]": details.chip, "[data-problems]": details.problems, "[data-count]": details.count })[sel] || null,
    querySelectorAll: (sel) => (sel === "[data-problem]" ? details.buttons : []),
    addEventListener() {},
  };
  return details;
}

const logs = { s: fakeLog("s"), big: fakeLog("big"), prob: fakeLog("prob") };
logs.prob.open = false;
const sizes = { s: 0, big: 5 * 1024 * 1024 };
const asked = [];
globalThis.fetch = (url, init) => {
  asked.push(init.method + " " + url);
  const name = url.split("/")[6];
  return Promise.resolve({ headers: { get: (h) => (h === "X-Kipp-Size" ? String(sizes[name]) : null) } });
};
const live = { dataset: { logs: "s", open: "s" } };
const revealed = [];
const windowListeners = {};
globalThis.window = { addEventListener: (name, fn) => (windowListeners[name] = fn) };
globalThis.requestAnimationFrame = (fn) => fn();
let afterSwap;
let shown = [logs.s, logs.prob];
globalThis.EventSource = FakeEventSource;
const tails = {};
const boxes = [{ checked: false, addEventListener: (_, fn) => (boxes.changed = fn) }];
const select = (selector) => {
  if (selector === "details.log[open]") return shown.filter((d) => d.open);
  if (selector === "pre[data-log]") return shown.map((d) => d.pre);
  if (selector === "input[data-times]") return boxes;
  return shown;
};
globalThis.document = {
  querySelector: (sel) => tails[/data-tail-of="(.*)"/.exec(sel)?.[1]] || null,
  createElement: () => fakeNode(),
  createTextNode: (text) => ({ textContent: text }),
  querySelectorAll: select,
  getElementById: (id) => (id === "live" ? live : logs[id.slice(4)]),
  body: { addEventListener: (_, fn) => (afterSwap = fn) },
};
globalThis.location = {
  reload() {
    throw new Error("the page was loaded again");
  },
};

function check(ok, what) {
  if (!ok) {
    console.error("build.js: FAILED: " + what);
    process.exit(1);
  }
}

const settle = () => new Promise((resolve) => setImmediate(resolve));

new Function(fs.readFileSync(script, "utf8"))();
await settle();

const pre = logs.s.pre;
check(asked.length === 1 && asked[0] === "HEAD /api/v1/builds/1/stages/s/log", "the size of a log is asked for before it is followed");
check(opened.length === 1 && opened[0].url.endsWith("?offset=0&timestamps=true"), "a short log is followed from its start, with its times");
check(opened[0].url.startsWith("/api/v1/builds/1/stages/s/log/events?"), "the stream is the log's events, got " + opened[0].url);
check(boxes[0].checked && pre.classes.has("times"), "the times are shown until the reader hides them");
check(logs.s.cut.hidden, "a log shown whole says nothing of a cut");
const first = opened[0];
first.onmessage({ data: "line", lastEventId: "5" });
check(pre.text() === "line\n" && pre.dataset.offset === "5", "a line is appended and its offset kept");
check(pre.times() === "-", "a line no time came for carries none");
check(revealed.length === 1 && revealed[0] === "s nearest", "the page is taken to the open log, not to its end, got " + revealed);

first.onmessage({ data: "10%", lastEventId: "5" });
first.onmessage({ data: "20%", lastEventId: "5" });
check(pre.text() === "line\n20%\n" && pre.dataset.offset === "5", "of a run of progress lines the last one is shown");
first.onmessage({ data: "done", lastEventId: "20" });
check(pre.text() === "line\ndone\n" && pre.dataset.offset === "20", "the piece that ends the line takes the place of the progress before it");
first.onmessage({ data: "next", lastEventId: "25" });
check(pre.text() === "line\ndone\nnext\n", "a line after the progress is a line of its own");

const at = (h, m, s) => new Date(2026, 9, 14, h, m, s).getTime();
first.listeners.time({ data: new Date(at(9, 5, 7)).toISOString() });
first.onmessage({ data: "50%", lastEventId: "25" });
first.onmessage({ data: "stamped", lastEventId: "33" });
first.onmessage({ data: "bare", lastEventId: "38" });
check(pre.times() === "- - - 09:05:07 -", "a line carries the time that came before it, got " + pre.times());
boxes[0].checked = false;
boxes.changed();
check(!pre.classes.has("times"), "the times are hidden when the box is cleared");
boxes[0].checked = true;
boxes.changed();
check(pre.classes.has("times"), "and shown again when it is ticked");

first.readyState = 1;
check(typeof first.onerror === "function", "the stream has an onerror handler");
first.onerror();
afterSwap();
await settle();
check(opened.length === 1, "a stream the browser is reconnecting is left alone");

first.readyState = FakeEventSource.CLOSED;
first.onerror();
check(pre.stream === null, "a stream the browser closed is forgotten");
afterSwap();
await settle();
check(opened.length === 2 && opened[1].url.endsWith("?offset=38&timestamps=true"), "a dead stream is followed again from its offset");
check(asked.length === 1, "a log followed again is not measured again");

const second = opened[1];
for (let i = 0; i < 12000; i++) second.onmessage({ data: "l" + i, lastEventId: String(100 + i) });
check(pre.childNodes.length === 5000, "the page keeps 5000 lines of a log, got " + pre.childNodes.length);
check(pre.firstChild.textContent === "l7000\n" && pre.lastChild.textContent === "l11999\n", "the lines kept are the last ones");
check(!logs.s.cut.hidden && logs.s.cut.textContent.includes("5000 lines"), "a log that lost lines from its top says so");

second.listeners.end();
afterSwap();
await settle();
check(opened.length === 2 && pre.dataset.done === "1", "a log that ended is not followed again");

shown = [logs.big];
globalThis.document.querySelectorAll = () => shown;
afterSwap();
await settle();
const big = logs.big.pre;
check(opened.length === 3 && opened[2].url.endsWith("?offset=" + (4 * 1024 * 1024) + "&timestamps=true"), "a long log is followed from 1 MiB before its end, got " + opened[2]?.url);
check(!logs.big.cut.hidden && logs.big.cut.textContent.includes("last 1 MiB"), "a log shown from its end says so");
const third = opened[2];
third.onmessage({ data: "the end of a pro", lastEventId: "" });
third.onmessage({ data: "gress line that was cut", lastEventId: "4194400" });
check(big.text() === "" && big.dataset.offset === "4194400", "the line the cut fell in is not shown");
third.onmessage({ data: "whole", lastEventId: "4194406" });
check(big.text() === "whole\n", "the lines after the cut are shown");

logs.big.cut.hidden = true;
const wide = "x".repeat(300 * 1024 - 1);
for (let i = 0; i < 10; i++) third.onmessage({ data: wide, lastEventId: String(5000000 + i) });
check(big.childNodes.length === 3, "the page keeps the lines that fit in 1 MiB, got " + big.childNodes.length);
check(!logs.big.cut.hidden && logs.big.cut.textContent.includes("last 1 MiB"), "a log that lost long lines from its top says so");
const accented = "\u00e9".repeat(400 * 1024);
third.onmessage({ data: accented, lastEventId: "6000000" });
third.onmessage({ data: accented, lastEventId: "6000001" });
check(big.childNodes.length === 1, "the bound counts bytes, not letters, got " + big.childNodes.length + " lines");
third.onmessage({ data: "y".repeat(3 * 1024 * 1024), lastEventId: "6000002" });
check(big.childNodes.length === 1 && big.lastChild.textContent.startsWith("y"), "a line longer than the bound is kept alone");
third.onmessage({ data: "p".repeat(900 * 1024), lastEventId: "6000002" });
third.onmessage({ data: "short", lastEventId: "6000003" });
for (let i = 0; i < 3; i++) third.onmessage({ data: wide, lastEventId: String(7000000 + i) });
check(big.childNodes.length === 4 && big.firstChild.textContent === "short\n", "progress a short line replaced gives its bytes back, got " + big.childNodes.length + " lines");

const late = fakeLog("late");
logs.late = late;
sizes.late = 10;
shown = [late];
afterSwap();
late.pre.stream.close();
late.pre.stream = null;
await settle();
check(opened.length === 3, "a log closed before its size came is not followed");

shown = [];
for (const name of ["first", "second", "quiet"]) {
  logs[name] = fakeLog(name);
  logs[name].open = false;
}
live.dataset.logs = "first second quiet";
live.dataset.open = "first second";
afterSwap();
check(logs.first.open && logs.second.open, "the logs worth reading are opened after a refresh");
check(!logs.quiet.open, "a log the server does not name stays closed");
logs.second.pre.dataset.offset = "0";
shown = [logs.first, logs.second];
afterSwap();
await settle();
revealed.length = 0;
opened[opened.length - 2].onmessage({ data: "a", lastEventId: "2" });
opened[opened.length - 1].onmessage({ data: "b", lastEventId: "2" });
check(revealed.length === 1 && revealed[0] === "second nearest", "the page follows the log opened last, got " + revealed);
logs.second.open = false;
afterSwap();
check(!logs.second.open, "a log the reader closed is not opened again");

windowListeners.wheel();
revealed.length = 0;
opened[opened.length - 1].onmessage({ data: "c", lastEventId: "4" });
check(revealed.length === 0, "the page is not moved after the reader moved it");

const box = logs.second.pre;
box.scrollHeight = 1000;
box.clientHeight = 300;
box.scrollTop = 700;
box.stream.onmessage({ data: "d", lastEventId: "5" });
check(box.scrollTop === box.scrollHeight, "a new line keeps a box that was at its end at its end");
box.scrollTop = 100;
box.stream.onmessage({ data: "e", lastEventId: "6" });
check(box.scrollTop === 100, "a box the reader scrolled up stays where it is when lines arrive");

logs.ansi = fakeLog("ansi");
sizes.ansi = 0;
shown = [logs.ansi];
live.dataset.logs = "ansi";
live.dataset.open = "ansi";
afterSwap();
await settle();
const ansi = logs.ansi.pre;
const feed = opened[opened.length - 1];
let id = 0;
const send = (data) => {
  feed.onmessage({ data, lastEventId: String(++id) });
  return ansi.lastChild;
};
const shape = (node) => node.children.map((c) => (c.className ? "[" + c.className + "]" : "") + c.textContent).join("|");

const bytes = (text) => new TextEncoder().encode(text).length;
const held = () => ansi.held || 0;
let before = held();
let line = send("\x1b[1;31merror\x1b[0m done");
check(shape(line) === "[ansi-b ansi-fg-red]error| done|\n", "bold and red give a span of those classes, got " + shape(line));
check(line.textContent === "error done\n", "the text of a styled line is the text without the escapes");
check(held() - before === bytes("\x1b[1;31merror\x1b[0m done\n"), "the held bytes are those of the raw line, got " + (held() - before));

line = send("\x1b[32mok\x1b[0m plain");
check(shape(line) === "[ansi-fg-green]ok| plain|\n", "a reset in the middle of a line ends the style, got " + shape(line));
line = send("plain again");
check(line.children.length === 0 && line.textContent === "plain again\n", "a style ends with its line and a line with no escape is one text node");
line = send("\x1b[31mred to the end");
line = send("next");
check(line.children.length === 0, "the line after an open style starts at the default");

line = send("\x1b[2Kcleared\x1b[?25l\x1b]0;a title\x07 and\x1b]8;;http://x\x1b\\ more\x1bc!");
check(line.textContent === "cleared and more!\n", "other sequences are removed from the shown text, got " + JSON.stringify(line.textContent));
line = send("abc\x1b[3");
check(line.textContent === "abc[3\n", "a sequence broken at the line's end loses its ESC and keeps the rest, got " + JSON.stringify(line.textContent));
line = send("a\x1b=b\x1b\\c\x1b^d\x1b~e");
check(line.textContent === "abcde\n", "ESC and one final byte from 0 to Z, backslash or ^ to ~ is a two-byte sequence, got " + JSON.stringify(line.textContent));
line = send("p\x1b]q");
check(line.textContent === "p]q\n", "ESC ] with no terminator is no two-byte sequence: only the ESC goes, got " + JSON.stringify(line.textContent));
line = send("p\x1b[");
check(line.textContent === "p[\n", "ESC [ with no final byte is no two-byte sequence: only the ESC goes, got " + JSON.stringify(line.textContent));
line = send("abc\x1b");
check(line.textContent === "abc\n", "a lone ESC is removed");

line = send("\x1b[2;3;4mq\x1b[22;23;24mr");
check(shape(line) === "[ansi-dim ansi-i ansi-u]q|r|\n", "dim, italic and underline are set and cleared, got " + shape(line));
line = send("\x1b[91;102ma\x1b[39mb\x1b[49mc");
check(shape(line) === "[ansi-fg-red-bright ansi-bg-green-bright]a|[ansi-bg-green-bright]b|c|\n", "bright colours, and their own resets, got " + shape(line));
line = send("\x1b[38;5;196mx\x1b[0m\x1b[38;2;10;200;10my\x1b[48;5;21mz");
check(shape(line) === "[ansi-fg-red-bright]x|[ansi-fg-green]y|[ansi-fg-green ansi-bg-blue]z|\n", "256 and true colours map to the nearest of the 16, got " + shape(line));

before = held();
feed.onmessage({ data: "\x1b[33m50%", lastEventId: String(id) });
feed.onmessage({ data: "\x1b[33m60%", lastEventId: String(id) });
check(held() - before === bytes("\x1b[33m60%\n"), "progress replaced counts only its last raw line, got " + (held() - before));
check(shape(ansi.lastChild) === "[ansi-fg-yellow]60%|\n", "the replacement is drawn, got " + shape(ansi.lastChild));

logs.red = fakeLog("red");
sizes.red = 0;
tails.red = { hidden: true, textContent: "" };
shown = [logs.red];
live.dataset.logs = "red";
live.dataset.open = "red";
live.dataset.states = "red=failed";
afterSwap();
await settle();
check(tails.red.hidden, "the tail stays hidden while the log has no line");
check(logs.red.chip.className === "status failed" && logs.red.chip.textContent === "failed", "the heading of a log carries its stage's state, got " + logs.red.chip.className);
const red = opened[opened.length - 1];
let seq = 0;
for (const text of ["a", "", "b", "   ", "c", "d", "e", "f", "g", ""]) red.onmessage({ data: text, lastEventId: String(++seq) });
check(!tails.red.hidden && tails.red.textContent === "c\nd\ne\nf\ng", "the tail is the last 5 lines that are not empty, got " + JSON.stringify(tails.red.textContent));
red.onmessage({ data: "h", lastEventId: String(++seq) });
check(tails.red.textContent === "d\ne\nf\ng\nh", "the tail follows the log as it grows, got " + JSON.stringify(tails.red.textContent));
tails.red = { hidden: true, textContent: "" };
live.dataset.states = "red=cancelled";
afterSwap();
check(!tails.red.hidden && tails.red.textContent === "d\ne\nf\ng\nh", "a refresh fills the notice again, got " + JSON.stringify(tails.red.textContent));
check(logs.red.chip.className === "status cancelled", "the heading follows the state of a refresh");

logs.prob.open = true;
shown = [logs.prob];
live.dataset.logs = "prob";
live.dataset.open = "prob";
live.dataset.states = "";
afterSwap();
await settle();
const prob = logs.prob.pre;
const pfeed = opened[opened.length - 1];
let pid = 0;
const say = (data) => pfeed.onmessage({ data, lastEventId: String(++pid) });
check(logs.prob.problems.hidden, "the problem buttons stay hidden while the log holds no problem");
say("all fine");
say("\x1b[32mgreen is not a problem\x1b[0m");
check(logs.prob.problems.hidden, "plain and green lines are no problem");
const kinds = {
  "\x1b[31mred": true, "\x1b[91mbright red": true, "\x1b[33myellow": true, "\x1b[93mbright yellow": true,
  "\x1b[34mblue": false, "\x1b[1;38;5;196mindexed red": true, "ok \x1b[0m\x1b[1mbold only": false,
  "error: x": true, "Error: x": true, "FAIL pkg": true, "panic: boom": true, "warning: x": true, "WARN x": true,
  "a clean line": false, "\x1b[32mgreen with an error in it": true,
};
for (const [text, bad] of Object.entries(kinds)) {
  say(text);
  check(prob.lastChild.problem === bad, JSON.stringify(text) + (bad ? " is" : " is not") + " a problem");
}
const total = Object.values(kinds).filter(Boolean).length;
check(!logs.prob.problems.hidden && logs.prob.count.textContent === total + " problems", "the count of problems is shown, got " + logs.prob.count.textContent);

const lines = prob.childNodes.filter((n) => n.problem);
check(lines.length === total, "the problem lines are the ones counted");
prob.clientHeight = 300;
prob.scrollTop = 0;
lines[0].offsetTop = 900;
lines[0].offsetHeight = 20;
revealed.length = 0;
logs.prob.click1();
check(prob.scrollTop === 760, "problem navigation puts the line in the middle of the box, got " + prob.scrollTop);
check(revealed.length === 1 && revealed[0] === "prob nearest", "problem navigation moves the page only as far as it takes to show the box, got " + revealed);
check(prob.current === lines[0] && lines[0].dataset.current === "1", "next from nowhere is the first problem");
check(logs.prob.count.textContent === "1 of " + total, "the position is shown, got " + logs.prob.count.textContent);
check(lines[0].tabIndex === -1 && focused.at(-1).node === lines[0], "the line is focusable and focused");
logs.prob.click1();
check(prob.current === lines[1] && lines[0].dataset.current === undefined, "next moves on and the mark moves with it");
check(logs.prob.count.textContent === "2 of " + total, "the position follows");
logs.prob["click-1"]();
logs.prob["click-1"]();
check(prob.current === lines[total - 1] && logs.prob.count.textContent === total + " of " + total, "previous from the first wraps to the last");
logs.prob.click1();
check(prob.current === lines[0], "next from the last wraps to the first");
say("error: late");
check(logs.prob.count.textContent === "1 of " + (total + 1), "a new problem is counted, got " + logs.prob.count.textContent);
revealed.length = 0;
pfeed.onmessage({ data: "more", lastEventId: String(++pid) });
check(revealed.length === 0, "the page stays where the reader jumped to");

const before2 = prob.bad;
pfeed.onmessage({ data: "5%", lastEventId: String(pid) });
pfeed.onmessage({ data: "error: 5%", lastEventId: String(pid) });
pfeed.onmessage({ data: "ok", lastEventId: String(pid) });
check(prob.bad === before2, "progress turned into a clean line is not counted, got " + prob.bad);
for (let i = 0; i < 5000; i++) say("fine " + i);
check(prob.bad === 0 && logs.prob.problems.hidden, "the problems of dropped lines are not counted, got " + prob.bad);
check(prob.current === null, "the current line is forgotten when it is dropped");
check(logs.prob.cut.textContent.includes("Problems before the cut are not searched"), "a cut log says problems before it are not searched, got " + logs.prob.cut.textContent);
logs.prob.click1();
check(prob.current === null, "next with no problem does nothing");

const stored = {};
async function startWith(wide, kept, broken) {
  boxes[0] = { checked: !wide, addEventListener() {} };
  globalThis.window.matchMedia = (q) => ({ matches: q === "(min-width: 48rem)" && wide });
  Object.keys(stored).forEach((k) => delete stored[k]);
  if (kept !== undefined) stored["kipp.log.times"] = kept;
  globalThis.localStorage = {
    getItem: (k) => {
      if (broken) throw new Error("no storage");
      return stored[k] ?? null;
    },
    setItem: (k, v) => (stored[k] = v),
  };
  shown = [];
  globalThis.document.querySelectorAll = select;
  new Function(fs.readFileSync(script, "utf8"))();
  await settle();
  return boxes[0].checked;
}
check((await startWith(true)) === true, "the times are shown on a wide screen when nothing is remembered");
check((await startWith(false)) === false, "the times are hidden on a narrow screen when nothing is remembered");
check((await startWith(false, "1")) === true, "a narrow screen shows them when the reader chose to");
check((await startWith(true, "0")) === false, "a wide screen hides them when the reader chose to");
check((await startWith(false, undefined, true)) === false, "storage that cannot be read leaves the default of the width");

console.log("build.js: ok");
