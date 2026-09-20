/* agent of chaos dashboard — vanilla JS, no build step. Polls /harness/stats
   (report + live) and /harness/timeseries once a second. */
(() => {
'use strict';

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// ── formatting ───────────────────────────────────────────────────────────────
const fmt = {
  int: (n) => (n == null ? '–' : Math.round(n).toLocaleString('en-US')),
  compact: (n) => {
    if (n == null || isNaN(n)) return '–';
    const a = Math.abs(n);
    if (a < 1000) return (Number.isInteger(n) ? n : n.toFixed(1)).toString();
    if (a < 1e6) return (n / 1e3).toFixed(a < 1e4 ? 2 : 1) + 'k';
    if (a < 1e9) return (n / 1e6).toFixed(2) + 'M';
    return (n / 1e9).toFixed(2) + 'B';
  },
  bytes: (n) => {
    if (n == null || isNaN(n) || n < 0) return '–';
    if (n < 1000) return n.toFixed(0) + ' B';
    if (n < 1e6) return (n / 1e3).toFixed(1) + ' kB';
    if (n < 1e9) return (n / 1e6).toFixed(1) + ' MB';
    return (n / 1e9).toFixed(2) + ' GB';
  },
  secs: (s) => {
    if (s == null || isNaN(s)) return '–';
    if (s === 0) return '0';
    if (s < 0.001) return (s * 1e6).toFixed(0) + 'µs';
    if (s < 1) return (s * 1000).toFixed(0) + 'ms';
    if (s < 60) return s.toFixed(2) + 's';
    return (s / 60).toFixed(1) + 'm';
  },
  pct: (r, d = 2) => (r == null || isNaN(r) ? '–' : (r * 100).toFixed(d) + '%'),
  dur: (s) => {
    if (s == null) return '–';
    s = Math.round(s);
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm' + String(s % 60).padStart(2, '0') + 's';
    return Math.floor(s / 3600) + 'h' + String(Math.floor((s % 3600) / 60)).padStart(2, '0') + 'm';
  },
  ago: (s) => (s == null ? '–' : s < 2 ? 'now' : fmt.dur(s) + ' ago'),
  time: (t) => new Date(t * 1000).toLocaleTimeString('en-GB', { hour12: false }),
};
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// ── state ────────────────────────────────────────────────────────────────────
const state = { paused: false, window: 600, points: [], lastT: 0, snap: null, streamFilter: '', telemetryFilter: 'logs' };

async function getJSON(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

async function poll() {
  if (state.paused) return;
  try {
    const now = Math.floor(Date.now() / 1000);
    const since = state.lastT ? state.lastT + 1 : (state.window ? now - state.window : 0);
    const [snap, pts] = await Promise.all([getJSON('/harness/stats'), getJSON('/harness/timeseries?since=' + since)]);
    state.snap = snap;
    if (pts.length) {
      state.points = state.points.concat(pts);
      state.lastT = pts[pts.length - 1].t;
    }
    // drop points outside the largest window we ever show (keep 24h max)
    const cutoff = now - 86400;
    if (state.points.length && state.points[0].t < cutoff) state.points = state.points.filter((p) => p.t >= cutoff);
    render();
    setConn(true);
  } catch (e) {
    setConn(false, e);
  }
}

function setConn(ok, err) {
  const el = $('#run-line');
  if (!ok) { el.textContent = 'intake unreachable — ' + (err && err.message ? err.message : 'retrying'); el.classList.add('status-critical'); }
  else el.classList.remove('status-critical');
}

// ── render ───────────────────────────────────────────────────────────────────
function render() {
  const { report: r, live } = state.snap;
  renderHeader(r, live);
  renderTiles(r, live);
  renderCharts();
  renderLedger(r, live);
  renderFaults(r, live);
  renderStreams(r);
  renderGens(live, r);
  renderHTTP(r);
  renderTags(r);
  renderBreakdown(r);
  renderTelemetry(r, live);
  renderSamples(live);
  $('#version').textContent = live.version || '';
}

function renderHeader(r, live) {
  const ver = Object.keys(r.agent.versions || {});
  const enc = Object.keys(r.agent.encodings || {});
  $('#run-line').textContent = `run “${r.name}” · window ${fmt.dur(r.seconds)} · intake up ${fmt.dur(live.uptime_seconds)}` +
    (ver.length ? ` · agent ${ver.join(', ')}` : '') + (r.agent.image ? ` (${r.agent.image})` : '') + (enc.length ? ` · ${enc.join('/')}` : '');
  const chips = [];
  const gens = live.generators || [];
  const active = gens.filter((g) => !g.final && g.ago_seconds < 5).length;
  chips.push(chip(gens.length ? (active ? 'ok' : 'warn') : '', `${active}/${gens.length} generators`, gens.length ? '' : 'no generator has reported yet — run aoc generate --intake <this url>'));
  const o = live.observer || {};
  if (o.telemetry_url) chips.push(chip(o.telemetry_ok ? 'ok' : 'bad', 'agent telemetry', o.telemetry_error || o.telemetry_url));
  if (o.docker_container) chips.push(chip(o.docker_ok ? 'ok' : 'bad', 'docker stats', o.docker_error || o.docker_container));
  chips.push(chip(live.faults_active ? 'warn' : 'ok', live.faults_active ? 'faults: ' + faultDesc(live.faults) : 'no faults', ''));
  if (live.inflight > 0) chips.push(chip('ok', `${live.inflight} in flight`, ''));
  $('#chips').innerHTML = chips.join('');
}
function chip(cls, text, title) { return `<span class="chip ${cls}" title="${esc(title)}"><span class="dot"></span>${esc(text)}</span>`; }

function tile(label, value, unit, delta, cls = '') {
  return `<div class="tile ${cls}"><div class="label">${esc(label)}</div><div class="value">${value}${unit ? `<span class="unit">${esc(unit)}</span>` : ''}</div><div class="delta">${delta || ''}</div></div>`;
}

function renderTiles(r, live) {
  const d = r.delivery, rc = live.recent, t = r.throughput, res = r.resources, o = live.observer || {};
  const ratioCls = d.generated_records === 0 ? '' : d.ratio >= 0.99999 ? 'status-good' : d.ratio >= 0.999 ? 'status-warn' : 'status-critical';
  const missingLabel = d.all_generators_final ? 'lost' : 'outstanding';
  const tiles = [
    tile('Generated', fmt.compact(rc.gen_records_per_sec), 'rec/s', `${fmt.int(d.generated_records)} total · ${fmt.bytes(rc.gen_bytes_per_sec)}/s`),
    tile('Received', fmt.compact(rc.recv_logs_per_sec), 'logs/s', `${fmt.int(d.received_logs)} total · peak ${fmt.compact(t.peak_recv_logs_per_sec)}/s`),
    tile('Delivered', `<span class="${ratioCls}">${d.generated_records ? fmt.pct(d.ratio, d.ratio >= 0.9999 ? 4 : 2) : '–'}</span>`, '', `${fmt.int(d.unique)} unique · ${fmt.int(d.missing)} ${missingLabel}`, 'hero'),
    tile('Duplicates', fmt.int(d.duplicates), '', `${fmt.int(d.out_of_order)} out of order · ${fmt.int(d.orphans)} orphan lines`),
    tile('Latency e2e', fmt.secs(rc.e2e_p99), 'p99', `p50 ${fmt.secs(rc.e2e_p50)} · sender p99 ${fmt.secs(rc.sender_p99)}`),
    tile('On the wire', fmt.bytes(rc.recv_wire_bytes_per_sec), '/s', `raw ${fmt.bytes(rc.recv_raw_bytes_per_sec)}/s · ${t.compression_ratio ? t.compression_ratio.toFixed(1) + '× compression' : '–'}`),
    tile('Agent CPU', o.docker_ok ? o.cpu_percent.toFixed(0) : o.telemetry_ok ? o.proc_cpu_percent.toFixed(0) : 'n/a', o.docker_ok || o.telemetry_ok ? '%' : '', o.docker_ok ? `container · core process ${o.telemetry_ok ? o.proc_cpu_percent.toFixed(0) + '%' : '–'} · avg ${res.container_cpu_avg_percent.toFixed(0)}%` : o.telemetry_ok ? 'core process (from telemetry)' : 'enable --docker-container / --agent-telemetry'),
    tile('Agent memory', o.docker_ok ? fmt.bytes(o.mem_bytes) : o.telemetry_ok ? fmt.bytes(o.proc_rss) : 'n/a', '', o.docker_ok ? `container · max ${fmt.bytes(res.container_mem_max_bytes)} · ${o.pids} pids` : o.telemetry_ok ? 'core process RSS' : ''),
  ];
  $('#tiles').innerHTML = tiles.join('');
}

// ── charts ───────────────────────────────────────────────────────────────────
const charts = {
  throughput: { unit: 'rate', series: [{ name: 'generated rec/s', color: '--s1', get: (p) => p.gen_records }, { name: 'received logs/s', color: '--s2', get: (p) => p.logs }] },
  latency: { unit: 'secs', series: [{ name: 'p50', color: '--s1', get: (p) => (p.e2e_p50 > 0 ? p.e2e_p50 : null) }, { name: 'p99', color: '--s2', get: (p) => (p.e2e_p99 > 0 ? p.e2e_p99 : null) }] },
  bytes: { unit: 'bytes', series: [{ name: 'raw (decompressed) B/s', color: '--s1', get: (p) => p.raw_bytes }, { name: 'wire B/s', color: '--s2', get: (p) => p.wire_bytes }] },
  cpu: { unit: 'pct', series: [{ name: 'container %', color: '--s1', get: (p) => (p.agent_cpu >= 0 ? p.agent_cpu : null) }, { name: 'core process %', color: '--s2', get: (p) => (p.proc_cpu >= 0 ? p.proc_cpu : null) }] },
  mem: { unit: 'bytes', series: [{ name: 'container', color: '--s1', get: (p) => (p.agent_mem >= 0 ? p.agent_mem : null) }, { name: 'core process RSS', color: '--s2', get: (p) => (p.proc_rss >= 0 ? p.proc_rss : null) }] },
  http: { unit: 'rate', series: [{ name: 'requests/s', color: '--s1', get: (p) => p.requests }, { name: 'errors + drops/s', color: '--s2', get: (p) => p.err_4xx + p.err_5xx + p.dropped }] },
};
const chartEls = {};
$$('.chart').forEach((el) => {
  const key = el.dataset.chart;
  chartEls[key] = { el, canvas: $('canvas', el), tip: $('.tip', el), legend: $('.legend', el), hoverX: null };
  const c = chartEls[key];
  c.canvas.addEventListener('mousemove', (e) => { c.hoverX = e.offsetX; drawChart(key); });
  c.canvas.addEventListener('mouseleave', () => { c.hoverX = null; c.tip.style.display = 'none'; drawChart(key); });
});

function visiblePoints() {
  if (!state.window) return state.points;
  const cutoff = Math.floor(Date.now() / 1000) - state.window;
  return state.points.filter((p) => p.t >= cutoff);
}

function fmtUnit(unit, v) {
  if (v == null) return '–';
  switch (unit) {
    case 'bytes': return fmt.bytes(v);
    case 'secs': return fmt.secs(v);
    case 'pct': return v.toFixed(0) + '%';
    default: return fmt.compact(v);
  }
}

function niceTicks(max, n = 4) {
  if (max <= 0) return [0, 1];
  const raw = max / n;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  const step = (norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 2.5 ? 2.5 : norm <= 5 ? 5 : 10) * mag;
  const ticks = [];
  for (let v = 0; v <= max + step * 0.001; v += step) ticks.push(v);
  if (ticks[ticks.length - 1] < max) ticks.push(ticks[ticks.length - 1] + step);
  return ticks;
}

function renderCharts() {
  for (const key of Object.keys(charts)) {
    const c = chartEls[key];
    const cfg = charts[key];
    c.legend.innerHTML = cfg.series.map((s) => `<span><span class="key" style="background:${css(s.color)}"></span>${esc(s.name)}</span>`).join('');
    drawChart(key);
  }
}

function drawChart(key) {
  const c = chartEls[key], cfg = charts[key], pts = visiblePoints();
  const canvas = c.canvas, dpr = window.devicePixelRatio || 1;
  const W = canvas.clientWidth, H = canvas.clientHeight;
  if (canvas.width !== W * dpr || canvas.height !== H * dpr) { canvas.width = W * dpr; canvas.height = H * dpr; }
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const m = { l: 52, r: 12, t: 8, b: 22 };
  const pw = W - m.l - m.r, ph = H - m.t - m.b;
  ctx.font = '11px -apple-system, "Segoe UI", Inter, Roboto, sans-serif';
  ctx.fillStyle = css('--text-3');
  if (pts.length < 2) { ctx.fillText('waiting for data…', m.l, m.t + 14); c.tip.style.display = 'none'; return; }

  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  let max = 0;
  for (const p of pts) for (const s of cfg.series) { const v = s.get(p); if (v != null && v > max) max = v; }
  const ticks = niceTicks(max);
  const yMax = ticks[ticks.length - 1];
  const x = (t) => m.l + ((t - t0) / Math.max(t1 - t0, 1)) * pw;
  const y = (v) => m.t + ph - (v / yMax) * ph;

  // grid + y labels
  ctx.strokeStyle = css('--line'); ctx.lineWidth = 1;
  ctx.fillStyle = css('--text-3');
  ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
  for (const tv of ticks) {
    const yy = Math.round(y(tv)) + 0.5;
    ctx.beginPath(); ctx.moveTo(m.l, yy); ctx.lineTo(W - m.r, yy); ctx.stroke();
    ctx.fillText(fmtUnit(cfg.unit, tv), m.l - 6, yy);
  }
  // x labels
  ctx.textAlign = 'center'; ctx.textBaseline = 'top';
  const nx = Math.max(2, Math.floor(pw / 90));
  for (let i = 0; i <= nx; i++) {
    const t = t0 + ((t1 - t0) * i) / nx;
    ctx.fillText(fmt.time(t), x(t), m.t + ph + 6);
  }
  // series
  cfg.series.forEach((s, si) => {
    const color = css(s.color);
    ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.lineJoin = 'round'; ctx.lineCap = 'round';
    ctx.beginPath();
    let pen = false;
    for (const p of pts) {
      const v = s.get(p);
      if (v == null) { pen = false; continue; }
      const px = x(p.t), py = y(v);
      if (!pen) { ctx.moveTo(px, py); pen = true; } else ctx.lineTo(px, py);
    }
    ctx.stroke();
    if (si === 0 && cfg.series.length === 1) {
      ctx.globalAlpha = 0.1; ctx.fillStyle = color; ctx.lineTo(x(t1), y(0)); ctx.lineTo(x(t0), y(0)); ctx.closePath(); ctx.fill(); ctx.globalAlpha = 1;
    }
  });
  // hover
  if (c.hoverX != null && c.hoverX >= m.l && c.hoverX <= W - m.r) {
    const ht = t0 + ((c.hoverX - m.l) / pw) * (t1 - t0);
    let best = pts[0];
    for (const p of pts) if (Math.abs(p.t - ht) < Math.abs(best.t - ht)) best = p;
    const hx = Math.round(x(best.t)) + 0.5;
    ctx.strokeStyle = css('--text-3'); ctx.lineWidth = 1; ctx.beginPath(); ctx.moveTo(hx, m.t); ctx.lineTo(hx, m.t + ph); ctx.stroke();
    cfg.series.forEach((s) => {
      const v = s.get(best);
      if (v == null) return;
      ctx.fillStyle = css('--surface-2'); ctx.beginPath(); ctx.arc(x(best.t), y(v), 6, 0, Math.PI * 2); ctx.fill();
      ctx.fillStyle = css(s.color); ctx.beginPath(); ctx.arc(x(best.t), y(v), 4, 0, Math.PI * 2); ctx.fill();
    });
    c.tip.innerHTML = `<div class="t">${fmt.time(best.t)}</div>` + cfg.series.map((s) => `<div class="r"><span class="n"><span class="key" style="display:inline-block;width:10px;height:3px;background:${css(s.color)}"></span>${esc(s.name)}</span><span>${fmtUnit(cfg.unit, s.get(best))}</span></div>`).join('');
    c.tip.style.display = 'block';
    const tw = c.tip.offsetWidth;
    c.tip.style.left = Math.min(c.hoverX + 24, W - tw - 4) + 'px';
    c.tip.style.top = '36px';
  }
}

// ── panels ───────────────────────────────────────────────────────────────────
function kv(rows) {
  return rows.map((r) => (r === null ? '<div class="sep"></div>' : `<div class="k">${esc(r[0])}</div><div class="v ${r[2] || ''}">${r[1]}</div>`)).join('');
}

function renderLedger(r, live) {
  const d = r.delivery, l = r.latency;
  const final = d.all_generators_final;
  $('#ledger-note').textContent = final ? 'all generators finished — counts are final' : 'generators running — "missing" includes logs still in flight';
  const lostCls = d.missing === 0 ? 'good' : final ? 'bad' : 'warn';
  $('#ledger').innerHTML = kv([
    ['records written by generators', fmt.int(d.generated_records)],
    ['physical lines written', fmt.int(d.generated_lines)],
    ['bytes written', fmt.bytes(d.generated_bytes)],
    null,
    ['logs received by intake', fmt.int(d.received_logs)],
    ['· with a marker', fmt.int(d.received_marked)],
    ['· unique markers', fmt.int(d.unique)],
    ['· duplicates', fmt.int(d.duplicates), d.duplicates ? 'warn' : ''],
    ['· out of order', fmt.int(d.out_of_order)],
    ['· orphan continuation lines (multiline split)', fmt.int(d.orphans), d.orphans ? 'warn' : ''],
    ['· unmarked (other sources)', fmt.int(d.unmarked)],
    ['· truncated by the agent', fmt.int(d.truncated), d.truncated ? 'warn' : ''],
    [final ? 'lost' : 'missing (outstanding)', fmt.int(d.missing), lostCls],
    ['delivery ratio', d.generated_records ? fmt.pct(d.ratio, 5) : '–', lostCls],
    null,
    ['multiline logs received / traces written', `${fmt.int(d.multiline)} / ${fmt.int(d.multiline_written)}`, d.multiline_written && d.multiline < d.multiline_written * 0.999 ? 'warn' : ''],
    ['lines inside received logs', fmt.int(d.received_lines)],
    null,
    ['e2e latency p50 / p99 / max (window)', `${fmt.secs(l.end_to_end.p50)} / ${fmt.secs(l.end_to_end.p99)} / ${fmt.secs(l.end_to_end.max)}`],
    ['sender latency p50 / p99 (agent encode → intake)', `${fmt.secs(l.sender.p50)} / ${fmt.secs(l.sender.p99)}`],
    ['marked logs without a parseable timestamp', fmt.int(l.no_timestamp)],
  ]);
}

function faultDesc(f) {
  if (!f) return 'none';
  const p = [];
  if (f.outage) p.push('outage');
  else if (f.drop_rate > 0) p.push(`drop ${(f.drop_rate * 100).toFixed(0)}%`);
  if (f.error_rate > 0) p.push(`HTTP ${f.error_status || 500} ${(f.error_rate * 100).toFixed(0)}%`);
  if (f.latency_ms > 0) p.push(`+${f.latency_ms}ms` + (f.jitter_ms ? `±${f.jitter_ms}` : ''));
  if (f.read_bps > 0) p.push(`read ${fmt.bytes(f.read_bps)}/s`);
  return p.join(', ') + (f.note ? ` (${f.note})` : '');
}

let faultsDirty = false;
function renderFaults(r, live) {
  const f = live.faults || {};
  $('#faults-state').textContent = live.faults_active ? 'ACTIVE since ' + new Date(live.faults_since).toLocaleTimeString() + ' — ' + faultDesc(f) : 'none active';
  if (!faultsDirty) fillFaultForm(f);
  const ev = (r.faults || []).slice(-8).reverse();
  $('#fault-events').innerHTML = ev.map((e) => `<div>${new Date(e.at).toLocaleTimeString()}  ${esc(e.desc)}</div>`).join('');
}
function fillFaultForm(f) {
  const form = $('#faults');
  form.latency_ms.value = f.latency_ms || '';
  form.jitter_ms.value = f.jitter_ms || '';
  form.error_rate.value = f.error_rate ? Math.round(f.error_rate * 100) : '';
  form.error_status.value = f.error_status || '';
  form.drop_rate.value = f.drop_rate ? Math.round(f.drop_rate * 100) : '';
  form.read_bps.value = f.read_bps || '';
  form.outage.checked = !!f.outage;
  form.note.value = f.note || '';
}
function readFaultForm() {
  const form = $('#faults');
  return {
    latency_ms: +form.latency_ms.value || 0, jitter_ms: +form.jitter_ms.value || 0,
    error_rate: (+form.error_rate.value || 0) / 100, error_status: +form.error_status.value || 0,
    drop_rate: (+form.drop_rate.value || 0) / 100, read_bps: +form.read_bps.value || 0,
    outage: form.outage.checked, note: form.note.value,
  };
}
$('#faults').addEventListener('input', () => { faultsDirty = true; });
$('#faults').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await getJSON('/harness/faults', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(readFaultForm()) });
    faultsDirty = false; poll();
  } catch (err) { alert('faults: ' + err.message); }
});
$('#faults-clear').addEventListener('click', async () => {
  await fetch('/harness/faults', { method: 'DELETE' }); faultsDirty = false; poll();
});
$$('.presets button').forEach((b) => b.addEventListener('click', () => {
  const p = JSON.parse(b.dataset.preset);
  fillFaultForm(Object.assign({}, readFaultForm(), p)); faultsDirty = true;
}));

function renderStreams(r) {
  const q = state.streamFilter.toLowerCase();
  let rows = r.streams || [];
  if (q) rows = rows.filter((s) => (s.gen + '/' + s.stream).toLowerCase().includes(q));
  const total = rows.length;
  rows = rows.slice().sort((a, b) => b.missing - a.missing || b.duplicates - a.duplicates || (a.gen + a.stream).localeCompare(b.gen + b.stream)).slice(0, 300);
  $('#streams-note').textContent = `${total} streams` + (total > 300 ? ' (showing 300)' : '') + ' · sorted by missing';
  const now = Date.now();
  $('#streams tbody').innerHTML = rows.map((s) => {
    const bad = s.missing > 0 && !r.delivery.all_generators_final ? '' : s.missing > 0 || s.duplicates > 0 ? 'bad' : '';
    const seen = s.last_at && !s.last_at.startsWith('0001') ? fmt.ago((now - Date.parse(s.last_at)) / 1000) : 'never';
    return `<tr class="${bad}"><td class="name">${esc(s.gen)}</td><td class="name">${esc(s.stream)}${s.active === false ? ' <span class="muted small">stopped</span>' : ''}</td><td class="num">${fmt.int(s.generated)}</td><td class="num">${fmt.int(s.received)}</td><td class="num">${fmt.int(s.unique)}</td><td class="num">${fmt.int(s.missing)}</td><td class="num">${fmt.int(s.duplicates)}</td><td class="num">${fmt.int(s.out_of_order)}</td><td class="num">${fmt.int(s.multiline)}</td><td class="num">${fmt.bytes(s.bytes)}</td><td>${seen}</td></tr>`;
  }).join('') || '<tr><td colspan="11" class="muted">no marked logs received yet</td></tr>';
}
$('#stream-filter').addEventListener('input', (e) => { state.streamFilter = e.target.value; if (state.snap) renderStreams(state.snap.report); });

function renderGens(live, r) {
  const byName = {};
  for (const g of r.generators || []) byName[g.name] = g;
  $('#gens tbody').innerHTML = (live.generators || []).map((g) => {
    const rep = byName[g.name] || {};
    return `<tr><td>${esc(g.name)}</td><td>${esc(g.mode)}</td><td>${esc(g.phase)}</td><td class="num">${g.active_streams}</td><td class="num">${g.target_rate ? fmt.compact(g.target_rate) + '/s' : 'flat out'}</td><td class="num">${fmt.compact(g.record_rate)}/s</td><td class="num">${fmt.int(rep.records)}</td><td class="num">${fmt.int(rep.rotations)}</td><td class="num">${(g.cpu_percent || 0).toFixed(0)}%</td><td>${g.final ? 'finished' : fmt.ago(g.ago_seconds)}</td></tr>`;
  }).join('') || '<tr><td colspan="10" class="muted">no generator has reported — start one with --intake pointing here</td></tr>';
}

function renderHTTP(r) {
  const h = r.http, t = r.throughput;
  const statuses = Object.entries(h.by_status || {}).sort().map(([k, v]) => `${k}: ${fmt.int(v)}`).join(' · ') || '–';
  const enc = Object.entries(r.agent.encodings || {}).map(([k, v]) => `${k} ${fmt.int(v)}`).join(' · ') || '–';
  const other = Object.entries(h.other_requests || {}).sort((a, b) => b[1] - a[1]).slice(0, 6).map(([k, v]) => `${k} ${fmt.int(v)}`).join(' · ') || 'none';
  const rej = Object.entries(h.rejected || {}).map(([k, v]) => `${k}: ${fmt.int(v)}`).join(' · ');
  $('#http').innerHTML = kv([
    ['logs requests', `${fmt.int(h.requests)} (${t.requests_per_sec.toFixed(1)}/s)`],
    ['responses', statuses],
    ['content-encoding', enc],
    ['logs per payload p50 / p99 / max', `${fmt.int(h.logs_per_payload.p50)} / ${fmt.int(h.logs_per_payload.p99)} / ${fmt.int(h.logs_per_payload.max)}`],
    ['payload on wire p50 / p99 / max', `${fmt.bytes(h.payload_wire_bytes.p50)} / ${fmt.bytes(h.payload_wire_bytes.p99)} / ${fmt.bytes(h.payload_wire_bytes.max)}`],
    ['payload decompressed p50 / p99 / max', `${fmt.bytes(h.payload_raw_bytes.p50)} / ${fmt.bytes(h.payload_raw_bytes.p99)} / ${fmt.bytes(h.payload_raw_bytes.max)}`],
    ['total wire / decompressed', `${fmt.bytes(h.wire_bytes)} / ${fmt.bytes(h.raw_bytes)} (${t.compression_ratio ? t.compression_ratio.toFixed(2) + '×' : '–'})`],
    ['intake processing p99', fmt.secs(r.latency.processing.p99)],
    null,
    ['connectivity probes', fmt.int(h.probes)],
    ['malformed payloads', fmt.int(h.malformed), h.malformed ? 'warn' : ''],
    ['rejected', rej || 'none', rej ? 'warn' : ''],
    ['injected: dropped / errored / delayed / slowed', `${fmt.int(h.fault_dropped)} / ${fmt.int(h.fault_errored)} / ${fmt.int(h.fault_delayed)} / ${fmt.int(h.fault_slowed)}`],
    ['tcp frames', fmt.int(h.tcp_frames)],
    null,
    ['other agent traffic (sink)', other],
    ['user-agent', Object.keys(r.agent.user_agent || {}).join(', ') || '–'],
    ['api keys seen', Object.keys(r.agent.api_keys || {}).join(', ') || '–'],
  ]);
}

function renderTags(r) {
  const t = r.tags;
  $('#tags-note').textContent = `${t.avg_tags_per_log.toFixed(1)} tags · ${t.avg_tag_bytes_per_log.toFixed(0)} B per log · ${t.unique_keys} keys`;
  const total = r.delivery.received_logs || 1;
  $('#tags tbody').innerHTML = (t.keys || []).slice(0, 40).map((k) => `<tr><td class="mono">${esc(k.name)}</td><td class="num">${fmt.int(k.count)}</td><td class="num"><span class="bar" style="width:${Math.min(60, (k.count / total) * 60).toFixed(0)}px"></span>${fmt.pct(k.count / total, 0)}</td></tr>`).join('') || '<tr><td colspan="3" class="muted">no tags yet</td></tr>';
}

function renderBreakdown(r) {
  const b = r.breakdown;
  $('#services tbody').innerHTML = (b.services || []).slice(0, 20).map((s) => `<tr><td>${esc(s.name)}</td><td class="num">${fmt.int(s.count)}</td><td class="num">${fmt.bytes(s.bytes)}</td></tr>`).join('') || '<tr><td colspan="3" class="muted">–</td></tr>';
  const rows = [];
  for (const [label, list] of [['source', b.sources], ['host', b.hosts], ['status', b.statuses]]) {
    for (const s of (list || []).slice(0, 8)) rows.push(`<tr><td><span class="muted small">${label}</span> ${esc(s.name)}</td><td class="num">${fmt.int(s.count)}</td></tr>`);
  }
  $('#sources tbody').innerHTML = rows.join('') || '<tr><td colspan="2" class="muted">–</td></tr>';
}

function renderTelemetry(r, live) {
  const o = live.observer || {};
  const q = state.telemetryFilter.toLowerCase();
  const all = r.telemetry || [];
  const rows = all.filter((m) => !q || (m.name + m.labels).toLowerCase().includes(q)).slice(0, 400);
  $('#telemetry-note').textContent = o.telemetry_url ? (o.telemetry_ok ? `${all.length} series · scraped ${o.scrapes}× from ${o.telemetry_url}` : `unreachable: ${o.telemetry_error}`) : 'not configured (--agent-telemetry http://…:5000/telemetry, needs DD_TELEMETRY_ENABLED=true on the agent)';
  $('#telemetry tbody').innerHTML = rows.map((m) => `<tr><td class="mono">${esc(m.name)}</td><td class="mono muted">${esc(m.labels)}</td><td class="muted">${esc(m.type)}</td><td class="num">${fmt.compact(m.last)}</td><td class="num">${m.type === 'counter' ? fmt.compact(m.delta) : ''}</td><td class="num">${m.type === 'counter' ? fmt.compact(m.rate) : ''}</td></tr>`).join('') || `<tr><td colspan="6" class="muted">${all.length ? 'no match' : 'no telemetry'}</td></tr>`;
}
$('#telemetry-filter').addEventListener('input', (e) => { state.telemetryFilter = e.target.value; if (state.snap) renderTelemetry(state.snap.report, state.snap.live); });

let samplesPaused = false;
function renderSamples(live) {
  if (samplesPaused) return;
  $('#samples').innerHTML = (live.samples || []).slice(0, 20).map((s) => `<div class="sample"><div class="meta"><span>${new Date(s.at).toLocaleTimeString()}</span><span>service <b>${esc(s.service)}</b></span><span>source <b>${esc(s.source)}</b></span><span>host <b>${esc(s.host)}</b></span><span>status <b>${esc(s.status)}</b></span><span>${esc(s.encoding || 'identity')} · ${s.payload_logs} logs · ${fmt.bytes(s.payload_wire_bytes)} wire</span>${s.e2e_seconds >= 0 ? `<span>e2e <b>${fmt.secs(s.e2e_seconds)}</b></span>` : ''}${s.marked ? '' : '<span class="status-warn">unmarked</span>'}</div><pre>${esc(s.message)}</pre><div class="tags">${esc(s.tags)}</div></div>`).join('') || '<div class="muted">nothing received yet</div>';
}
$('#samples').addEventListener('mouseenter', () => { samplesPaused = true; });
$('#samples').addEventListener('mouseleave', () => { samplesPaused = false; });

// ── controls ─────────────────────────────────────────────────────────────────
$('#window').addEventListener('change', (e) => {
  state.window = +e.target.value;
  state.points = []; state.lastT = 0;
  poll();
});
$('#pause').addEventListener('click', (e) => {
  state.paused = !state.paused;
  e.target.textContent = state.paused ? 'resume' : 'pause';
  e.target.classList.toggle('active', state.paused);
});
$('#reset').addEventListener('click', async () => {
  if (!confirm('Zero every counter and ledger and start a new measurement window?')) return;
  await fetch('/harness/reset', { method: 'POST' });
  state.points = []; state.lastT = 0;
  poll();
});
window.addEventListener('resize', () => { if (state.snap) renderCharts(); });

poll();
setInterval(poll, 1000);
})();
