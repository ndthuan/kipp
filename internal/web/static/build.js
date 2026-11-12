// A log is opened at its end and the page keeps a bounded part of it, in
// bytes and in lines: a build can write more than a browser can hold, in
// many lines or in a few enormous ones. The whole log is the download
// beside it.
const TAIL = 1 << 20; // bytes before its end a log is opened at, and bytes of it the page keeps
const MAX_LINES = 5000; // lines of a log the page keeps
const encoder = new TextEncoder();
const bytes = (text) => encoder.encode(text).length;

// What a build printed to colour its output is drawn, and nothing else of
// the escape sequences is: a log is text, not a terminal. SGR sequences
// (ESC [ ... m) set bold, dim, italic, underline and the 16 colours,
// which are classes the stylesheet paints in the colour roles; a colour of
// the 256 or true-colour range becomes the nearest of the 16. Every other
// sequence is dropped from what is shown, and a broken one loses only its
// ESC. The download is the log as written.
const COLOURS = ["black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"];
const NAMES = [...COLOURS, ...COLOURS.map((c) => c + "-bright")];
const XTERM = [
  [0, 0, 0], [205, 0, 0], [0, 205, 0], [205, 205, 0], [0, 0, 238], [205, 0, 205], [0, 205, 205], [229, 229, 229],
  [127, 127, 127], [255, 0, 0], [0, 255, 0], [255, 255, 0], [92, 92, 255], [255, 0, 255], [0, 255, 255], [255, 255, 255],
];
const SEQUENCE = /\x1b\[([0-?]*)[ -/]*([@-~])|\x1b\](?:[^\x07\x1b]|\x1b(?!\\))*(?:\x07|\x1b\\)|\x1b[ -/]*[\x30-\x5a\x5c\x5e-\x7e]|\x1b/g;

function nearest([r, g, b]) {
  let best = 0;
  let gap = Infinity;
  XTERM.forEach(([x, y, z], i) => {
    const d = (r - x) ** 2 + (g - y) ** 2 + (b - z) ** 2;
    if (d < gap) [best, gap] = [i, d];
  });
  return NAMES[best];
}

// The colour of an index of the 256-colour palette, or null past it.
function indexed(n) {
  if (n < 16) return NAMES[n];
  if (n > 255) return null;
  if (n >= 232) return nearest(Array(3).fill(8 + 10 * (n - 232)));
  const level = (v) => (v ? 55 + 40 * v : 0);
  const c = n - 16;
  return nearest([level(Math.floor(c / 36)), level(Math.floor(c / 6) % 6), level(c % 6)]);
}

// Applies one SGR sequence's parameters to a style.
function sgr(style, params) {
  const codes = params === "" ? [0] : params.split(";").map((p) => (p === "" ? 0 : Number(p)));
  for (let i = 0; i < codes.length; i++) {
    const n = codes[i];
    if (n === 0) Object.keys(style).forEach((k) => delete style[k]);
    else if (n === 1) style.b = true;
    else if (n === 2) style.dim = true;
    else if (n === 3) style.i = true;
    else if (n === 4) style.u = true;
    else if (n === 22) ["b", "dim"].forEach((k) => delete style[k]);
    else if (n === 23) delete style.i;
    else if (n === 24) delete style.u;
    else if (n >= 30 && n <= 37) style.fg = NAMES[n - 30];
    else if (n >= 90 && n <= 97) style.fg = NAMES[n - 90 + 8];
    else if (n >= 40 && n <= 47) style.bg = NAMES[n - 40];
    else if (n >= 100 && n <= 107) style.bg = NAMES[n - 100 + 8];
    else if (n === 39) delete style.fg;
    else if (n === 49) delete style.bg;
    else if (n === 38 || n === 48) {
      const mode = codes[i + 1];
      const colour = mode === 5 ? indexed(codes[i + 2]) : mode === 2 ? nearest(codes.slice(i + 2, i + 5)) : null;
      i += mode === 5 ? 2 : mode === 2 ? 4 : 0;
      const key = n === 38 ? "fg" : "bg";
      if (colour) style[key] = colour;
      else delete style[key];
    }
  }
}

const classesOf = (s) =>
  [s.b && "ansi-b", s.dim && "ansi-dim", s.i && "ansi-i", s.u && "ansi-u", s.fg && "ansi-fg-" + s.fg, s.bg && "ansi-bg-" + s.bg]
    .filter(Boolean)
    .join(" ");

// A line is a problem when it was drawn in red or yellow (SGR 31, 33, 91,
// 93), or when it says one of these words, written exactly so.
const PROBLEM_COLOURS = ["red", "red-bright", "yellow", "yellow-bright"];
const PROBLEM_WORDS = ["error", "Error", "ERROR", "FAIL", "panic:", "warning", "WARN"];

// Puts a line in a span: one text node when nothing in it is styled, a
// span per styled run otherwise. The newline is plain text after them, so
// no style reaches past the line.
function fill(node, text) {
  node.raw = bytes(text) + 1;
  node.textContent = "";
  node.problem = false;
  if (!text.includes("\x1b")) {
    node.textContent = text + "\n";
    node.problem = PROBLEM_WORDS.some((w) => text.includes(w));
    return;
  }
  const style = {};
  let plain = "";
  const put = (run) => {
    if (!run) return;
    plain += run;
    if (PROBLEM_COLOURS.includes(style.fg)) node.problem = true;
    const classes = classesOf(style);
    if (!classes) return node.append(document.createTextNode(run));
    const span = document.createElement("span");
    span.className = classes;
    span.textContent = run;
    node.append(span);
  };
  let at = 0;
  for (const m of text.matchAll(SEQUENCE)) {
    put(text.slice(at, m.index));
    at = m.index + m[0].length;
    if (m[2] === "m") sgr(style, m[1]);
  }
  put(text.slice(at));
  node.append(document.createTextNode("\n"));
  node.problem = node.problem || PROBLEM_WORDS.some((w) => plain.includes(w));
}

// Shows the line that says the log is not all there.
function cut(details, text) {
  const note = details.querySelector("[data-cut]");
  if (!note) return;
  note.textContent = text + " Problems before the cut are not searched.";
  note.hidden = false;
}

// Follows a log while its <details> is open: from where it stopped, or,
// the first time, from TAIL bytes before its end, once the server has said
// how long it is.
function follow(details) {
  const pre = details.querySelector("pre[data-log]");
  if (!pre || pre.stream || pre.dataset.done) return;
  if (pre.dataset.offset !== undefined) {
    stream(details, pre, false);
    return;
  }
  const asking = {
    close() {
      this.closed = true;
    },
  };
  pre.stream = asking;
  const size = (res) => Number(res.headers.get("X-Kipp-Size")) || 0;
  fetch(pre.dataset.log, { method: "HEAD" })
    .then(size, () => 0)
    .then((length) => {
      if (asking.closed) return;
      const start = Math.max(0, length - TAIL);
      pre.dataset.offset = String(start);
      if (start > 0) cut(details, "Showing the last 1 MiB of this log.");
      stream(details, pre, start > 0);
    });
}

// The notice at the top of a red build's page has room for the last lines
// of the log it points at, taken from the log the page already holds: the
// last TAIL_LINES that are not empty, counted back from the end.
const TAIL_LINES = 5;

function showTail(details) {
  const name = details.id.replace(/^log-/, "");
  const box = document.querySelector('[data-tail-of="' + name + '"]');
  const pre = details.querySelector("pre[data-log]");
  if (!box || !pre) return;
  const last = [];
  const nodes = pre.childNodes;
  for (let i = nodes.length - 1; i >= 0 && last.length < TAIL_LINES; i--) {
    const text = nodes[i].textContent.replace(/\n$/, "");
    if (text.trim() !== "") last.unshift(text);
  }
  if (last.length === 0) return;
  box.textContent = last.join("\n");
  box.hidden = false;
}

// Streams a log over SSE. A line is a span whose text is set as text,
// never as markup: a log holds whatever a build printed. An event with a
// new id ends a line; one without is a piece of a line that a carriage
// return ended, progress that the next piece overwrites as it would on a
// terminal. A "time" event before a line says when it began (RFC 3339), which the
// span carries for the stylesheet to show. midLine says the stream starts
// inside a line, which is dropped up to its end.
function stream(details, pre, midLine) {
  const es = new EventSource(pre.dataset.log + "/events?offset=" + pre.dataset.offset + "&timestamps=true");
  pre.stream = es;
  let seen = "";
  let began = null;
  es.addEventListener("time", (e) => (began = Date.parse(e.data)));
  es.onmessage = (e) => {
    const ends = e.lastEventId !== "" && e.lastEventId !== seen;
    if (ends) {
      seen = e.lastEventId;
      pre.dataset.offset = seen;
    }
    if (midLine) {
      midLine = !ends;
      if (ends) began = null;
      return;
    }
    const atEnd = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    let node = pre.progress;
    if (node) {
      pre.held -= node.raw;
      if (node.problem) pre.bad--;
    } else {
      node = document.createElement("span");
      pre.append(node);
    }
    fill(node, e.data);
    pre.held = (pre.held || 0) + node.raw;
    if (node.problem) pre.bad = (pre.bad || 0) + 1;
    if (began !== null) node.dataset.t = clockOf(began);
    if (ends) began = null;
    pre.progress = ends ? null : node;
    trim(details, pre);
    showProblems(details, pre);
    showTail(details);
    if (atEnd) pre.scrollTop = pre.scrollHeight;
    reveal(details);
  };
  // A browser reconnects by itself (readyState CONNECTING) until the
  // server refuses it; then the stream is dead, and is forgotten so that
  // closing the log and opening it again starts a new one where it stopped.
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) pre.stream = null;
  };
  es.addEventListener("end", () => {
    es.close();
    pre.stream = null;
    pre.dataset.done = "1";
  });
}

// Drops lines from the top of a log until what is left is within both
// bounds, and says so. The last line stays however long it is: the server
// sends none longer than two of its chunks.
function trim(details, pre) {
  let said = "";
  while (pre.childNodes.length > MAX_LINES) {
    drop(pre);
    said = "Showing the last " + MAX_LINES + " lines of this log.";
  }
  while (pre.held > TAIL && pre.childNodes.length > 1) {
    drop(pre);
    said = "Showing the last 1 MiB of this log.";
  }
  if (said) cut(details, said);
}

function drop(pre) {
  const gone = pre.firstChild;
  pre.held -= gone.raw;
  if (gone.problem) pre.bad--;
  if (pre.current === gone) pre.current = null;
  pre.firstChild.remove();
}

// The buttons that walk the problem lines of a log, and the count between
// them: "3 of 7" once one is chosen, "7 problems" before. They are drawn
// when the log holds a problem.
const problemsOf = (pre) => Array.from(pre.childNodes).filter((n) => n.problem);

function showProblems(details, pre) {
  const box = details.querySelector("[data-problems]");
  const count = details.querySelector("[data-count]");
  if (!box || !count) return;
  const n = pre.bad || 0;
  box.hidden = n === 0;
  const at = pre.current ? problemsOf(pre).indexOf(pre.current) : -1;
  count.textContent = at < 0 ? n + (n === 1 ? " problem" : " problems") : at + 1 + " of " + n;
}

// Moves to the next (1) or previous (-1) problem line, round the ends, and
// puts the keyboard there.
function goProblem(details, pre, step) {
  const all = problemsOf(pre);
  if (all.length === 0) return;
  const from = pre.current ? all.indexOf(pre.current) : -1;
  const to = from < 0 ? (step > 0 ? 0 : all.length - 1) : (from + step + all.length) % all.length;
  if (pre.current) delete pre.current.dataset.current;
  const line = all[to];
  pre.current = line;
  line.dataset.current = "1";
  line.tabIndex = -1;
  following = false;
  // The box scrolls to the line, the page only far enough to show the box.
  pre.scrollTop = line.offsetTop - (pre.clientHeight - line.offsetHeight) / 2;
  details.scrollIntoView({ block: "nearest" });
  line.focus({ preventScroll: true });
  showProblems(details, pre);
}

const pad = (n) => String(n).padStart(2, "0");

// The time of day a line began, in the reader's own zone.
function clockOf(ms) {
  const d = new Date(ms);
  return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}

// The times beside the lines are shown or hidden on every log of the page
// at once, and the choice is remembered in this browser. Storage that cannot be used only forgets the choice.
const TIMES = "kipp.log.times";

// A narrow screen has no room for them: with nothing remembered they are
// shown from 48rem up.
function timesShown() {
  try {
    const kept = localStorage.getItem(TIMES);
    if (kept === "0" || kept === "1") return kept === "1";
  } catch {
    // nothing remembered
  }
  return !window.matchMedia || window.matchMedia("(min-width: 48rem)").matches;
}

function showTimes(on) {
  document.querySelectorAll("pre[data-log]").forEach((pre) => pre.classList.toggle("times", on));
  document.querySelectorAll("input[data-times]").forEach((box) => (box.checked = on));
}

document.querySelectorAll("input[data-times]").forEach((box) => {
  box.addEventListener("change", () => {
    showTimes(box.checked);
    try {
      localStorage.setItem(TIMES, box.checked ? "1" : "0");
    } catch {
      // the choice holds for this page
    }
  });
});
showTimes(timesShown());

// A closed log gives its connection back; opened again it goes on from
// where it was.
function pause(details) {
  const pre = details.querySelector("pre[data-log]");
  if (pre && pre.stream) {
    pre.stream.close();
    pre.stream = null;
  }
}

// The stage chips beside the names of the logs say how each stage stands;
// the logs are not part of the refreshed summary, so the summary carries
// the states as "name=status" and the chips follow it.
function syncChips(live) {
  (live.dataset.states || "").split(" ").filter(Boolean).forEach((pair) => {
    const at = pair.indexOf("=");
    const details = document.getElementById("log-" + pair.slice(0, at));
    const chip = details && details.querySelector("[data-chip]");
    if (!chip) return;
    chip.className = "status " + pair.slice(at + 1);
    chip.textContent = pair.slice(at + 1);
  });
}

// The page is taken to the log a reader came for, the last one open, and
// kept showing it as it grows, until the reader moves the page themselves.
// The log scrolls in a box of its own, so the page never has to follow it
// to its end.
let focus = null;
let following = true;
let revealing = false;
for (const name of ["wheel", "touchstart", "keydown"]) {
  window.addEventListener(name, () => (following = false), { once: true, passive: true });
}

function reveal(details) {
  if (!following || details !== focus || revealing) return;
  revealing = true;
  requestAnimationFrame(() => {
    revealing = false;
    if (following) details.scrollIntoView({ block: "nearest" });
  });
}

const opened = new Set();
document.querySelectorAll("details.log").forEach((details) => {
  details.addEventListener("toggle", () => (details.open ? follow(details) : pause(details)));
  details.querySelectorAll("[data-problem]").forEach((button) => {
    button.addEventListener("click", () => goProblem(details, details.querySelector("pre[data-log]"), Number(button.dataset.problem)));
  });
  if (details.open) {
    opened.add(details.id);
    focus = details;
    follow(details);
  }
});

// After each refresh of the build's summary: a log the page has no place
// for yet (the stages of a build that had not started, the hook's) is
// fetched by loading the page again; a log the server now says is worth
// reading (a stage that started, failed or ended unstable) is opened,
// once, so one the reader closed stays closed, and becomes the one the
// page follows; and an open log whose stream died is followed again.
document.body.addEventListener("htmx:afterSwap", () => {
  const live = document.getElementById("live");
  if (!live) return;
  const logs = (live.dataset.logs || "").split(" ").filter(Boolean);
  if (logs.some((name) => !document.getElementById("log-" + name))) {
    location.reload();
    return;
  }
  (live.dataset.open || "").split(" ").filter(Boolean).forEach((name) => {
    const details = document.getElementById("log-" + name);
    if (details && !opened.has(details.id)) {
      opened.add(details.id);
      details.open = true;
      focus = details;
    }
  });
  syncChips(live);
  document.querySelectorAll("details.log[open]").forEach(follow);
  // The refresh drew the notice again, empty.
  document.querySelectorAll("details.log").forEach(showTail);
});
