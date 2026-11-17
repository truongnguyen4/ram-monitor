'use strict';

// Layers of the stacked chart, bottom → top. Values are kB from the server.
const LAYERS = [
  { k: 'anon',   label: 'Anon (process)',          color: '#4C8DFF' },
  { k: 'shmem',  label: 'Shmem',                   color: '#F472B6' },
  { k: 'kernel', label: 'Kernel',                  color: '#F5A524' },
  { k: 'other',  label: 'Other (DMA-BUF, GPU…)',   color: '#A78BFA' },
  { k: 'zram',   label: 'zram',                    color: '#2DD4BF' },
];
const KB_PER_GB = 1024 * 1024;

const $ = (id) => document.getElementById(id);
const gb = (kb) => (kb / KB_PER_GB).toFixed(2);
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 'es'}`;
const mb = (kb) => Math.round(kb / 1024).toLocaleString('en-US');
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// OOM adjustment groups in dumpsys order (ActivityManagerService DUMP_MEM_OOM_LABEL).
const GROUP_ORDER = ['Native', 'System', 'Persistent', 'Persistent Service', 'Foreground', 'Visible',
  'Perceptible', 'Perceptible Medium', 'Perceptible Low', 'Backup', 'Heavy Weight',
  'A Services', 'Home', 'Previous', 'B Services', 'Cached'];

let cfg = { intervalMs: 1000, procIntervalMs: 10000, threshold: 80, topN: 0, maxSamples: 7200, maxPoints: 1440 };
let info = null;
let samples = [];
let procs = null;
let paused = false;
let status = { connected: false, message: 'Not connected' };
let mode = 'kernel';
let filter = '';
let groupFilter = '';
let plot = null;
let cols = null; // uPlot data columns
let selected = decodeURIComponent(location.hash.slice(1)); // serial shown in detail (kept in the URL hash)
const overview = new Map(); // serial → { info, status, paused, last, androidUsed, recent: [used kB] }
const RECENT = 300;
const q = () => `serial=${encodeURIComponent(selected)}`;

// ---------- chart ----------

function emptyCols() {
  // [x, stack top→bottom (5), used, memTotal, threshold]
  return [[], [], [], [], [], [], [], [], []];
}

function pushCols(s) {
  const x = (s.t - info.startedAt) / 1000;
  cols[0].push(x);
  let cum = 0;
  const stack = LAYERS.map((l) => (cum += s[l.k]));
  // series 1..5 are drawn top layer first so lower layers paint over it
  for (let i = 0; i < LAYERS.length; i++) cols[1 + i].push(stack[LAYERS.length - 1 - i] / KB_PER_GB);
  cols[6].push(s.used / KB_PER_GB);
  cols[7].push(s.memTotal / KB_PER_GB);
  cols[8].push((s.memTotal * cfg.threshold) / 100 / KB_PER_GB);
}

function yMax() {
  const last = samples[samples.length - 1];
  return last ? Math.ceil(last.memTotal / KB_PER_GB) : 8;
}

function buildPlot() {
  const wrap = $('chart-wrap');
  if (plot) { plot.destroy(); plot = null; }
  const top = LAYERS.slice().reverse();
  const opts = {
    width: wrap.clientWidth,
    height: 340,
    padding: [12, 8, 4, 0],
    scales: { x: { time: false }, y: { range: () => [0, yMax()] } },
    axes: [
      { show: false },
      {
        stroke: '#9BA7B4',
        size: 60,
        grid: { stroke: '#2A313C', width: 1 },
        ticks: { show: false },
        values: (u, vals) => vals.map((v) => `${+v.toFixed(1)} GB`),
      },
    ],
    legend: { show: false },
    cursor: { y: false, points: { show: false }, drag: { x: false, y: false } },
    series: [
      {},
      ...top.map((l) => ({ label: l.label, stroke: l.color, fill: l.color, width: 1, points: { show: false } })),
      { label: 'Used', stroke: '#E6EDF3', width: 2, points: { show: false } },
      { label: 'MemTotal', stroke: '#E6EDF3', width: 1, dash: [2, 4], points: { show: false } },
      { label: 'Threshold', stroke: '#F5A524', width: 1.5, dash: [6, 4], points: { show: false } },
    ],
    hooks: { setCursor: [updateTooltip] },
  };
  plot = new uPlot(opts, cols, $('chart'));
}

function updateTooltip(u) {
  const tip = $('tooltip');
  const idx = u.cursor.idx;
  if (idx == null || !samples[idx]) { tip.hidden = true; return; }
  const s = samples[idx];
  let html = `<div class="tt-total"><span>Total Used</span><span class="mono">${gb(s.used)} GB</span></div>`;
  for (const l of LAYERS.slice().reverse()) {
    html += `<div class="tt-row"><span><i style="background:${l.color}"></i>${esc(l.label)}</span><span class="mono">${gb(s[l.k])} GB</span></div>`;
  }
  tip.innerHTML = html;
  tip.hidden = false;
  const wrapRect = $('chart-wrap').getBoundingClientRect();
  const overRect = u.over.getBoundingClientRect();
  const cx = overRect.left - wrapRect.left + u.cursor.left;
  const w = tip.offsetWidth;
  const left = cx + 14 + w > wrapRect.width ? cx - 14 - w : cx + 14;
  tip.style.left = `${Math.max(0, left)}px`;
}

function renderLegend() {
  $('legend').innerHTML =
    '<span><i class="line" style="background:#E6EDF3"></i>Total Used</span>' +
    LAYERS.map((l) => `<span><i style="background:${l.color}"></i>${esc(l.label)}</span>`).join('');
}

// ---------- panels ----------

function renderHero() {
  const last = samples[samples.length - 1];
  const total = last ? last.memTotal : 0;
  let used = null;
  if (mode === 'kernel') {
    used = last ? last.used : null;
    $('formula').textContent = `Used = MemTotal − MemAvailable · every ${cfg.intervalMs / 1000} s`;
  } else {
    used = procs && procs.androidUsed ? procs.androidUsed : null;
    $('formula').textContent = `dumpsys meminfo "Used RAM" · every ${cfg.procIntervalMs / 1000} s`;
  }
  $('total').textContent = total ? `/ ${gb(total)} GB` : '/ – GB';
  if (used == null || !total) {
    $('used').textContent = '–';
    $('pct').textContent = '–';
    $('bar-fill').style.width = '0';
    return;
  }
  const pct = (used / total) * 100;
  const warn = pct >= cfg.threshold;
  $('used').textContent = gb(used);
  $('pct').textContent = `${pct.toFixed(1)}%`;
  $('pct').classList.toggle('warn', warn);
  $('bar-fill').style.width = `${Math.min(100, pct)}%`;
  $('bar-fill').classList.toggle('warn', warn);
}

function renderZram() {
  const last = samples[samples.length - 1];
  if (!last) return;
  $('zram').textContent = `${gb(last.zram)} GB`;
  $('zram-stored').textContent = `${gb(last.zramOrig)} GB`;
  $('zram-ratio').textContent = last.zram > 0 ? `${(last.zramOrig / last.zram).toFixed(1)}×` : '–';
}

function renderGroupOptions() {
  const sel = $('proc-group');
  if (document.activeElement === sel) return; // do not close the list while the user is picking
  const totals = new Map();
  for (const p of procs ? procs.rows : []) {
    const g = p.group || '';
    const t = totals.get(g) || { pss: 0, n: 0 };
    t.pss += p.pss;
    t.n += 1;
    totals.set(g, t);
  }
  // keep the selected group listed even if it has no process right now
  if (groupFilter && !totals.has(groupFilter)) totals.set(groupFilter, { pss: 0, n: 0 });
  const rank = (g) => { const i = GROUP_ORDER.indexOf(g); return i < 0 ? GROUP_ORDER.length : i; };
  const groups = [...totals.keys()].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  const opts = ['<option value="">All groups</option>'];
  for (const g of groups) {
    const t = totals.get(g);
    const label = `${g || '(none)'} · ${mb(t.pss)} MB · ${t.n}`;
    opts.push(`<option value="${esc(g)}">${esc(label)}</option>`);
  }
  sel.innerHTML = opts.join('');
  sel.value = groupFilter;
}

function renderProcs() {
  const body = $('proc-body');
  const totalBody = $('proc-total');
  renderGroupOptions();
  updateCompAddBtn();
  if (!procs) { body.innerHTML = ''; totalBody.innerHTML = ''; return; }
  const f = filter.trim().toLowerCase();
  const matched = procs.rows.filter((p) =>
    (!groupFilter || (p.group || '') === groupFilter) && (!f || p.name.toLowerCase().includes(f)));
  const rows = cfg.topN > 0 ? matched.slice(0, cfg.topN) : matched;
  const deltaCell = (kb) => {
    const d = Math.round(kb / 1024); // MB, sign follows the rounded value
    const cls = d > 0 ? 'up' : d < 0 ? 'down' : '';
    const sign = d > 0 ? '+' : d < 0 ? '−' : '±';
    return `<td class="num ${cls}">${sign}${Math.abs(d).toLocaleString('en-US')} MB</td>`;
  };

  // Totals cover every process matching the filters, not only the rows shown.
  const sum = matched.reduce((a, p) => ({ pss: a.pss + p.pss, swap: a.swap + p.swap, delta: a.delta + p.delta }),
    { pss: 0, swap: 0, delta: 0 });
  const scope = groupFilter || f ? 'filtered' : 'all';
  totalBody.innerHTML = `<tr>
      <td class="name" title="Sum over all ${matched.length} matching processes">Total (${scope}, ${plural(matched.length, 'process')})</td>
      <td><span class="group">${esc(groupFilter || 'All groups')}</span></td>
      <td class="num pss">${mb(sum.pss)} MB</td>
      <td class="num swap">${mb(sum.swap)} MB</td>
      ${deltaCell(sum.delta)}
    </tr>`;

  body.innerHTML = rows.map((p) => `<tr>
      <td class="name" title="${esc(p.name)} (pid ${p.pid})">${esc(p.name)}</td>
      <td><span class="group">${esc(p.group || '–')}</span></td>
      <td class="num pss">${mb(p.pss)} MB</td>
      <td class="num swap">${mb(p.swap)} MB</td>
      ${deltaCell(p.delta)}
    </tr>`).join('');
  const t = new Date(procs.t).toLocaleTimeString('en-GB');
  const shown = rows.length < matched.length ? `showing top ${rows.length} of ${plural(matched.length, 'process')}` : plural(matched.length, 'process');
  $('proc-meta').textContent =
    `${shown} · updated every ${cfg.procIntervalMs / 1000} s · last ${t}`;
}

function renderStatus() {
  const dot = $('status-dot');
  dot.className = 'dot';
  let text = status.message || '';
  if (!info) { text = 'No device selected'; }
  else if (paused) { dot.classList.add('paused'); text = 'Paused'; }
  else if (status.connected) { dot.classList.add('ok'); text = 'Recording'; }
  else { dot.classList.add('err'); }
  $('status-text').textContent = text;
  $('status-meta').textContent = info ? `· adb · /proc/meminfo every ${cfg.intervalMs / 1000} s` : '';
  $('pause-btn').disabled = !info;
  $('restart-btn').disabled = !info;
  $('export-btn').setAttribute('aria-disabled', info ? 'false' : 'true');
  $('pause-label').textContent = paused ? 'Resume' : 'Pause';
  $('pause-icon').setAttribute('d', paused ? 'M7 5l12 7-12 7z' : 'M9 5v14M15 5v14');
}

function renderElapsed() {
  if (!info) { $('elapsed').textContent = '00:00:00'; return; }
  const s = Math.max(0, Math.floor((Date.now() - info.startedAt) / 1000));
  const p = (n) => String(n).padStart(2, '0');
  $('elapsed').textContent = `${p(Math.floor(s / 3600))}:${p(Math.floor((s % 3600) / 60))}:${p(s % 60)}`;
}

function renderAll() {
  const waiting = !info && overview.has(selected);
  $('empty').hidden = !!info;
  $('empty').querySelector('h2').textContent = waiting ? `Waiting for ${selected}` : 'No device monitored';
  $('empty').querySelector('p').innerHTML = waiting
    ? 'This device is remembered but not connected. Monitoring resumes by itself when adb sees it again (checked every 15 s).'
    : 'Connect an Android device with USB debugging enabled and tick it in <b>Devices</b> above.';
  // several instances can run side by side: name the tab after the device
  document.title = info ? `RAM · ${info.model || 'Device'} · ${info.serial}` : 'RAM Monitor';
  $('main').hidden = !info;
  $('th-label').textContent = `${cfg.threshold}%`;
  $('bar-tick').style.left = `${cfg.threshold}%`;
  $('csv-path').textContent = info ? `Saving to: ${info.csvPath}` : '';
  renderStatus();
  renderElapsed();
  renderHero();
  renderZram();
  renderProcs();
}

// ---------- components ----------
// A component is a saved table filter (group and/or name). It is drawn as a horizontal bar whose
// length is the total PSS of the matching processes at the latest dumpsys poll. The process history
// kept by the server gives the change since the start of the session.

const COMP_COLORS = ['#4C8DFF', '#F5A524', '#2DD4BF', '#F472B6', '#A78BFA', '#3FB950', '#FF7B72', '#E6EDF3'];
const COMP_STORE = 'ram-monitor.components';

let comps = loadComps();     // [{ id, group, name, color }], name is lower-case
let hist = emptyHist();      // { keys: [[name, group]], idx: Map, points: [{ t, k, p }] }

// Mirrors session.ThinSamples / ThinPoints on the server: the older half is thinned 2:1,
// the first entry is kept, and for samples each pair keeps the higher Used (peaks survive).
function thinSamples(xs) {
  const half = Math.floor(xs.length / 2);
  const out = [xs[0]];
  let i = 1;
  for (; i + 1 < half; i += 2) out.push(xs[i + 1].used >= xs[i].used ? xs[i + 1] : xs[i]);
  return out.concat(xs.slice(i));
}
function thinPoints(ps) {
  const half = Math.floor(ps.length / 2);
  const out = [ps[0]];
  let i = 1;
  for (; i + 1 < half; i += 2) out.push(ps[i + 1]);
  return out.concat(ps.slice(i));
}

function emptyHist() { return { keys: [], idx: new Map(), points: [] }; }

function loadComps() {
  try {
    const v = JSON.parse(localStorage.getItem(COMP_STORE) || '[]');
    return Array.isArray(v) ? v.filter((c) => c && typeof c.id === 'number') : [];
  } catch (_) { return []; }
}

function saveComps() {
  try {
    localStorage.setItem(COMP_STORE, JSON.stringify(comps.map(({ id, group, name, color }) => ({ id, group, name, color }))));
  } catch (_) { /* storage unavailable: components last until reload */ }
}

function compLabel(c) {
  const parts = [];
  if (c.group) parts.push(c.group);
  if (c.name) parts.push(`name ~ "${c.name}"`);
  return parts.length ? parts.join(' + ') : 'All processes';
}

function setHistory(h) {
  hist = emptyHist();
  const keys = (h && h.keys) || [];
  keys.forEach((k, i) => { hist.keys.push(k); hist.idx.set(`${k[0]}\u0000${k[1]}`, i); });
  hist.points = ((h && h.points) || []).map((pt) => ({ t: pt.t, k: pt.k || [], p: pt.p || [] }));
  comps.forEach((c) => { c.m = null; });
}

// Adds a dumpsys poll received live (same data the server appends to its history).
function appendHistory(snap) {
  const last = hist.points[hist.points.length - 1];
  if (last && snap.t <= last.t) return null; // already in the history loaded from /api/state
  const pt = { t: snap.t, k: [], p: [] };
  for (const r of snap.rows) {
    const key = `${r.name}\u0000${r.group || ''}`;
    let i = hist.idx.get(key);
    if (i === undefined) {
      i = hist.keys.length;
      hist.keys.push([r.name, r.group || '']);
      hist.idx.set(key, i);
    }
    pt.k.push(i);
    pt.p.push(r.pss);
  }
  hist.points.push(pt);
  if (hist.points.length > cfg.maxPoints) hist.points = thinPoints(hist.points);
  return pt;
}

function compMatch(c) {
  // per history key: does it belong to the component? (extended as new keys appear)
  const m = c.m || (c.m = []);
  for (let i = m.length; i < hist.keys.length; i++) {
    const [name, group] = hist.keys[i];
    m.push((!c.group || group === c.group) && (!c.name || name.toLowerCase().includes(c.name)));
  }
  return m;
}

// Total PSS (kB) of the component at one history point.
function compSum(c, pt) {
  const m = compMatch(c);
  let kb = 0;
  for (let i = 0; i < pt.k.length; i++) if (m[pt.k[i]]) kb += pt.p[i];
  return kb;
}

// Current and first total (kB) of each component, plus the number of matching processes now.
function compValues() {
  const pts = hist.points;
  const last = pts[pts.length - 1];
  return comps.map((c) => {
    if (!last) return { cur: null, first: null, n: 0 };
    const m = compMatch(c);
    return { cur: compSum(c, last), first: compSum(c, pts[0]), n: last.k.filter((k) => m[k]).length };
  });
}

function renderComps() {
  const vals = compValues();
  const max = Math.max(1, ...vals.map((v) => v.cur || 0));
  $('comp-list').innerHTML = comps.map((c, i) => {
    const v = vals[i];
    const pct = v.cur == null ? 0 : (v.cur / max) * 100;
    const d = v.cur == null ? 0 : Math.round((v.cur - v.first) / 1024);
    const cls = d > 0 ? 'up' : d < 0 ? 'down' : '';
    const sign = d > 0 ? '+' : d < 0 ? '−' : '±';
    return `<div class="comp-item">
      <button type="button" class="comp-apply" data-id="${c.id}" title="${esc(compLabel(c))} · ${plural(v.n, 'process')} · click to apply this filter to the table">${esc(compLabel(c))}</button>
      <div class="comp-bar" title="${esc(compLabel(c))}"><span style="width:${pct.toFixed(1)}%;background:${c.color}"></span></div>
      <span class="comp-val">${v.cur == null ? '–' : `${mb(v.cur)} MB`}</span>
      <span class="comp-delta ${cls}" title="Change since the start of the session">${v.cur == null ? '' : `${sign}${Math.abs(d).toLocaleString('en-US')} MB`}</span>
      <button type="button" class="comp-del" data-id="${c.id}" aria-label="Remove ${esc(compLabel(c))}" title="Remove">×</button>
    </div>`;
  }).join('');
  $('comp-hint').hidden = comps.length > 0;
  $('comp-meta').textContent = comps.length && hist.points.length
    ? `PSS at the last poll · ${new Date(hist.points[hist.points.length - 1].t).toLocaleTimeString('en-GB')}` : '';
  updateCompAddBtn();
}

function currentComp() { return { group: groupFilter, name: filter.trim().toLowerCase() }; }

function updateCompAddBtn() {
  const cur = currentComp();
  const dup = comps.some((c) => c.group === cur.group && c.name === cur.name);
  const btn = $('comp-add');
  btn.disabled = !info || dup || comps.length >= COMP_COLORS.length;
  btn.title = dup ? 'This filter is already a component'
    : comps.length >= COMP_COLORS.length ? `At most ${COMP_COLORS.length} components`
    : 'Save the current group/name filter of the table as a component';
}

function addComp() {
  const cur = currentComp();
  const used = new Set(comps.map((c) => c.color));
  const color = COMP_COLORS.find((col) => !used.has(col)) || COMP_COLORS[0];
  comps.push({ id: Date.now(), group: cur.group, name: cur.name, color });
  saveComps();
  renderComps();
}

function onCompPoint(pt) {
  if (pt) renderComps();
}

// ---------- devices ----------
// Every monitored device has a tile in the strip; the selected one is shown in detail below.

// Tiles are updated in place (not rebuilt) so a click is not lost when a sample arrives mid-click.
function newTile(serial) {
  const el = document.createElement('div');
  el.className = 'dev';
  el.dataset.serial = serial;
  el.tabIndex = 0;
  el.setAttribute('role', 'button');
  el.innerHTML = `<div class="dev-head"><span class="dot"></span><b></b><span class="dev-key"></span></div>
    <div class="dev-sub"></div>
    <div class="dev-num"><span class="v"></span><span class="p"></span></div>
    <svg viewBox="0 0 100 30" preserveAspectRatio="none" aria-hidden="true"></svg>
    <button type="button" class="dev-x" title="Stop monitoring (CSV files are kept)">×</button>`;
  el.querySelector('.dev-x').dataset.stop = serial;
  el.querySelector('.dev-x').setAttribute('aria-label', `Stop monitoring ${serial}`);
  return el;
}

function updateTile(el, serial, ov, i) {
  const last = ov.last;
  const total = last ? last.memTotal : 0;
  const pct = total ? (last.used / total) * 100 : null;
  const warn = pct != null && pct >= cfg.threshold;
  const conn = ov.status && ov.status.connected;
  const state = ov.paused ? 'Paused' : conn ? 'Recording' : (ov.status && ov.status.message) || 'Not connected';
  const inf = ov.info || {};
  el.classList.toggle('sel', serial === selected);
  el.classList.toggle('warn', warn);
  el.classList.toggle('offline', !!ov.offline);
  el.setAttribute('aria-pressed', String(serial === selected));
  el.title = `${state} · click to view in detail${i < 9 ? ` (key ${i + 1})` : ''}`;
  el.querySelector('.dot').className = `dot ${ov.paused ? 'paused' : conn ? 'ok' : 'err'}`;
  el.querySelector('b').textContent = inf.model || 'Device';
  const key = el.querySelector('.dev-key');
  key.textContent = i < 9 ? String(i + 1) : '';
  key.hidden = i >= 9;
  el.querySelector('.dev-sub').textContent =
    [inf.serial, inf.android && `Android ${inf.android}`, inf.physicalGB && `${inf.physicalGB} GB`].filter(Boolean).join(' · ');
  el.querySelector('.v').textContent = last ? `${gb(last.used)} GB` : '–';
  const p = el.querySelector('.p');
  p.textContent = pct == null ? state : `${pct.toFixed(1)}% of ${gb(total)}`;
  p.classList.toggle('warn', warn);
  let svg = '';
  if (total && ov.recent.length > 1) {
    const n = ov.recent.length;
    const pts = ov.recent.map((u, j) => `${((j / (n - 1)) * 100).toFixed(2)},${(30 - (u / total) * 30).toFixed(2)}`).join(' ');
    const th = (30 - (cfg.threshold / 100) * 30).toFixed(2);
    svg = `<line x1="0" x2="100" y1="${th}" y2="${th}"/><polyline points="${pts}"/>`;
  }
  el.querySelector('svg').innerHTML = svg;
}

function renderDevices() {
  const el = $('devices');
  const tiles = new Map([...el.children].map((t) => [t.dataset.serial, t]));
  let i = 0;
  for (const [serial, ov] of overview) {
    let t = tiles.get(serial);
    if (!t) t = newTile(serial);
    tiles.delete(serial);
    if (el.children[i] !== t) el.insertBefore(t, el.children[i] || null);
    updateTile(t, serial, ov, i);
    i++;
  }
  for (const t of tiles.values()) t.remove();
}

let devRenderQueued = false;
function queueRenderDevices() {
  if (devRenderQueued) return;
  devRenderQueued = true;
  requestAnimationFrame(() => { devRenderQueued = false; renderDevices(); });
}

// Loads the overview of every monitored device and makes sure a valid device is selected.
async function loadSessions() {
  let res = {};
  try { res = await (await fetch('api/sessions')).json(); } catch (_) { /* keep the empty list */ }
  overview.clear();
  for (const x of res.sessions || []) {
    overview.set(x.info.serial, { info: x.info, status: x.status, paused: x.paused, last: x.last, androidUsed: x.androidUsed, recent: x.recent || [] });
  }
  // remembered devices that are not connected: shown greyed until they come back (re-attached automatically)
  for (const serial of res.wanted || []) {
    if (!overview.has(serial)) {
      overview.set(serial, { info: { serial, model: (lastDevs.find((d) => d.serial === serial) || {}).model }, offline: true,
        status: { connected: false, message: 'waiting for the device' }, recent: [] });
    }
  }
  $('picker-label').textContent = overview.size ? `Devices · ${overview.size}` : 'Devices';
  if ($('picker').open) renderPicker();
  if (!overview.has(selected)) selected = overview.size ? overview.keys().next().value : '';
  renderDevices();
}

async function select(serial) {
  if (!serial || serial === selected) return;
  selected = serial;
  history.replaceState(null, '', `#${encodeURIComponent(serial)}`);
  renderDevices();
  await loadState();
}

let lastDevs = []; // last `adb devices` answer
const pending = new Set(); // serials with a start/stop request in flight

function renderPicker() {
  const rows = lastDevs.map((d) => ({ serial: d.serial, model: d.model, state: d.state }));
  for (const serial of overview.keys()) if (!rows.some((r) => r.serial === serial)) rows.push({ serial, model: (overview.get(serial).info || {}).model, state: 'not connected' });
  const ready = rows.filter((r) => r.state === 'device');
  const allOn = ready.length > 0 && ready.every((r) => overview.has(r.serial));
  const allRow = `<label class="all-row"><input type="checkbox" data-all="1" ${allOn ? 'checked' : ''} ${ready.length && !pending.has('all') ? '' : 'disabled'}>
      <span><b>All devices</b></span><span class="ser">${overview.size} / ${ready.length} watched</span></label>`;
  $('picker-list').innerHTML = rows.length ? allRow + rows.map((r) => {
    const on = overview.has(r.serial);
    const usable = r.state === 'device' || on; // an unauthorized/offline unit cannot be started, but can be removed
    return `<label class="${usable ? '' : 'off'}"><input type="checkbox" data-serial="${esc(r.serial)}" ${on ? 'checked' : ''} ${usable && !pending.has(r.serial) ? '' : 'disabled'}>
      <span>${esc(r.model || 'Device')}${r.state !== 'device' ? ` <span class="muted small">(${esc(r.state)})</span>` : ''}</span><span class="ser">${esc(r.serial)}</span></label>`;
  }).join('') : '<span class="muted small">No device found</span>';
}

async function loadDevices() {
  try {
    const devs = await (await fetch('api/devices')).json();
    if (!Array.isArray(devs)) throw new Error(devs.error || 'adb error');
    lastDevs = devs;
    renderPicker();
  } catch (e) {
    $('picker-list').innerHTML = `<span class="muted small">adb error: ${esc(e.message)}</span>`;
  }
}

async function setMonitored(serial, on) {
  pending.add(serial);
  try {
    const r = on
      ? await fetch('api/device', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ serial }) })
      : await fetch(`api/device?serial=${encodeURIComponent(serial)}`, { method: 'DELETE' });
    if (!r.ok) {
      const err = await r.json().catch(() => ({}));
      alert(`Cannot ${on ? 'monitor' : 'stop'} ${serial}: ${err.error || r.status}`);
    }
  } finally {
    pending.delete(serial);
  }
}

// ---------- state & events ----------

async function loadState() {
  const want = selected;
  const st = want ? await (await fetch(`api/state?${q()}`)).json() : { info: null };
  if (want !== selected) return; // the user picked another device meanwhile
  info = st.info || null;
  samples = st.samples || [];
  procs = st.procs || null;
  setHistory(st.procHistory);
  paused = !!st.paused;
  status = st.status || status;
  cols = emptyCols();
  if (info) samples.forEach(pushCols);
  renderAll();
  $('export-btn').href = `api/export.csv?${q()}`;
  if (info) {
    buildPlot();
  } else if (plot) {
    plot.destroy();
    plot = null;
  }
  renderComps();
  loadDevices();
}

function connectEvents() {
  const es = new EventSource('api/events');
  // Every event is {serial, v}. The overview strip follows all devices; the detail view only the selected one.
  const on = (name, fn) => es.addEventListener(name, (e) => {
    const { serial, v } = JSON.parse(e.data);
    fn(serial, v, overview.get(serial));
  });
  on('sample', (serial, s, ov) => {
    if (ov) {
      ov.last = s;
      ov.recent.push(s.used);
      if (ov.recent.length > RECENT) ov.recent.splice(0, ov.recent.length - RECENT);
      if (!ov.status || !ov.status.connected) ov.status = { connected: true, message: 'Recording' };
      queueRenderDevices();
    }
    if (serial !== selected || !info) return;
    samples.push(s);
    if (samples.length > cfg.maxSamples) { // same thinning as the server, so a long session stays light
      samples = thinSamples(samples);
      cols = emptyCols();
      samples.forEach(pushCols);
    } else {
      pushCols(s);
    }
    if (plot) plot.setData(cols);
    renderHero();
    renderZram();
    if (!status.connected) { status = { connected: true, message: 'Recording' }; renderStatus(); }
  });
  on('procs', (serial, p, ov) => {
    if (ov) ov.androidUsed = p.androidUsed;
    if (serial !== selected) return;
    procs = p;
    renderProcs();
    renderHero();
    if (info) onCompPoint(appendHistory(procs));
  });
  on('status', (serial, st, ov) => {
    if (ov) { ov.status = st; queueRenderDevices(); }
    if (serial === selected) { status = st; renderStatus(); }
  });
  on('paused', (serial, p, ov) => {
    if (ov) { ov.paused = p; queueRenderDevices(); }
    if (serial === selected) { paused = p; renderStatus(); }
  });
  // a device was added or restarted
  on('session', async (serial) => {
    const wasEmpty = !selected;
    await loadSessions();
    if (serial === selected || wasEmpty) await loadState();
  });
  on('stopped', async (serial) => {
    const wasSelected = serial === selected;
    await loadSessions();
    if (wasSelected) await loadState();
    else renderAll();
  });
  es.onopen = async () => { await loadSessions(); await loadState(); }; // resync after reconnect
}

// ---------- wiring ----------

$('picker').addEventListener('toggle', () => { if ($('picker').open) { renderPicker(); loadDevices(); } });
document.addEventListener('pointerdown', (e) => { if ($('picker').open && !$('picker').contains(e.target)) $('picker').open = false; });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') $('picker').open = false; });
$('picker-list').addEventListener('change', async (e) => {
  if (e.target.dataset.all) { // All devices: watch every ready device, or stop them all
    const on = e.target.checked;
    e.target.disabled = true;
    if (on) {
      await setMonitored('all', true);
    } else if (confirm(`Stop watching all ${overview.size} devices?\nTheir CSV files are kept.`)) {
      await Promise.all([...overview.keys()].map((serial) => setMonitored(serial, false)));
    }
    await loadSessions();
    await loadState();
    renderPicker();
    return;
  }
  const cb = e.target.closest('input[data-serial]');
  if (!cb) return;
  const serial = cb.dataset.serial;
  cb.disabled = true;
  await setMonitored(serial, cb.checked);
  await loadSessions();
  if (cb.checked && !info) await loadState();
  else if (!cb.checked && serial === selected) await loadState();
  renderPicker();
});


$('devices').addEventListener('click', async (e) => {
  const stop = e.target.closest('[data-stop]');
  if (stop) {
    e.stopPropagation();
    const serial = stop.dataset.stop;
    if (!confirm(`Stop monitoring ${serial}?\nThe CSV files of its session are kept.`)) return;
    await fetch(`api/device?serial=${encodeURIComponent(serial)}`, { method: 'DELETE' });
    return; // the "stopped" event refreshes the view
  }
  const tile = e.target.closest('.dev');
  if (tile) select(tile.dataset.serial);
});
$('devices').addEventListener('keydown', (e) => {
  const tile = e.target.closest('.dev');
  if (tile && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); select(tile.dataset.serial); }
});
// keys 1–9 select a device tile (ignored while typing)
document.addEventListener('keydown', (e) => {
  if (e.ctrlKey || e.metaKey || e.altKey || /^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) return;
  const n = Number(e.key);
  if (n >= 1 && n <= 9) { const serial = [...overview.keys()][n - 1]; if (serial) select(serial); }
});
window.addEventListener('hashchange', () => select(decodeURIComponent(location.hash.slice(1))));

$('pause-btn').addEventListener('click', async () => {
  const r = await fetch(`api/pause?${q()}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ paused: !paused }) });
  if (r.ok) {
    paused = (await r.json()).paused;
    const ov = overview.get(selected);
    if (ov) { ov.paused = paused; renderDevices(); }
    renderStatus();
  }
});

$('restart-btn').addEventListener('click', async () => {
  if (!info) return;
  if (!confirm('Clear the displayed data and restart monitoring from scratch?\nThe CSV files of the current session are kept.')) return;
  const btn = $('restart-btn');
  btn.disabled = true;
  try {
    const r = await fetch(`api/restart?${q()}`, { method: 'POST' });
    if (!r.ok) {
      const err = await r.json().catch(() => ({}));
      status = { connected: false, message: `Cannot restart: ${err.error || r.status}` };
      renderStatus();
      return;
    }
    await loadState();
  } finally {
    btn.disabled = !info;
  }
});

document.querySelectorAll('.seg button').forEach((b) => {
  b.addEventListener('click', () => {
    mode = b.dataset.mode;
    document.querySelectorAll('.seg button').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
    renderHero();
  });
});

$('proc-filter').addEventListener('input', (e) => { filter = e.target.value; renderProcs(); });
$('proc-group').addEventListener('change', (e) => { groupFilter = e.target.value; renderProcs(); });
$('comp-add').addEventListener('click', addComp);
$('comp-list').addEventListener('click', (e) => {
  const btn = e.target.closest('button[data-id]');
  if (!btn) return;
  const c = comps.find((x) => x.id === Number(btn.dataset.id));
  if (!c) return;
  if (btn.classList.contains('comp-del')) {
    comps = comps.filter((x) => x !== c);
    saveComps();
    renderComps();
  } else {
    groupFilter = c.group;
    filter = c.name;
    $('proc-filter').value = c.name;
    renderProcs();
  }
});
$('chart-wrap').addEventListener('mouseleave', () => { $('tooltip').hidden = true; });

new ResizeObserver(() => {
  if (plot) plot.setSize({ width: $('chart-wrap').clientWidth, height: 340 });
}).observe($('chart-wrap'));

setInterval(renderElapsed, 1000);

(async () => {
  try { cfg = await (await fetch('api/config')).json(); } catch (_) { /* defaults */ }
  renderLegend();
  connectEvents();
})();
