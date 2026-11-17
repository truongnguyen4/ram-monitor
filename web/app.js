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

let cfg = { intervalMs: 1000, procIntervalMs: 10000, threshold: 80, topN: 0 };
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
  $('empty').hidden = !!info;
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

async function loadDevices() {
  const sel = $('device');
  try {
    const devs = await (await fetch('api/devices')).json();
    if (!Array.isArray(devs)) throw new Error(devs.error || 'adb error');
    const opts = ['<option value="">Select a device…</option>'];
    for (const d of devs) {
      const ram = info && info.serial === d.serial && info.physicalGB ? ` · RAM ~${info.physicalGB} GB` : '';
      const android = info && info.serial === d.serial && info.android ? ` · Android ${info.android}` : '';
      const label = `${d.model || 'Device'} · ${d.serial}${android}${ram}${d.state !== 'device' ? ` (${d.state})` : ''}`;
      opts.push(`<option value="${esc(d.serial)}" ${d.state !== 'device' ? 'disabled' : ''}>${esc(label)}</option>`);
    }
    if (info && !devs.some((d) => d.serial === info.serial)) {
      opts.push(`<option value="${esc(info.serial)}">${esc(info.model || info.serial)} · ${esc(info.serial)} (disconnected)</option>`);
    }
    sel.innerHTML = opts.join('');
    sel.value = info ? info.serial : '';
  } catch (e) {
    sel.innerHTML = `<option value="">adb error: ${esc(e.message)}</option>`;
  }
}

// ---------- state & events ----------

async function loadState() {
  const st = await (await fetch('api/state')).json();
  info = st.info || null;
  samples = st.samples || [];
  procs = st.procs || null;
  setHistory(st.procHistory);
  paused = !!st.paused;
  status = st.status || status;
  cols = emptyCols();
  if (info) samples.forEach(pushCols);
  renderAll();
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
  es.addEventListener('sample', (e) => {
    if (!info) return;
    const s = JSON.parse(e.data);
    samples.push(s);
    pushCols(s);
    if (plot) plot.setData(cols);
    renderHero();
    renderZram();
    if (!status.connected) { status = { connected: true, message: 'Recording' }; renderStatus(); }
  });
  es.addEventListener('procs', (e) => {
    procs = JSON.parse(e.data);
    renderProcs();
    renderHero();
    if (info) onCompPoint(appendHistory(procs));
  });
  es.addEventListener('status', (e) => { status = JSON.parse(e.data); renderStatus(); });
  es.addEventListener('paused', (e) => { paused = JSON.parse(e.data); renderStatus(); });
  es.addEventListener('session', () => loadState());
  es.onopen = () => loadState(); // resync after reconnect
}

// ---------- wiring ----------

$('device').addEventListener('focus', loadDevices);
$('device').addEventListener('change', async (e) => {
  const serial = e.target.value;
  if (!serial || (info && serial === info.serial)) return;
  const r = await fetch('api/device', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ serial }) });
  if (!r.ok) {
    const err = await r.json().catch(() => ({}));
    status = { connected: false, message: `Cannot connect: ${err.error || r.status}` };
    renderStatus();
  }
  await loadState();
});

$('pause-btn').addEventListener('click', async () => {
  const r = await fetch('api/pause', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ paused: !paused }) });
  if (r.ok) { paused = (await r.json()).paused; renderStatus(); }
});

$('restart-btn').addEventListener('click', async () => {
  if (!info) return;
  if (!confirm('Clear the displayed data and restart monitoring from scratch?\nThe CSV files of the current session are kept.')) return;
  const btn = $('restart-btn');
  btn.disabled = true;
  try {
    const r = await fetch('api/restart', { method: 'POST' });
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
