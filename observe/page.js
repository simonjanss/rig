"use strict";

/* rig monitor.
 *
 * Vanilla, no build step, and no innerHTML anywhere — every node is built, so
 * nothing on this page can be escaped wrongly. The page is a reader over two
 * files; all the filtering happens on the server, and everything here is
 * layout, the numbers over the window it was handed, and the state in the URL.
 */

/* ── nodes ──────────────────────────────────────────────────────────────── */

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

const SVG = "http://www.w3.org/2000/svg";
const svg = (tag, attrs) => {
  const n = document.createElementNS(SVG, tag);
  for (const k in attrs) n.setAttribute(k, attrs[k]);
  return n;
};

const $ = (id) => document.getElementById(id);

/* ── numbers and time ───────────────────────────────────────────────────── */

// ms keeps three significant figures across five orders of magnitude, because
// the spans on one page run from a hundred microseconds of SQL to a request
// that took two seconds, and an integer would round most of the first kind to
// zero.
function ms(n) {
  if (!isFinite(n)) return "–";
  if (n >= 10000) return (n / 1000).toFixed(1) + "s";
  if (n >= 1000) return (n / 1000).toFixed(2) + "s";
  if (n >= 100) return n.toFixed(0) + "ms";
  if (n >= 1) return n.toFixed(1) + "ms";
  return n.toFixed(2) + "ms";
}

function ago(date) {
  const s = (Date.now() - date.getTime()) / 1000;
  if (s < 2) return "now";
  if (s < 90) return Math.round(s) + "s";
  if (s < 5400) return Math.round(s / 60) + "m";
  return Math.round(s / 3600) + "h";
}

const stamp = (date) => date.toLocaleString(undefined, {hour12: false});

// span is a duration in words, for the caption that keeps the summary honest
// about what it covers.
function span(msTotal) {
  const s = Math.round(msTotal / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.round(s / 60) + "m";
  const h = Math.floor(s / 3600);
  return h + "h " + Math.round((s - h * 3600) / 60) + "m";
}

// quantile by nearest rank. Over a few hundred requests, interpolating between
// two of them would be a decimal of false precision.
function quantile(sorted, p) {
  if (!sorted.length) return NaN;
  const i = Math.min(sorted.length - 1, Math.ceil(p * sorted.length) - 1);
  return sorted[Math.max(0, i)];
}

/* ── state, and the URL it lives in ─────────────────────────────────────── */

// In the hash so that a view is a link: reload, share, and the back button all
// work, and none of it reaches the server, which has no business knowing which
// request somebody had open.
const state = {tab: "requests", q: "", status: "", level: "", trace: ""};

let readingHash = false;

function readHash() {
  const p = new URLSearchParams(location.hash.replace(/^#/, ""));
  state.tab = p.get("tab") === "logs" ? "logs" : "requests";
  state.q = p.get("q") || "";
  state.status = p.get("status") === "error" ? "error" : "";
  state.level = (p.get("level") || "").toUpperCase();
  state.trace = p.get("trace") || "";
}

function writeHash() {
  const p = new URLSearchParams();
  if (state.tab !== "requests") p.set("tab", state.tab);
  if (state.q) p.set("q", state.q);
  if (state.status) p.set("status", state.status);
  if (state.level) p.set("level", state.level);
  if (state.trace) p.set("trace", state.trace);

  const next = "#" + p.toString();
  if (next === location.hash || (next === "#" && !location.hash)) return;
  readingHash = true;
  location.hash = next;
  // The flag is cleared by the hashchange this caused, or here if none fires.
  setTimeout(() => { readingHash = false; }, 0);
}

/* ── the shell ──────────────────────────────────────────────────────────── */

const tracesBody = $("traces-body");
const logsBody = $("logs-body");
const detail = $("detail");

let lastTraces = [];
let lastLoad = null;
// One trace's log lines, keyed by trace id. Safe to cache: a finished trace and
// the lines it wrote are both already on disk and cannot change.
const traceLogs = new Map();
// Why a trace has no lines, when it has none, which is not the same as not
// having asked yet.
let traceLogsReason = "";

function applyTab() {
  document.body.dataset.tab = state.tab;
  for (const [name, panel] of [["requests", "panel-requests"], ["logs", "panel-logs"]]) {
    const on = state.tab === name;
    $("tab-" + name).setAttribute("aria-selected", String(on));
    $(panel).hidden = !on;
  }
}

function applyFilters() {
  $("q").value = state.q;
  for (const b of $("status-seg").children) {
    b.setAttribute("aria-pressed", String(b.dataset.status === state.status));
  }
  for (const b of $("level-seg").children) {
    b.setAttribute("aria-pressed", String(b.dataset.level === state.level));
  }
}

/* ── loading ────────────────────────────────────────────────────────────── */

let inFlight = false;

async function get(path, params) {
  const qs = new URLSearchParams();
  for (const k in params) if (params[k]) qs.set(k, params[k]);
  const res = await fetch(path + "?" + qs.toString(), {headers: {Accept: "application/json"}});
  if (!res.ok) throw new Error(res.status + " " + res.statusText);
  return res.json();
}

async function load() {
  if (inFlight) return;
  inFlight = true;
  try {
    if (state.tab === "requests") {
      await loadTraces();
    } else {
      await loadLogs();
    }
    lastLoad = new Date();
    $("pulse").className = "pulse on";
  } catch (err) {
    $("pulse").className = "pulse bad";
    const note = state.tab === "requests" ? $("note-requests") : $("note-logs");
    note.textContent = "Could not load: " + err.message;
  } finally {
    inFlight = false;
    tickUpdated();
  }
}

async function loadTraces() {
  const data = await get("traces.json", {q: state.q, status: state.status});
  service(data.service);
  $("note-requests").textContent = data.reason || "";
  lastTraces = data.traces || [];
  renderTiles(lastTraces);
  renderTraces(lastTraces);
  renderDetail();
}

async function loadLogs() {
  const data = await get("logs.json", {q: state.q, level: state.level});
  service(data.service);
  $("note-logs").textContent = data.reason || "";
  renderLevels(data.levels || {});
  renderLogs(data.logs || []);
}

function service(name) {
  if (!name) return;
  $("service").textContent = name;
  document.title = "rig monitor · " + name;
}

function tickUpdated() {
  $("updated").textContent = lastLoad ? "updated " + ago(lastLoad) : "";
}

/* ── rows ───────────────────────────────────────────────────────────────── */

// syncRows replaces the body with one node per item, reusing the node an item
// already had.
//
// Reuse and not a rebuild, because a rebuild every five seconds is what makes a
// live page unusable: it drops the row you had open, the row you had focused,
// and the scroll position you had found them from. Both files are append-only
// and hold finished records, so a row that is still here is a row whose
// contents cannot have changed — refresh only touches what is relative to the
// window, which is the age and the bar.
function syncRows(tbody, items, keyOf, build, refresh) {
  const existing = new Map();
  for (const tr of tbody.children) {
    if (tr.dataset.key) existing.set(tr.dataset.key, tr);
  }

  const focused = document.activeElement && document.activeElement.closest
    ? document.activeElement.closest("tr")
    : null;
  const focusKey = focused ? focused.dataset.key : null;

  const next = [];
  items.forEach((item, i) => {
    const key = keyOf(item);
    const old = existing.get(key);
    if (old) {
      refresh(old, item, i);
      next.push(old);
      return;
    }
    next.push(build(item, key, i));
  });

  tbody.replaceChildren(...next);

  if (focusKey) {
    const again = tbody.querySelector('tr[data-key="' + CSS.escape(focusKey) + '"]');
    if (again) again.focus({preventScroll: true});
  }
}

// row is the shell every clickable row shares: a real table row that answers to
// the keyboard, since a div that listens for clicks is a div.
function row(key, onOpen) {
  const tr = el("tr");
  tr.dataset.key = key;
  tr.tabIndex = 0;
  tr.addEventListener("click", (ev) => {
    // Not when somebody was reaching for the copy button inside it.
    if (ev.target.closest("button")) return;
    onOpen();
  });
  tr.addEventListener("keydown", (ev) => {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    ev.preventDefault();
    onOpen();
  });
  return tr;
}

function whenCell(date) {
  const td = el("td", "c-when");
  td.append(el("span", null, ago(date)));
  td.title = stamp(date);
  return td;
}

function idCell(id) {
  const td = el("td", "c-id");
  if (!id) return td;

  const short = el("span", "id", id.slice(0, 8));
  short.title = id;
  td.append(short, copyButton(id));
  return td;
}

// copyButton is here because a trace id is for pasting somewhere else — into a
// search box, a ticket, a message to whoever complained.
function copyButton(text) {
  const b = el("button", "copy", "⧉");
  b.type = "button";
  b.title = "Copy " + text;
  b.setAttribute("aria-label", "Copy " + text);
  b.addEventListener("click", async (ev) => {
    ev.stopPropagation();
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = "✓";
      setTimeout(() => { b.textContent = "⧉"; }, 900);
    } catch {
      // No clipboard permission, over plain HTTP or in a locked-down browser.
      // The full id is in the title either way, so this is a nicety failing and
      // not the page failing.
      b.textContent = "✗";
      setTimeout(() => { b.textContent = "⧉"; }, 900);
    }
  });
  return b;
}

/* ── the requests table ─────────────────────────────────────────────────── */

// statusOf reads the answer off the root span's attributes. A trace whose root
// was rotated away has no status code, and says so rather than claiming one.
function statusOf(t) {
  const attrs = (t.root && t.root.attributes) || {};
  const code = Number(attrs["http.response.status_code"]);
  return isFinite(code) && code > 0 ? code : 0;
}

function methodOf(t) {
  const attrs = (t.root && t.root.attributes) || {};
  return attrs["http.request.method"] || "";
}

// routeLabel is the route without the verb in front of it, because the verb has
// a column of its own. A request span is named "GET /api/v1/teams" — which is
// what groups in a collector — and printing it whole beside the chip says GET
// twice.
function routeLabel(t) {
  const name = t.name || "(root missing)";
  const method = methodOf(t);
  return method && name.startsWith(method + " ") ? name.slice(method.length + 1) : name;
}

function statusBadge(code, failed) {
  const cls = code >= 500 || (failed && !code) ? "err"
    : code >= 400 ? "warn"
    : code >= 200 ? "ok"
    : "mute";
  const word = code >= 500 ? "server"
    : code >= 400 ? "client"
    : code >= 300 ? "moved"
    : code >= 200 ? "ok"
    : failed ? "error" : "–";
  const mark = cls === "err" ? "✕ " : cls === "warn" ? "! " : cls === "ok" ? "✓ " : "";
  return el("span", "badge " + cls, mark + (code ? code + " " : "") + word);
}

function methodClass(m) {
  if (m === "DELETE") return "method d";
  if (m === "POST" || m === "PUT" || m === "PATCH") return "method w";
  return "method";
}

function renderTraces(traces) {
  const worst = traces.reduce((m, t) => Math.max(m, t.duration_ms || 0), 0);

  syncRows(tracesBody, traces, (t) => t.id,
    (t) => {
      const tr = row(t.id, () => select(t.id === state.trace ? "" : t.id));
      const failed = t.status === "error";

      tr.append(whenCell(new Date(t.start)));
      tr.append(cell("c-method", el("span", methodClass(methodOf(t)), methodOf(t))));

      const route = el("td", "c-route");
      const name = el("span", "route", routeLabel(t));
      name.title = t.name || "";
      route.append(name);
      tr.append(route);

      tr.append(cell("c-status", statusBadge(statusOf(t), failed)));
      tr.append(durationCell(t.duration_ms || 0, worst, failed));
      tr.append(idCell(t.id));
      return tr;
    },
    (tr, t) => {
      // The age, and the bar the newest request may have just rescaled.
      tr.children[0].firstChild.textContent = ago(new Date(t.start));
      const bar = tr.querySelector(".dur .b i");
      if (bar) bar.style.width = barWidth(t.duration_ms || 0, worst);
    });

  markSelection(tracesBody);
}

function cell(cls, node) {
  const td = el("td", cls);
  td.append(node);
  return td;
}

const barWidth = (v, worst) => (worst > 0 ? Math.max(2, (v / worst) * 100) : 0) + "%";

function durationCell(v, worst, failed) {
  const td = el("td", "c-ms num");
  const wrap = el("div", "dur");
  wrap.append(el("span", "n", ms(v)));

  const track = el("span", "b");
  const fill = el("i", failed ? "err" : null);
  fill.style.width = barWidth(v, worst);
  track.append(fill);
  wrap.append(track);
  td.append(wrap);
  return td;
}

function markSelection(tbody) {
  for (const tr of tbody.children) {
    const on = tr.dataset.key === state.trace;
    tr.classList.toggle("sel", on);
    if (on) {
      tr.setAttribute("aria-current", "true");
    } else {
      tr.removeAttribute("aria-current");
    }
  }
}

/* ── the summary ────────────────────────────────────────────────────────── */

// The numbers describe the requests on this page and nothing else. That is why
// every tile says so: this is a reader over the last few hundred requests, not
// a metrics store, and a percentile with no window on it would be read as one.
function renderTiles(traces) {
  const tiles = $("tiles");
  if (!traces.length) {
    tiles.replaceChildren();
    return;
  }

  const errors = traces.filter((t) => t.status === "error").length;
  const durations = traces.map((t) => t.duration_ms || 0).sort((a, b) => a - b);
  const starts = traces.map((t) => new Date(t.start).getTime());
  const covered = Math.max(...starts) - Math.min(...starts);

  const out = [];
  out.push(tile("Requests", big(String(traces.length)), "over " + span(covered)));

  const rate = (errors / traces.length) * 100;
  out.push(tile("Errors", big(String(errors), errors > 0),
    errors ? rate.toFixed(rate < 10 ? 1 : 0) + "% of them" : "none here"));

  const lat = el("div", "pct");
  for (const [k, p] of [["p50", .5], ["p95", .95], ["p99", .99]]) {
    const box = el("div");
    box.append(el("span", "k", k), el("span", "v", ms(quantile(durations, p))));
    lat.append(box);
  }
  out.push(tile("Latency", lat, "nearest rank"));

  const chart = el("div");
  const plot = svg("svg", {class: "spark", role: "img",
    "aria-label": "Requests over the window, errors marked"});
  chart.append(plot);
  const legend = el("div", "legend");
  for (const [cls, word] of [["", "requests"], ["err", "errors"]]) {
    const item = el("span");
    item.append(el("i", cls), el("span", null, word));
    legend.append(item);
  }
  chart.append(legend);
  out.push(tile("Over time", chart, null, true));

  tiles.replaceChildren(...out);
  // Drawn after it is in the document, because the bar geometry is measured
  // rather than scaled: a viewBox stretched to the tile would stretch the gaps
  // between bars with it, and those gaps are what keeps them countable.
  drawSpark(plot, traces);
}

function tile(heading, body, sub, wide) {
  const t = el("div", wide ? "tile wide" : "tile");
  t.append(el("h2", null, heading), body);
  if (sub) t.append(el("div", "sub", sub));
  return t;
}

function big(text, bad) {
  return el("div", bad ? "big bad" : "big", text);
}

function drawSpark(plot, traces) {
  const width = Math.max(80, Math.round(plot.getBoundingClientRect().width));
  const height = 34;
  plot.setAttribute("viewBox", "0 0 " + width + " " + height);

  const starts = traces.map((t) => new Date(t.start).getTime());
  const t0 = Math.min(...starts);
  const t1 = Math.max(...starts);
  const range = Math.max(1, t1 - t0);

  const slot = 6;
  const n = Math.max(8, Math.min(60, Math.floor(width / slot)));
  const buckets = Array.from({length: n}, () => ({all: 0, err: 0}));
  traces.forEach((t) => {
    const i = Math.min(n - 1, Math.floor(((new Date(t.start).getTime() - t0) / range) * n));
    buckets[i].all++;
    if (t.status === "error") buckets[i].err++;
  });

  const peak = buckets.reduce((m, b) => Math.max(m, b.all), 0) || 1;
  const w = width / n;
  const bar = Math.max(1, w - 2); // a 2px gap, so two full buckets stay two
  const usable = height - 1;

  const parts = [svg("rect", {x: 0, y: height - 1, width: width, height: 1,
    fill: "var(--grid)"})];
  buckets.forEach((b, i) => {
    const x = i * w + (w - bar) / 2;
    const when = new Date(t0 + (range * (i + 0.5)) / n);

    // A hit target over the whole column rather than over the bar, so a bucket
    // with one request in it is still something a pointer can find.
    const hit = svg("rect", {x: i * w, y: 0, width: w, height: height, fill: "transparent"});
    const label = document.createElementNS(SVG, "title");
    label.textContent = when.toLocaleTimeString(undefined, {hour12: false}) + " · " +
      b.all + (b.all === 1 ? " request" : " requests") +
      (b.err ? ", " + b.err + " failed" : "");
    hit.append(label);
    parts.push(hit);

    if (!b.all) return;

    const total = Math.max(2, (b.all / peak) * usable);
    const errH = b.err ? Math.max(2, (b.err / peak) * usable) : 0;
    // Errors sit on the baseline and the rest above them, with a gap between:
    // stacked, because the two add up to the bucket, and gapped for the reason
    // the bars are.
    if (errH) {
      parts.push(svg("rect", {x: x, y: height - errH, width: bar, height: errH,
        rx: 1, fill: "var(--critical)"}));
    }
    const okH = Math.max(0, total - errH - (errH ? 2 : 0));
    if (okH > 0) {
      parts.push(svg("rect", {x: x, y: height - errH - (errH ? 2 : 0) - okH,
        width: bar, height: okH, rx: 1, fill: "var(--accent)"}));
    }
  });
  plot.replaceChildren(...parts);
}

/* ── the detail pane ────────────────────────────────────────────────────── */

function select(id) {
  state.trace = id;
  writeHash();
  markSelection(tracesBody);
  renderDetail();
}

function renderDetail() {
  const open = Boolean(state.trace) && state.tab === "requests";
  document.body.classList.toggle("open", open);
  detail.hidden = !open;
  if (!open) return;

  const t = lastTraces.find((x) => x.id === state.trace);
  if (!t) {
    detail.replaceChildren(detailHead(null), el("p", "note",
      "That request is not in the window on this page — the filter excludes it, or the file has rotated past it."));
    return;
  }

  const parts = [detailHead(t)];

  const spans = t.spans || [];
  const depth = depths(spans);
  const t0 = new Date(t.start).getTime();
  const total = t.duration_ms || 1;

  parts.push(el("h3", null, "Timeline"));
  parts.push(axis(total, traceLogs.get(t.id), t0));

  const wf = el("div", "wf");
  const selfTimes = selfTime(spans);
  spans.forEach((s) => wf.append(spanRow(s, depth.get(s.span_id) || 0, t0, total, selfTimes)));
  parts.push(wf);

  parts.push(...detailLogs(t));
  detail.replaceChildren(...parts);

  if (!traceLogs.has(t.id)) fetchTraceLogs(t.id);
}

function detailHead(t) {
  const head = el("div", "dh");
  if (t) {
    head.append(statusBadge(statusOf(t), t.status === "error"));
    head.append(el("span", "route", t.name || "(root missing)"));
  } else {
    head.append(el("span", "route", "Request"));
  }

  const close = el("button", "ghost close", "Close");
  close.type = "button";
  close.addEventListener("click", () => select(""));
  head.append(close);

  const wrap = document.createDocumentFragment();
  wrap.append(head);

  const sub = el("div", "dh-sub");
  if (t) {
    sub.append(el("span", null, ms(t.duration_ms || 0)));
    sub.append(el("span", null, stamp(new Date(t.start))));
    const id = el("span", "id", t.id);
    sub.append(id, copyButton(t.id));
  }
  wrap.append(sub);
  return wrap;
}

// depthOf walks parent_id up to the root. The file is one line per finished
// span with no nesting of its own, so the tree is rebuilt here.
function depths(spans) {
  const byID = new Map(spans.map((s) => [s.span_id, s]));
  const out = new Map();
  const walk = (s, seen) => {
    if (out.has(s.span_id)) return out.get(s.span_id);
    const parent = byID.get(s.parent_id);
    // seen guards against a cycle, which cannot happen but would hang the page.
    const d = (!parent || seen.has(s.span_id)) ? 0 : walk(parent, seen.add(s.span_id)) + 1;
    out.set(s.span_id, d);
    return d;
  };
  spans.forEach((s) => walk(s, new Set()));
  return out;
}

// selfTime is a span's duration less what its children accounted for, which is
// the number that answers "was it this stage, or something it called".
function selfTime(spans) {
  const own = new Map(spans.map((s) => [s.span_id, s.duration_ms || 0]));
  spans.forEach((s) => {
    if (!own.has(s.parent_id)) return;
    own.set(s.parent_id, own.get(s.parent_id) - (s.duration_ms || 0));
  });
  return own;
}

function axis(total, logs, t0) {
  const a = el("div", "axis");
  const stops = [0, .25, .5, .75, 1];
  stops.forEach((f, i) => {
    const last = i === stops.length - 1;
    const t = el("span", "t" + (i === 0 ? " first" : last ? " last" : ""), ms(total * f));
    if (last) {
      t.style.right = "0";
    } else {
      t.style.left = (f * 100) + "%";
    }
    a.append(t);
  });

  // Every warn-or-worse line the trace wrote, on the same time base as the bars
  // below it.
  (logs || []).forEach((rec) => {
    const level = (rec.level || "").toUpperCase();
    if (level !== "WARN" && level !== "ERROR") return;
    const at = ((new Date(rec.time).getTime() - t0) / total) * 100;
    if (!isFinite(at)) return;
    const mk = el("span", level === "ERROR" ? "mk err" : "mk");
    mk.style.left = Math.max(0, Math.min(99.5, at)) + "%";
    mk.title = level + " · " + rec.msg;
    a.append(mk);
  });
  return a;
}

function spanRow(s, depth, t0, total, selfTimes) {
  const wrap = el("div", "wf-row");

  const head = el("div", "wf-head");
  const name = el("span", "wf-name", s.name);
  name.style.paddingLeft = (depth * 12) + "px";
  name.title = s.name;
  head.append(name);
  if (s.status === "error") head.append(el("span", "badge err", "✕ error"));

  const own = selfTimes.get(s.span_id);
  const timing = el("span", "wf-ms");
  timing.append(el("span", null, ms(s.duration_ms || 0)));
  // Only when the difference is worth a reader's attention. On a leaf span the
  // two are the same number twice.
  if (own !== undefined && s.duration_ms - own > 0.5) {
    timing.append(el("span", "wf-self", " · self " + ms(Math.max(0, own))));
  }
  head.append(timing);
  wrap.append(head);

  const track = el("div", "track");
  const fill = el("div", "fill" + (s.status === "error" ? " err" : ""));
  const offset = total > 0 ? Math.max(0, ((new Date(s.start).getTime() - t0) / total) * 100) : 0;
  fill.style.left = Math.min(offset, 99) + "%";
  fill.style.width = Math.max(total > 0 ? (s.duration_ms / total) * 100 : 0, 0.4) + "%";
  fill.title = "at " + ms(total * offset / 100) + " for " + ms(s.duration_ms || 0);
  track.append(fill);
  wrap.append(track);

  if (s.error) wrap.append(el("div", "err-text", s.error));

  const attrs = s.attributes || {};
  // The statement, on its own, because it is the one attribute anybody reads in
  // full — and it does not fit on a line with the others.
  const sql = attrs["db.query.text"];
  const rest = Object.keys(attrs).filter((k) => k !== "db.query.text").sort();
  if (rest.length) {
    wrap.append(el("div", "attrs", rest.map((k) => k + "=" + attrs[k]).join("  ")));
  }
  if (sql) wrap.append(el("pre", "sql", String(sql)));
  return wrap;
}

function detailLogs(t) {
  const logs = traceLogs.get(t.id);
  const out = [el("h3", null, logs ? "Logs (" + logs.length + ")" : "Logs")];

  if (!logs) {
    out.push(el("p", "attrs", "reading…"));
    return out;
  }
  if (!logs.length) {
    out.push(el("p", "attrs", traceLogsReason || "No line here belongs to this request."));
    return out;
  }

  const t0 = new Date(t.start).getTime();
  const list = el("div", "dlogs");
  logs.forEach((rec) => {
    const line = el("div", "dlog");
    const at = (new Date(rec.time).getTime() - t0);
    line.append(el("span", "at", isFinite(at) ? "+" + ms(at) : ""));
    line.append(levelBadge(rec.level));
    line.append(el("span", "m", rec.msg));

    // Not the request group: every key in it — the route, the trace, the user
    // agent — is already at the top of the pane this line is inside, and
    // repeating it per line is what turns a readable few lines into a wall.
    const flat = flatten(rec.attrs).filter(([k]) => !k.startsWith("request."));
    if (flat.length) {
      line.append(el("div", "attrs a", flat.map(([k, v]) => k + "=" + v).join("  ")));
    }
    list.append(line);
  });
  out.push(list);
  return out;
}

async function fetchTraceLogs(id) {
  try {
    const data = await get("logs.json", {trace: id});
    traceLogs.set(id, data.logs || []);
    traceLogsReason = data.reason || "";
  } catch {
    traceLogs.set(id, []);
    traceLogsReason = "Could not read the log file.";
  }
  if (state.trace === id) renderDetail();
}

/* ── the logs table ─────────────────────────────────────────────────────── */

function levelBadge(level) {
  const up = (level || "").toUpperCase();
  const cls = up === "ERROR" ? "err" : up === "WARN" ? "warn" : up === "INFO" ? "info" : "mute";
  const mark = up === "ERROR" ? "✕ " : up === "WARN" ? "! " : "";
  return el("span", "badge " + cls, mark + (up || "?"));
}

// flatten is an attribute tree as a list of dotted keys, because a log line's
// fields are read left to right and a nested object on one line is not.
function flatten(attrs, prefix) {
  const out = [];
  for (const k of Object.keys(attrs || {}).sort()) {
    const v = attrs[k];
    const key = prefix ? prefix + "." + k : k;
    if (v && typeof v === "object" && !Array.isArray(v)) {
      out.push(...flatten(v, key));
      continue;
    }
    out.push([key, typeof v === "string" ? v : JSON.stringify(v)]);
  }
  return out;
}

const attrAt = (attrs, key) => {
  const hit = flatten(attrs).find(([k]) => k === key || k.endsWith("." + key));
  return hit ? hit[1] : "";
};

function renderLevels(levels) {
  for (const b of $("level-seg").children) {
    const want = b.dataset.level;
    const n = want
      ? Object.keys(levels).reduce((sum, k) => sum + (atLeastJS(k, want) ? levels[k] : 0), 0)
      : Object.values(levels).reduce((sum, v) => sum + v, 0);

    let count = b.querySelector(".count");
    if (!count) {
      count = el("span", "count");
      b.append(document.createTextNode(" "), count);
    }
    count.textContent = n ? String(n) : "";
  }
}

// The same "this level and louder" the server filters by, so the number on a
// chip is the number of rows pressing it produces.
const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];
function atLeastJS(level, min) {
  const li = LEVELS.indexOf((level || "").toUpperCase());
  const mi = LEVELS.indexOf((min || "").toUpperCase());
  if (li < 0 || mi < 0) return true;
  return li >= mi;
}

function renderLogs(logs) {
  syncRows(logsBody, logs, (rec, i) => (rec.time || "") + "/" + i,
    (rec, key) => {
      const tr = row(key, () => toggleLogDetail(tr, rec));
      tr.classList.add("logrow");

      tr.append(whenCell(new Date(rec.time)));
      tr.append(cell("c-level", levelBadge(rec.level)));

      const msg = el("td");
      msg.append(el("span", "msg", rec.msg));
      msg.title = rec.msg;
      tr.append(msg);

      const route = attrAt(rec.attrs, "route");
      const routeCell = el("td", "c-route");
      routeCell.append(el("span", null, route));
      routeCell.title = route;
      tr.append(routeCell);

      tr.append(idCell(rec.trace_id));
      return tr;
    },
    (tr, rec) => {
      tr.children[0].firstChild.textContent = ago(new Date(rec.time));
    });
}

// toggleLogDetail expands a line in place: every attribute it carried, and the
// way back to the request it belongs to.
function toggleLogDetail(tr, rec) {
  const open = tr.classList.toggle("open");
  const next = tr.nextElementSibling;
  if (next && next.classList.contains("logdetail")) next.remove();
  if (!open) return;

  const holder = el("tr", "logdetail");
  const td = el("td");
  td.colSpan = 5;

  const kv = el("dl", "kv");
  const rows = flatten(rec.attrs);
  if (rec.span_id) rows.unshift(["span_id", rec.span_id]);
  if (rec.trace_id) rows.unshift(["trace_id", rec.trace_id]);
  rows.forEach(([k, v]) => {
    kv.append(el("dt", null, k), el("dd", null, v));
  });
  td.append(kv);

  if (rec.trace_id) {
    const jump = el("button", "jump", "Open this request →");
    jump.type = "button";
    jump.addEventListener("click", (ev) => {
      ev.stopPropagation();
      state.tab = "requests";
      state.trace = rec.trace_id;
      state.level = "";
      applyTab();
      applyFilters();
      writeHash();
      load();
    });
    td.append(jump);
  }

  holder.append(td);
  tr.after(holder);
}

/* ── the keyboard ───────────────────────────────────────────────────────── */

function typing(target) {
  return target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA");
}

function currentBody() {
  return state.tab === "requests" ? tracesBody : logsBody;
}

function move(delta) {
  const rows = [...currentBody().querySelectorAll("tr[tabindex]")];
  if (!rows.length) return;

  const here = document.activeElement && document.activeElement.closest
    ? document.activeElement.closest("tr[tabindex]")
    : null;
  const at = here ? rows.indexOf(here) : (state.trace ? rows.findIndex((r) => r.dataset.key === state.trace) : -1);
  const next = rows[Math.max(0, Math.min(rows.length - 1, at + delta))];
  if (next) next.focus();
}

document.addEventListener("keydown", (ev) => {
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return;

  if (ev.key === "Escape") {
    if (typing(ev.target)) {
      ev.target.blur();
      return;
    }
    if ($("shortcuts").open) return; // dialog closes itself
    if (state.trace) select("");
    return;
  }
  if (typing(ev.target)) return;

  switch (ev.key) {
    case "/":
      ev.preventDefault();
      $("q").focus();
      $("q").select();
      break;
    case "j": move(1); break;
    case "k": move(-1); break;
    case "e":
      if (state.tab === "requests") setStatus(state.status ? "" : "error");
      break;
    case "g": setTab("requests"); break;
    case "l": setTab("logs"); break;
    case "r": load(); break;
    case "?": $("shortcuts").showModal(); break;
  }
});

/* ── the controls ───────────────────────────────────────────────────────── */

function setTab(tab) {
  if (state.tab === tab) return;
  state.tab = tab;
  applyTab();
  renderDetail();
  writeHash();
  load();
}

function setStatus(status) {
  state.status = status;
  applyFilters();
  writeHash();
  load();
}

$("tab-requests").addEventListener("click", () => setTab("requests"));
$("tab-logs").addEventListener("click", () => setTab("logs"));

for (const b of $("status-seg").children) {
  b.addEventListener("click", () => setStatus(b.dataset.status));
}
for (const b of $("level-seg").children) {
  b.addEventListener("click", () => {
    state.level = b.dataset.level;
    applyFilters();
    writeHash();
    load();
  });
}

let typingTimer;
$("q").addEventListener("input", () => {
  clearTimeout(typingTimer);
  typingTimer = setTimeout(() => {
    state.q = $("q").value.trim();
    writeHash();
    load();
  }, 200);
});

$("refresh").addEventListener("click", load);
$("help").addEventListener("click", () => $("shortcuts").showModal());
$("shortcuts").addEventListener("click", (ev) => {
  if (ev.target === $("shortcuts")) $("shortcuts").close();
});

window.addEventListener("hashchange", () => {
  if (readingHash) {
    readingHash = false;
    return;
  }
  readHash();
  applyTab();
  applyFilters();
  load();
});

// Not while nobody is looking. A page left open in a background tab asking
// every five seconds forever is a cost this server pays for no reader.
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && $("auto").checked) load();
});

setInterval(() => {
  if ($("auto").checked && !document.hidden) load();
}, 5000);
setInterval(tickUpdated, 1000);

readHash();
applyTab();
applyFilters();
load();
