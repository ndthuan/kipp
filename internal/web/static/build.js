const TAIL = 1 << 20;
const MAX_LINES = 5000;
const encoder = new TextEncoder();
const bytes = (text) => encoder.encode(text).length;

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

function indexed(n) {
  if (n < 16) return NAMES[n];
  if (n > 255) return null;
  if (n >= 232) return nearest(Array(3).fill(8 + 10 * (n - 232)));
  const level = (v) => (v ? 55 + 40 * v : 0);
  const c = n - 16;
  return nearest([level(Math.floor(c / 36)), level(Math.floor(c / 6) % 6), level(c % 6)]);
}

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

const PROBLEM_COLOURS = ["red", "red-bright", "yellow", "yellow-bright"];
const PROBLEM_WORDS = ["error", "Error", "ERROR", "FAIL", "panic:", "warning", "WARN"];

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

function cut(details, text) {
  const note = details.querySelector("[data-cut]");
  if (!note) return;
  note.textContent = text + " Problems before the cut are not searched.";
  note.hidden = false;
}

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
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) pre.stream = null;
  };
  es.addEventListener("end", () => {
    es.close();
    pre.stream = null;
    pre.dataset.done = "1";
  });
}

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
  pre.scrollTop = line.offsetTop - (pre.clientHeight - line.offsetHeight) / 2;
  details.scrollIntoView({ block: "nearest" });
  line.focus({ preventScroll: true });
  showProblems(details, pre);
}

const pad = (n) => String(n).padStart(2, "0");

function clockOf(ms) {
  const d = new Date(ms);
  return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}

const TIMES = "kipp.log.times";

function timesShown() {
  try {
    const kept = localStorage.getItem(TIMES);
    if (kept === "0" || kept === "1") return kept === "1";
  } catch {
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
    }
  });
});
showTimes(timesShown());

function pause(details) {
  const pre = details.querySelector("pre[data-log]");
  if (pre && pre.stream) {
    pre.stream.close();
    pre.stream = null;
  }
}

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
  document.querySelectorAll("details.log").forEach(showTail);
});
