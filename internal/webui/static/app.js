// The mini-sprites dashboard. Plain ES modules, no build step. It talks to the
// ordinary /v1 API and to /ui/api (status, metrics history, suspend/wake),
// authenticated by the cookie /ui/login sets plus the X-Mini-Sprites-UI header.

import { timeSeries, lanes, bars, sparkline, fmtTime } from './charts.js';

const $ = (sel, root = document) => root.querySelector(sel);
const POLL_MS = 5000;
const STATE_COLORS = { running: 'var(--s-running)', warm: 'var(--s-warm)', cold: 'var(--s-cold)' };

// ---------- formatting ----------

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
class Raw { constructor(s) { this.s = s; } toString() { return this.s; } }
const raw = (s) => new Raw(s);
// html`...` escapes every interpolation except raw() and arrays of them.
function html(strings, ...vals) {
  let out = strings[0];
  vals.forEach((v, i) => {
    if (Array.isArray(v)) out += v.map((x) => (x instanceof Raw ? x.s : esc(x))).join('');
    else out += v instanceof Raw ? v.s : esc(v);
    out += strings[i + 1];
  });
  return raw(out);
}

function fmtBytes(n, digits = 1) {
  if (!n) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (Math.abs(n) >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(digits)} ${u[i]}`;
}
const fmtCores = (v) => (v === 0 ? '0' : v >= 10 ? v.toFixed(0) : v >= 1 ? v.toFixed(1) : v >= 0.1 ? v.toFixed(2) : v.toFixed(3));
const fmtPct = (v) => `${Math.round(v * 100)}%`;
function ago(t) {
  if (!t) return '—';
  const s = (Date.now() - new Date(t).getTime()) / 1000;
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}
function dur(ms) {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}
const stateBadge = (st, busy) => html`<span class="state ${st}${busy ? ' busy' : ''}">${st || 'unknown'}</span>`;

// ---------- API ----------

class ApiError extends Error { constructor(status, msg) { super(msg); this.status = status; } }

async function api(path, opts = {}) {
  const headers = { 'X-Mini-Sprites-UI': '1', ...(opts.headers || {}) };
  let body = opts.body;
  if (body !== undefined && typeof body !== 'string' && !(body instanceof Blob)) {
    body = JSON.stringify(body);
    headers['Content-Type'] = 'application/json';
  }
  const res = await fetch(path, { method: opts.method || 'GET', headers, body, credentials: 'same-origin' });
  if (res.status === 401) { showLogin(); throw new ApiError(401, 'signed out'); }
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try { const j = await res.json(); msg = j.error?.message || j.message || j.error || msg; } catch {}
    throw new ApiError(res.status, typeof msg === 'string' ? msg : JSON.stringify(msg));
  }
  if (opts.raw) return res;
  if (res.status === 204) return null;
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('ndjson') || opts.ndjson) {
    const lines = (await res.text()).split('\n').filter(Boolean).map((l) => { try { return JSON.parse(l); } catch { return { type: 'info', data: l }; } });
    const err = lines.find((l) => l.type === 'error');
    if (err) throw new ApiError(500, err.error || err.data || 'failed');
    return lines;
  }
  return ct.includes('json') ? res.json() : res.text();
}
const sp = (name) => `/v1/sprites/${encodeURIComponent(name)}`;

// ---------- toasts & dialogs ----------

function toast(msg, bad = false) {
  const t = document.createElement('div');
  t.className = 'toast' + (bad ? ' bad' : '');
  t.textContent = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), bad ? 7000 : 3500);
}
const fail = (e) => { if (e?.status !== 401) toast(e?.message || String(e), true); };

// modal(contentHTML) resolves with the FormData on submit, or null on cancel.
function modal(content, { submit = 'OK', danger = false } = {}) {
  const dlg = $('#modal'), form = $('#modal-form');
  form.innerHTML = String(content) + `<p class="err" id="modal-err" hidden></p><div class="row end"><button type="button" value="cancel" id="modal-cancel">Cancel</button><button class="${danger ? 'primary danger-fill' : 'primary'}" type="submit" value="ok">${esc(submit)}</button></div>`;
  if (danger) form.querySelector('.danger-fill').style.background = 'var(--critical)';
  dlg.showModal();
  form.querySelector('input,select,textarea')?.focus();
  return new Promise((resolve) => {
    const done = (v) => { dlg.close(); form.onsubmit = null; resolve(v); };
    $('#modal-cancel').onclick = () => done(null);
    dlg.oncancel = () => done(null);
    form.onsubmit = (ev) => { ev.preventDefault(); done(new FormData(form)); };
  });
}

// ---------- login ----------

let loginOpen = false;
function showLogin() {
  if (loginOpen) return;
  loginOpen = true;
  $('#logout').hidden = true;
  const dlg = $('#login');
  $('#login-err').hidden = true;
  dlg.showModal();
  dlg.oncancel = (e) => e.preventDefault();
  $('#login-form').onsubmit = async (ev) => {
    ev.preventDefault();
    const res = await fetch('/ui/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: $('#login-token').value }) });
    if (!res.ok) { $('#login-err').textContent = 'That is not this host\'s API token.'; $('#login-err').hidden = false; return; }
    $('#login-token').value = '';
    dlg.close();
    loginOpen = false;
    $('#logout').hidden = false;
    refresh(true);
  };
}
$('#logout').onclick = async () => { await fetch('/ui/logout', { method: 'POST' }); location.reload(); };

// ---------- theme ----------

function applyTheme(t) { if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme; }
try { applyTheme(localStorage.getItem('ms-theme')); } catch {}
$('#theme').onclick = () => {
  const dark = document.documentElement.dataset.theme ? document.documentElement.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  const next = dark ? 'light' : 'dark';
  applyTheme(next);
  try { localStorage.setItem('ms-theme', next); } catch {}
  view?.update?.();
};

// ---------- data: status + metrics history ----------

const data = { status: null, metrics: { points: [], events: [], disk: {}, host_cores: 1, interval_seconds: 5 }, sprites: new Map(), loaded: false };

async function loadMetrics() {
  const pts = data.metrics.points;
  const since = pts.length ? pts[pts.length - 1].t : '';
  const m = await api('/ui/api/metrics' + (since ? `?since=${encodeURIComponent(since)}` : ''));
  if (!since) { data.metrics = m; }
  else {
    data.metrics.points = pts.concat(m.points);
    data.metrics.events = data.metrics.events.concat(m.events).slice(-200);
    data.metrics.disk = m.disk; data.metrics.disk_at = m.disk_at; data.metrics.host_cores = m.host_cores;
    const cut = Date.now() - 3600e3;
    while (data.metrics.points.length && new Date(data.metrics.points[0].t).getTime() < cut) data.metrics.points.shift();
  }
  data.times = data.metrics.points.map((p) => new Date(p.t).getTime());
}

async function loadAll() {
  const [status, list] = await Promise.all([api('/ui/api/status'), listSprites(), loadMetrics()]);
  data.status = status;
  data.sprites = new Map(list.map((s) => [s.name, s]));
  data.loaded = true;
}
async function listSprites() {
  let out = [], token = '';
  for (;;) {
    const r = await api('/v1/sprites?max_results=50' + (token ? `&continuation_token=${encodeURIComponent(token)}` : ''));
    out = out.concat(r.sprites);
    if (!r.has_more) return out;
    token = r.next_continuation_token;
  }
}

// Joined per-sprite view: API record + operator status + disk figures.
function spriteRows() {
  const st = new Map((data.status?.sprites || []).map((s) => [s.name, s]));
  return [...data.sprites.values()].map((a) => {
    const s = st.get(a.name) || {};
    const d = data.metrics.disk?.[a.name] || {};
    return { ...a, ...s, api: a, state: s.state || a.status, disk_used: s.disk_used_bytes ?? d.disk_used_bytes ?? 0,
      disk_exclusive: s.disk_exclusive_bytes ?? d.disk_exclusive_bytes ?? 0, disk_apparent: s.disk_apparent_bytes ?? d.disk_apparent_bytes ?? 0 };
  });
}
const series = (fn) => data.metrics.points.map(fn);
const spriteSeries = (name, key) => series((p) => p.sprites?.[name]?.[key] ?? 0);
const lastCPU = (name) => { const p = data.metrics.points.at(-1); return p?.sprites?.[name]?.cpu_cores ?? 0; };
const recent = (arr, n) => arr.slice(Math.max(0, arr.length - n));

// ---------- polling ----------

let view = null, timer = null, lastOK = 0;
async function refresh(force) {
  clearTimeout(timer);
  try {
    await loadAll();
    lastOK = Date.now();
    $('#logout').hidden = false;
    if (force || !view?.mounted) mount(); else view.update?.();
  } catch (e) {
    if (e.status !== 401) console.warn(e);
  }
  $('#live').classList.toggle('stale', Date.now() - lastOK > POLL_MS * 3);
  timer = setTimeout(refresh, document.hidden ? POLL_MS * 6 : POLL_MS);
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });

// ---------- routing ----------

function route() {
  const h = location.hash.replace(/^#\/?/, '');
  const [a, b, c] = h.split('/').map(decodeURIComponent);
  if (a === 's' && b) return { name: 'sprite', sprite: b, tab: c || 'overview' };
  if (a === 'sprites') return { name: 'sprites' };
  if (a === 'host') return { name: 'host' };
  return { name: 'overview' };
}
function mount() {
  view?.unmount?.();
  const r = route();
  document.querySelectorAll('[data-nav]').forEach((a) => a.classList.toggle('on', a.dataset.nav === (r.name === 'sprite' ? 'sprites' : r.name)));
  const root = $('#view');
  root.innerHTML = '';
  if (!data.loaded) { root.innerHTML = '<div class="empty"><span class="spin"></span> Loading…</div>'; view = null; return; }
  view = ({ overview: Overview, sprites: SpritesView, host: HostView, sprite: SpriteView })[r.name](root, r);
  view.mounted = true;
  view.update?.();
}
addEventListener('hashchange', () => { if (data.loaded) mount(); });
let resizeT;
addEventListener('resize', () => { clearTimeout(resizeT); resizeT = setTimeout(() => view?.update?.(), 120); });

// ---------- shared actions ----------

async function act(name, what) {
  const verbs = { wake: 'Waking', suspend: 'Suspending', cool: 'Sending cold' };
  toast(`${verbs[what]} ${name}…`);
  try { await api(`/ui/api/sprites/${encodeURIComponent(name)}/${what}`, { method: 'POST' }); toast(`${name}: done`); }
  catch (e) { fail(e); }
  refresh();
}

async function createSprite(from) {
  const sprites = [...data.sprites.keys()];
  const fd = await modal(html`
    <h2>${from ? 'Clone checkpoint into a new sprite' : 'New sprite'}</h2>
    ${from ? html`<p class="muted">From <code>${from.sprite}</code> checkpoint <code>${from.checkpoint}</code>.</p>` : ''}
    <label class="field">Name<input name="name" required pattern="[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" placeholder="my-sprite" autocomplete="off"></label>
    <label class="field">URL access
      <select name="auth"><option value="sprite">Private (API token required)</option><option value="public">Public</option></select></label>
    <p class="faint" style="font-size:12px">Lowercase letters, digits and dashes.${sprites.length ? '' : ' This will be your first sprite.'}</p>`, { submit: 'Create' });
  if (!fd) return;
  const body = { name: fd.get('name'), url_settings: { auth: fd.get('auth') } };
  if (from) body.from = from;
  try {
    toast(`Creating ${body.name}…`);
    await api('/v1/sprites', { method: 'POST', body });
    toast(`Created ${body.name}`);
    await refresh();
    location.hash = `#/s/${encodeURIComponent(body.name)}`;
  } catch (e) { fail(e); }
}

async function deleteSprite(name) {
  const fd = await modal(html`
    <h2>Delete ${name}?</h2>
    <p class="muted">This destroys the sprite, its disk and every checkpoint. It cannot be undone.</p>
    <label class="field">Type <b>${name}</b> to confirm<input name="confirm" autocomplete="off" required></label>`, { submit: 'Delete sprite', danger: true });
  if (!fd) return;
  if (fd.get('confirm') !== name) { toast('Name did not match; nothing deleted', true); return; }
  try { await api(sp(name), { method: 'DELETE' }); toast(`Deleted ${name}`); location.hash = '#/sprites'; refresh(true); }
  catch (e) { fail(e); }
}

function actionButtons(s, { small = false } = {}) {
  const c = small ? 'small' : '';
  const b = [];
  if (s.state !== 'running') b.push(html`<button class="${c}" data-act="wake" data-name="${s.name}" title="Boot or resume the VM">▶ Wake</button>`);
  if (s.state === 'running') b.push(html`<button class="${c}" data-act="suspend" data-name="${s.name}" title="Snapshot memory and stop the VM (warm)">⏸ Suspend</button>`);
  if (s.state === 'warm' && !small) b.push(html`<button class="${c}" data-act="cool" data-name="${s.name}" title="Drop the memory snapshot; the next wake is a cold boot">❄ Send cold</button>`);
  return b;
}
function wireActions(root) {
  root.addEventListener('click', (ev) => {
    const btn = ev.target.closest('[data-act]');
    if (!btn) return;
    ev.stopPropagation(); ev.preventDefault();
    const { act: a, name } = btn.dataset;
    if (a === 'delete') deleteSprite(name);
    else if (a === 'new') createSprite();
    else act(name, a);
  });
}

// ---------- stat tiles ----------

function stat({ label, value, unit, foot, hero, sparkId }) {
  return html`<div class="card stat${hero ? ' hero' : ''}"><div class="label">${label}</div>
    <div class="value">${value}${unit ? html`<small>${unit}</small>` : ''}</div>
    ${foot ? html`<div class="foot">${foot}</div>` : ''}${sparkId ? html`<div class="spark" id="${sparkId}"></div>` : ''}</div>`;
}
function meter(parts, big) {
  const total = parts.reduce((a, p) => a + p.value, 0) || 1;
  return html`<div class="meter${big ? ' big' : ''}" role="img" aria-label="${parts.map((p) => `${p.label} ${p.value}`).join(', ')}">${parts.filter((p) => p.value > 0).map((p) =>
    raw(`<i style="flex-grow:${p.value / total};background:${p.color}" title="${esc(p.label)}"></i>`))}</div>`;
}
function legend(parts, fmt = String) {
  return html`<div class="legend">${parts.map((p) => html`<span><i style="background:${raw(p.color)}"></i>${p.label} <b>${fmt(p.value)}</b></span>`)}</div>`;
}
function putSpark(id, values, color, max) {
  const box = document.getElementById(id);
  if (box) { box.innerHTML = ''; box.appendChild(sparkline(values, { color, max })); }
}

// ---------- Overview ----------

function Overview(root) {
  root.innerHTML = String(html`
    <div class="page-head"><div><h1>Overview</h1><div class="sub" id="ov-sub"></div></div>
      <div class="actions"><button class="primary" data-act="new">＋ New sprite</button></div></div>
    <div class="grid kpis" id="ov-kpis"></div>
    <div class="grid two">
      <section class="card"><header><h2>Sprite states</h2><span class="note">stacked count · last hour</span></header>
        <div id="ch-states"></div><div id="lg-states"></div></section>
      <section class="card"><header><h2>CPU</h2><span class="note" id="cpu-note">cores busy across all VMs</span></header><div id="ch-cpu"></div></section>
    </div>
    <section class="card" style="margin-bottom:16px"><header><h2>Activity</h2><span class="note">each sprite's state over the last hour · click a lane to open</span></header>
      <div id="ch-lanes"></div><div id="lg-lanes"></div></section>
    <div class="grid two">
      <section class="card"><header><h2>Memory</h2><span class="note">resident memory of all VMs</span></header><div id="ch-mem"></div></section>
      <section class="card"><header><h2>Memory by sprite</h2><span class="note">running VMs, now</span></header><div id="ch-membar"></div></section>
    </div>
    <div class="grid two">
      <section class="card"><header><h2>Disk by sprite</h2><span class="note" id="disk-note"></span></header><div id="ch-disk"></div><div id="lg-disk"></div></section>
      <section class="card"><header><h2>Recent changes</h2><span class="note">state transitions seen by the sampler</span></header><ul class="feed" id="feed"></ul></section>
    </div>`);
  wireActions(root);
  return {
    update() {
      const st = data.status, h = st.host, pts = data.metrics.points, last = pts.at(-1) || {};
      const rows = spriteRows();
      const cores = data.metrics.host_cores;
      $('#ov-sub').textContent = `${rows.length} sprite${rows.length === 1 ? '' : 's'} · daemon up ${st.daemon ? dur(Date.now() - new Date(st.daemon.started_at)) : '—'} · ${h.data_dir}`;

      const stateParts = [
        { label: 'Running', value: h.running, color: STATE_COLORS.running },
        { label: 'Warm', value: h.warm, color: STATE_COLORS.warm },
        { label: 'Cold', value: h.cold, color: STATE_COLORS.cold }];
      const volUsed = h.volume.volume_total_bytes - h.volume.volume_free_bytes;
      $('#ov-kpis').innerHTML = [
        stat({ label: 'Sprites', value: rows.length, hero: true, foot: html`${meter(stateParts, true)}${legend(stateParts)}` }),
        stat({ label: 'CPU in use', value: fmtCores(last.cpu_cores || 0), unit: 'cores', foot: `of ${cores} host cores · load ${(last.load1 ?? 0).toFixed(2)}`, sparkId: 'sp-cpu' }),
        stat({ label: 'VM memory', value: fmtBytes(last.vmm_rss_bytes || 0), foot: `host ${fmtBytes(last.host_mem_used_bytes)} of ${fmtBytes(last.host_mem_total_bytes)} used`, sparkId: 'sp-mem' }),
        stat({ label: 'Sprite volume', value: fmtPct(volUsed / (h.volume.volume_total_bytes || 1)), unit: 'used',
          foot: html`${meter([{ label: 'Used', value: volUsed, color: volUsed / h.volume.volume_total_bytes > 0.9 ? 'var(--critical)' : 'var(--s-running)' }, { label: 'Free', value: h.volume.volume_free_bytes, color: 'transparent' }])}
            <span>${fmtBytes(volUsed)} of ${fmtBytes(h.volume.volume_total_bytes)}</span><span class="pill ${h.reflink ? 'good' : ''}">${h.reflink ? '✓ reflink clones' : 'full copies'}</span>` }),
        stat({ label: 'Network', value: h.networking ? `${h.taps_used}` : 'off', unit: h.networking ? `of ${h.taps_total} taps` : '',
          foot: html`<span class="pill ${h.policy_helper.reachable ? 'good' : 'bad'}">${h.policy_helper.reachable ? '✓ policy helper' : '✕ policy helper'}</span>${h.max_running ? html`<span>max ${h.max_running} running</span>` : ''}` }),
      ].join('');
      putSpark('sp-cpu', recent(series((p) => p.cpu_cores), 120), 'var(--s-running)');
      putSpark('sp-mem', recent(series((p) => p.vmm_rss_bytes), 120), 'var(--s-running)');

      const times = data.times, interval = data.metrics.interval_seconds * 1000;
      timeSeries($('#ch-states'), { times, interval, stacked: true, height: 190, label: 'Sprites by state over time',
        series: [{ label: 'Cold', color: STATE_COLORS.cold, values: series((p) => p.cold) },
          { label: 'Warm', color: STATE_COLORS.warm, values: series((p) => p.warm) },
          { label: 'Running', color: STATE_COLORS.running, values: series((p) => p.running) }],
        format: (v) => String(Math.round(v)) });
      $('#lg-states').innerHTML = String(legend(stateParts));
      timeSeries($('#ch-cpu'), { times, interval, height: 190, floor: 0.1, label: 'CPU cores in use', format: fmtCores,
        series: [{ label: 'CPU cores', color: 'var(--s-running)', values: series((p) => p.cpu_cores) }] });
      timeSeries($('#ch-mem'), { times, interval, height: 190, bytes: true, label: 'VM memory', format: (v) => fmtBytes(v, 0),
        series: [{ label: 'VM memory', color: 'var(--s-running)', values: series((p) => p.vmm_rss_bytes) }] });

      // Lanes: running first, then by name.
      const order = { running: 0, warm: 1, cold: 2 };
      const names = rows.slice().sort((a, b) => (order[a.state] - order[b.state]) || a.name.localeCompare(b.name)).map((r) => r.name);
      lanes($('#ch-lanes'), { names, times, interval, colors: STATE_COLORS,
        state: (name, i) => pts[i].sprites?.[name]?.state, onClick: (n) => { location.hash = `#/s/${encodeURIComponent(n)}`; } });
      $('#lg-lanes').innerHTML = String(legend(stateParts.map((p) => ({ ...p, value: '' })), () => ''));

      const running = rows.filter((r) => r.vmm_rss_bytes > 0).sort((a, b) => b.vmm_rss_bytes - a.vmm_rss_bytes).slice(0, 10);
      bars($('#ch-membar'), { rows: running.map((r) => ({ label: r.name, href: `#/s/${encodeURIComponent(r.name)}`,
        segments: [{ label: 'Resident memory', value: r.vmm_rss_bytes, color: 'var(--s-running)' }] })), format: (v) => fmtBytes(v, 0), empty: 'No sprite is running' });

      const disk = rows.filter((r) => r.disk_used > 0).sort((a, b) => b.disk_used - a.disk_used).slice(0, 10);
      bars($('#ch-disk'), { rows: disk.map((r) => ({ label: r.name, href: `#/s/${encodeURIComponent(r.name)}`,
        segments: [{ label: 'Only this sprite', value: r.disk_exclusive, color: 'var(--s-7)' },
          { label: 'Shared (clones, base image)', value: Math.max(0, r.disk_used - r.disk_exclusive), color: 'var(--s-6)' }] })),
        format: (v) => fmtBytes(v, 0), empty: 'No disk figures yet' });
      $('#lg-disk').innerHTML = String(legend([{ label: 'Only this sprite (freed on delete)', color: 'var(--s-7)', value: '' }, { label: 'Shared blocks', color: 'var(--s-6)', value: '' }], () => ''));
      $('#disk-note').textContent = h.reflink ? 'disk + checkpoints, shared blocks counted once' : 'disk + checkpoints';

      const ev = data.metrics.events.slice(-40).reverse();
      $('#feed').innerHTML = ev.length ? ev.map((e) => String(html`<li><time>${fmtTime(e.t)}</time>
        <a class="mono" href="#/s/${encodeURIComponent(e.name)}">${e.name}</a>
        ${e.from ? stateBadge(e.from) : html`<span class="pill">created</span>`}<span class="arrow">→</span>${e.to ? stateBadge(e.to) : html`<span class="pill bad">deleted</span>`}</li>`)).join('')
        : '<li class="faint">No changes yet. Transitions show up here as sprites wake and sleep.</li>';
    },
  };
}

// ---------- Sprites list ----------

let listFilter = '', listState = '';
function SpritesView(root) {
  root.innerHTML = String(html`
    <div class="page-head"><div><h1>Sprites</h1><div class="sub" id="sl-sub"></div></div>
      <div class="actions"><input id="sl-q" placeholder="Filter by name…" style="width:200px" value="${listFilter}">
      <div class="range" id="sl-state">${['', 'running', 'warm', 'cold'].map((s) => html`<button data-s="${s}" class="${s === listState ? 'on' : ''}">${s || 'All'}</button>`)}</div>
      <button class="primary" data-act="new">＋ New sprite</button></div></div>
    <section class="card"><div class="table-wrap"><table>
      <thead><tr><th>Name</th><th>State</th><th>CPU · 10 min</th><th class="num">Memory</th><th class="num">Disk</th><th class="num">Checkpoints</th><th>Last running</th><th>URL</th><th></th></tr></thead>
      <tbody id="sl-body"></tbody></table></div></section>`);
  wireActions(root);
  $('#sl-q').oninput = (e) => { listFilter = e.target.value; this_.update(); };
  $('#sl-state').onclick = (e) => {
    const b = e.target.closest('button'); if (!b) return;
    listState = b.dataset.s;
    root.querySelectorAll('#sl-state button').forEach((x) => x.classList.toggle('on', x === b));
    this_.update();
  };
  $('#sl-body').addEventListener('click', (e) => {
    if (e.target.closest('a,button')) return;
    const tr = e.target.closest('tr[data-name]');
    if (tr) location.hash = `#/s/${encodeURIComponent(tr.dataset.name)}`;
  });
  const this_ = {
    update() {
      const all = spriteRows().sort((a, b) => a.name.localeCompare(b.name));
      const rows = all.filter((r) => r.name.includes(listFilter.trim().toLowerCase()) && (!listState || r.state === listState));
      $('#sl-sub').textContent = `${all.length} total · showing ${rows.length}`;
      const body = $('#sl-body');
      if (!rows.length) { body.innerHTML = `<tr><td colspan="9" class="empty">${all.length ? 'No sprite matches.' : 'No sprites yet. Create one to get started.'}</td></tr>`; return; }
      body.innerHTML = rows.map((r) => String(html`<tr class="link" data-name="${r.name}">
        <td><a class="name" href="#/s/${encodeURIComponent(r.name)}">${r.name}</a>${r.parent_id ? html` <span class="pill" title="Created from inside another sprite">child</span>` : ''}${r.policy_restricted ? html` <span class="pill" title="Egress goes through a network policy">policy</span>` : ''}</td>
        <td>${stateBadge(r.state, r.busy)}</td>
        <td><div class="row"><div data-spark="${r.name}" style="width:90px;height:22px"></div><span class="faint mono" style="font-size:12px">${r.state === 'running' ? fmtCores(lastCPU(r.name)) : ''}</span></div></td>
        <td class="num">${r.vmm_rss_bytes ? fmtBytes(r.vmm_rss_bytes) : raw('<span class="faint">—</span>')}</td>
        <td class="num" title="${fmtBytes(r.disk_exclusive)} exclusive · ${fmtBytes(r.disk_apparent)} apparent">${fmtBytes(r.disk_used)}</td>
        <td class="num">${r.checkpoints ?? 0}</td>
        <td class="faint">${r.state === 'running' ? 'now' : ago(r.last_running_at)}</td>
        <td><a href="${r.api.url}" target="_blank" rel="noopener" class="faint" style="font-size:12px">${r.api.url_settings?.auth === 'public' ? '🌐 public' : '🔒 private'} ↗</a></td>
        <td class="actions">${actionButtons(r, { small: true })}</td></tr>`)).join('');
      body.querySelectorAll('[data-spark]').forEach((d) => {
        const s = sparkline(recent(spriteSeries(d.dataset.spark, 'cpu_cores'), 120), { color: 'var(--s-running)', max: 0.05, height: 22, width: 90 });
        s.style.width = '90px'; s.style.height = '22px';
        d.appendChild(s);
      });
    },
  };
  return this_;
}

// ---------- Host ----------

function HostView(root) {
  root.innerHTML = String(html`
    <div class="page-head"><div><h1>Host</h1><div class="sub" id="h-sub"></div></div></div>
    <div class="grid kpis" id="h-kpis"></div>
    <div class="grid two">
      <section class="card"><header><h2>Host memory</h2><span class="note">used, including every VM</span></header><div id="ch-hmem"></div></section>
      <section class="card"><header><h2>Load average</h2><span class="note">1-minute</span></header><div id="ch-load"></div></section>
    </div>
    <div class="grid two">
      <section class="card"><header><h2>Sprite volume</h2><span class="note">used space over time</span></header><div id="ch-vol"></div></section>
      <section class="card"><header><h2>Daemon</h2></header><dl class="kv" id="h-kv"></dl></section>
    </div>
    <section class="card" id="h-orphans"></section>`);
  return {
    update() {
      const st = data.status, h = st.host, last = data.metrics.points.at(-1) || {};
      $('#h-sub').textContent = h.data_dir;
      const volUsed = h.volume.volume_total_bytes - h.volume.volume_free_bytes;
      $('#h-kpis').innerHTML = [
        stat({ label: 'Host memory', value: fmtPct((last.host_mem_used_bytes || 0) / (last.host_mem_total_bytes || 1)), unit: 'used', foot: `${fmtBytes(last.host_mem_used_bytes)} of ${fmtBytes(last.host_mem_total_bytes)}` }),
        stat({ label: 'Load', value: (last.load1 ?? 0).toFixed(2), foot: `${data.metrics.host_cores} cores` }),
        stat({ label: 'Volume free', value: fmtBytes(h.volume.volume_free_bytes), foot: `reserve ${fmtBytes(h.disk_reserve_bytes)} kept for creates` }),
        stat({ label: 'Limits', value: h.max_sprites || '∞', unit: 'sprites', foot: `${h.max_running || '∞'} running at once` }),
      ].join('');
      const times = data.times, interval = data.metrics.interval_seconds * 1000;
      timeSeries($('#ch-hmem'), { times, interval, height: 180, bytes: true, max: last.host_mem_total_bytes, format: (v) => fmtBytes(v, 0),
        series: [{ label: 'Used', color: 'var(--s-running)', values: series((p) => p.host_mem_used_bytes) }] });
      timeSeries($('#ch-load'), { times, interval, height: 180, format: (v) => v.toFixed(1),
        series: [{ label: 'Load (1 min)', color: 'var(--s-running)', values: series((p) => p.load1) }] });
      timeSeries($('#ch-vol'), { times, interval, height: 180, bytes: true, max: h.volume.volume_total_bytes, format: (v) => fmtBytes(v, 0),
        series: [{ label: 'Used', color: 'var(--s-running)', values: series((p) => p.volume_used_bytes) }] });
      const d = st.daemon || {};
      $('#h-kv').innerHTML = String(html`
        <dt>PID</dt><dd class="mono">${d.pid}</dd><dt>Started</dt><dd>${d.started_at ? new Date(d.started_at).toLocaleString() : '—'} (${d.started_at ? dur(Date.now() - new Date(d.started_at)) : ''})</dd>
        <dt>API</dt><dd class="mono">${d.listen || location.host}</dd><dt>Data</dt><dd class="mono">${h.data_dir}</dd>
        <dt>Volume</dt><dd>${fmtBytes(volUsed)} of ${fmtBytes(h.volume.volume_total_bytes)}${h.volume.image ? html` · loop image <code>${h.volume.image}</code>` : ''}</dd>
        <dt>Clones</dt><dd>${h.reflink ? 'reflink (instant, copy-on-write)' : 'full sparse copies'}</dd>
        <dt>Networking</dt><dd>${h.networking ? `${h.taps_used} of ${h.taps_total} taps in use` : 'off (--net=false)'}</dd>
        <dt>Policy helper</dt><dd>${h.policy_helper.reachable ? '✓ reachable' : '✕ unavailable'}${h.policy_helper.detail ? html` <span class="faint">· ${h.policy_helper.detail}</span>` : ''}</dd>`);
      const o = st.orphans || [], od = st.other_daemons || [];
      $('#h-orphans').innerHTML = String(html`<header><h2>Stray processes</h2><span class="note">Firecrackers no daemon owns, and other spritesd instances</span></header>
        ${!o.length && !od.length ? html`<p class="faint" style="margin:0">None. Every VM on this host belongs to this daemon.</p>` : ''}
        ${o.length ? html`<div class="table-wrap"><table><thead><tr><th>PID</th><th>Directory</th><th class="num">Memory</th><th>Parent</th></tr></thead><tbody>
          ${o.map((p) => html`<tr><td class="mono">${p.pid}</td><td class="mono">${p.cwd}</td><td class="num">${fmtBytes(p.rss_bytes)}</td><td>${p.parent_name} (${p.parent_pid})</td></tr>`)}</tbody></table></div>` : ''}
        ${od.length ? html`<div class="table-wrap"><table><thead><tr><th>Other daemon PID</th><th>Command</th><th class="num">VMs</th></tr></thead><tbody>
          ${od.map((p) => html`<tr><td class="mono">${p.pid}</td><td class="mono" style="font-size:12px">${p.cmd.join(' ')}</td><td class="num">${p.vms}</td></tr>`)}</tbody></table></div>` : ''}`);
    },
  };
}

// ---------- Sprite detail ----------

const TABS = [['overview', 'Overview'], ['terminal', 'Terminal'], ['files', 'Files'], ['checkpoints', 'Checkpoints'], ['services', 'Services'], ['policy', 'Policies'], ['raw', 'JSON']];

function SpriteView(root, r) {
  const name = r.sprite;
  if (!data.sprites.has(name)) {
    root.innerHTML = String(html`<div class="empty"><h2>No sprite named ${name}</h2><p><a href="#/sprites">Back to sprites</a></p></div>`);
    return {};
  }
  const base = `#/s/${encodeURIComponent(name)}`;
  root.innerHTML = String(html`
    <div class="page-head"><div><div class="faint" style="font-size:12px"><a href="#/sprites">Sprites</a> /</div>
      <div class="row"><h1 class="mono">${name}</h1><span id="sd-state"></span></div><div class="sub" id="sd-sub"></div></div>
      <div class="actions" id="sd-actions"></div></div>
    <nav class="tabs">${TABS.map(([k, l]) => html`<a href="${base}/${k}" class="${k === r.tab ? 'on' : ''}">${l}</a>`)}</nav>
    <div id="sd-body"></div>`);
  wireActions(root);
  const body = $('#sd-body');
  const tab = ({ overview: SpriteOverview, terminal: TerminalTab, files: FilesTab, checkpoints: CheckpointsTab, services: ServicesTab, policy: PolicyTab, raw: RawTab }[r.tab] || SpriteOverview)(body, name);
  return {
    update() {
      const s = spriteRows().find((x) => x.name === name);
      if (!s) { toast(`${name} no longer exists`, true); location.hash = '#/sprites'; return; }
      $('#sd-state').innerHTML = String(stateBadge(s.state, s.busy));
      $('#sd-sub').innerHTML = String(html`<a href="${s.api.url}" target="_blank" rel="noopener">${s.api.url}</a> · ${s.api.url_settings?.auth === 'public' ? 'public' : 'private'} · created ${ago(s.api.created_at)}`);
      $('#sd-actions').innerHTML = [...actionButtons(s), html`<button class="danger" data-act="delete" data-name="${name}">Delete</button>`].join('');
      tab.update?.(s);
    },
    unmount() { tab.unmount?.(); },
  };
}

function SpriteOverview(root, name) {
  root.innerHTML = `<div class="grid kpis" id="so-kpis"></div>
    <div class="grid two">
      <section class="card"><header><h2>CPU</h2><span class="note">cores · last hour</span></header><div id="so-cpu"></div></section>
      <section class="card"><header><h2>Memory</h2><span class="note">VM resident memory</span></header><div id="so-mem"></div></section>
    </div>
    <section class="card" style="margin-bottom:16px"><header><h2>State</h2><span class="note">last hour</span></header><div id="so-lane"></div></section>
    <div class="grid two">
      <section class="card"><header><h2>Details</h2></header><dl class="kv" id="so-kv"></dl></section>
      <section class="card"><header><h2>Environment &amp; labels</h2></header><div id="so-env"></div></section>
    </div>`;
  return {
    update(s) {
      const pts = data.metrics.points, times = data.times, interval = data.metrics.interval_seconds * 1000;
      const cpu = spriteSeries(name, 'cpu_cores'), mem = spriteSeries(name, 'rss_bytes');
      const cfg = s.api.config || {};
      $('#so-kpis').innerHTML = [
        stat({ label: 'CPU now', value: s.state === 'running' ? fmtCores(lastCPU(name)) : '—', unit: s.state === 'running' ? 'cores' : '', foot: `${cfg.cpus || 'default'} vCPUs`, sparkId: 'so-sp-cpu' }),
        stat({ label: 'Memory', value: s.vmm_rss_bytes ? fmtBytes(s.vmm_rss_bytes) : '—', foot: `${cfg.ram_mb ? cfg.ram_mb + ' MiB' : 'default'} guest RAM${s.snapshot_bytes ? ` · ${fmtBytes(s.snapshot_bytes)} snapshot` : ''}`, sparkId: 'so-sp-mem' }),
        stat({ label: 'Disk', value: fmtBytes(s.disk_used), foot: `${fmtBytes(s.disk_exclusive)} exclusive · ${fmtBytes(s.disk_apparent)} apparent` }),
        stat({ label: 'Checkpoints', value: s.checkpoints ?? 0, foot: s.mounted_checkpoints ? `${Object.keys(s.mounted_checkpoints).length} mounted` : 'none mounted' }),
      ].join('');
      putSpark('so-sp-cpu', recent(cpu, 120), 'var(--s-running)', 0.05);
      putSpark('so-sp-mem', recent(mem, 120), 'var(--s-running)');
      timeSeries($('#so-cpu'), { times, interval, height: 170, floor: 0.1, format: fmtCores, series: [{ label: 'CPU cores', color: 'var(--s-running)', values: cpu }] });
      timeSeries($('#so-mem'), { times, interval, height: 170, bytes: true, format: (v) => fmtBytes(v, 0), series: [{ label: 'Memory', color: 'var(--s-running)', values: mem }] });
      lanes($('#so-lane'), { names: [name], times, interval, colors: STATE_COLORS, state: (n, i) => pts[i].sprites?.[n]?.state });
      $('#so-kv').innerHTML = String(html`
        <dt>ID</dt><dd class="mono">${s.api.id}</dd>
        <dt>Created</dt><dd>${new Date(s.api.created_at).toLocaleString()}</dd>
        <dt>Last running</dt><dd>${s.state === 'running' ? 'now' : ago(s.api.last_running_at)}</dd>
        <dt>IP</dt><dd class="mono">${s.ip || '—'}${s.tap ? html` <span class="faint">on ${s.tap}</span>` : ''}</dd>
        <dt>Network policy</dt><dd>${s.policy_restricted ? 'restricted egress' : 'open egress'}</dd>
        <dt>Task holds</dt><dd>${s.task_holds ?? '—'}</dd>
        <dt>API calls in flight</dt><dd>${s.api_inflight ?? 0}</dd>
        ${s.api.parent_id ? html`<dt>Parent</dt><dd class="mono">${s.api.parent_id}</dd>` : ''}
        ${s.vmm_pid ? html`<dt>VMM PID</dt><dd class="mono">${s.vmm_pid}</dd>` : ''}`);
      const env = Object.entries(s.api.environment || {});
      $('#so-env').innerHTML = String(html`${env.length ? html`<dl class="kv">${env.map(([k, v]) => html`<dt class="mono">${k}</dt><dd class="mono">${v}</dd>`)}</dl>` : html`<p class="faint" style="margin:0">No environment variables.</p>`}
        ${(s.api.labels || []).length ? html`<div class="row wrap" style="margin-top:12px">${s.api.labels.map((l) => html`<span class="pill">${l}</span>`)}</div>` : ''}`);
    },
  };
}

// Tabs that talk to the guest wake it. Say so before doing it.
function needsWake(root, s, label, go) {
  if (s.state === 'running') return false;
  root.innerHTML = String(html`<div class="card empty"><p>This sprite is <b>${s.state}</b>. ${label} talks to the guest, which wakes it.</p>
    <button class="primary" id="nw-go">Wake and continue</button></div>`);
  $('#nw-go').onclick = go;
  return true;
}

function TerminalTab(root, name) {
  root.innerHTML = `<div class="term-bar"><button class="primary" id="t-connect">Connect</button><button id="t-disconnect" disabled>Disconnect</button>
    <span class="status" id="t-status">Opens a login shell inside the sprite (wakes it if needed).</span></div>
    <div class="term-wrap"><div class="term" id="t-term"></div></div>`;
  const term = new window.Terminal({ fontFamily: 'ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace', fontSize: 13, cursorBlink: true,
    theme: { background: '#0f0f0e', foreground: '#e8e6df', cursor: '#3987e5', selectionBackground: 'rgba(57,135,229,0.35)' }, scrollback: 5000 });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($('#t-term'));
  fit.fit();
  const ro = new ResizeObserver(() => { try { fit.fit(); } catch {} });
  ro.observe($('#t-term'));
  let ws = null;
  const status = (t) => { $('#t-status').textContent = t; };
  const enc = new TextEncoder();
  term.onData((d) => { if (ws?.readyState === 1) ws.send(enc.encode(d)); });
  term.onResize(({ cols, rows }) => { if (ws?.readyState === 1) ws.send(JSON.stringify({ type: 'resize', cols, rows })); });
  const connect = () => {
    if (ws) return;
    fit.fit();
    const q = new URLSearchParams();
    q.append('cmd', 'bash'); q.append('cmd', '-l');
    q.set('tty', 'true'); q.set('stdin', 'true'); q.set('cols', term.cols); q.set('rows', term.rows);
    q.append('env', 'TERM=xterm-256color'); q.append('env', 'COLORTERM=truecolor');
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${proto}//${location.host}${sp(name)}/exec?${q}`);
    ws.binaryType = 'arraybuffer';
    status('Connecting (waking the sprite if it is asleep)…');
    $('#t-connect').disabled = true; $('#t-disconnect').disabled = false;
    ws.onopen = () => { status('Connected'); term.focus(); };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        try {
          const m = JSON.parse(ev.data);
          if (m.type === 'exit') term.write(`\r\n\x1b[2m[process exited with code ${m.exit_code}]\x1b[0m\r\n`);
          if (m.type === 'port_opened') toast(`${name}: port ${m.port} opened`);
        } catch {}
        return;
      }
      term.write(new Uint8Array(ev.data));
    };
    ws.onclose = (ev) => {
      ws = null;
      status(ev.code === 1000 || ev.code === 1005 ? 'Disconnected' : `Disconnected (${ev.code}${ev.reason ? ': ' + ev.reason : ''})`);
      $('#t-connect').disabled = false; $('#t-disconnect').disabled = true;
      refresh();
    };
    ws.onerror = () => status('Connection failed');
  };
  $('#t-connect').onclick = connect;
  $('#t-disconnect').onclick = () => ws?.close(1000);
  connect();
  return { unmount() { ws?.close(1000); ro.disconnect(); term.dispose(); } };
}

function FilesTab(root, name) {
  let path = '/home/sprite', started = false;
  const load = async (p) => {
    path = p;
    root.innerHTML = '<div class="empty"><span class="spin"></span></div>';
    try {
      const r = await api(`${sp(name)}/fs/list?path=${encodeURIComponent(p)}`);
      const parts = r.path.split('/').filter(Boolean);
      const crumbs = [html`<a href="#" data-p="/">/</a>`, ...parts.map((x, i) => html`<a href="#" data-p="/${parts.slice(0, i + 1).join('/')}">${x}</a><span class="faint">/</span>`)];
      const entries = r.entries.slice().sort((a, b) => (b.isDir - a.isDir) || a.name.localeCompare(b.name));
      root.innerHTML = String(html`<section class="card"><header><div class="crumbs">${crumbs}</div><span class="note">${r.count} entries</span></header>
        <div class="table-wrap"><table><thead><tr><th>Name</th><th class="num">Size</th><th>Mode</th><th>Modified</th><th></th></tr></thead><tbody>
        ${r.path !== '/' ? html`<tr class="link" data-p="${r.path.replace(/\/[^/]+\/?$/, '') || '/'}"><td><span class="ico">↰</span> ..</td><td></td><td></td><td></td><td></td></tr>` : ''}
        ${entries.map((e) => html`<tr class="link" data-${e.isDir ? 'p' : 'f'}="${e.path}"><td class="mono"><span class="ico">${e.isDir ? '▸' : e.type === 'symlink' ? '↪' : '·'}</span> ${e.name}${e.isDir ? '/' : ''}</td>
          <td class="num">${e.isDir ? '' : fmtBytes(e.size)}</td><td class="mono faint">${e.mode}</td><td class="faint">${ago(e.modTime)}</td>
          <td class="actions">${e.isDir ? '' : html`<button class="small" data-dl="${e.path}">Download</button>`}</td></tr>`)}
        </tbody></table></div></section><section class="card" id="f-view" hidden style="margin-top:16px"></section>`);
    } catch (e) { root.innerHTML = String(html`<div class="card empty err">${e.message}</div>`); }
  };
  const view = async (p) => {
    const box = $('#f-view');
    box.hidden = false;
    box.innerHTML = String(html`<header><h2 class="mono">${p}</h2></header><div class="empty"><span class="spin"></span></div>`);
    try {
      const res = await api(`${sp(name)}/fs/read?path=${encodeURIComponent(p)}`, { raw: true });
      const buf = new Uint8Array(await res.arrayBuffer());
      const binary = buf.slice(0, 8000).includes(0);
      const text = binary ? '' : new TextDecoder().decode(buf.slice(0, 512 * 1024));
      box.innerHTML = String(html`<header><h2 class="mono">${p}</h2><span class="note">${fmtBytes(buf.length)}</span></header>
        ${binary ? html`<p class="faint">Binary file; use Download.</p>` : html`<pre class="file-view">${text}${buf.length > 512 * 1024 ? '\n… (truncated)' : ''}</pre>`}`);
      box.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    } catch (e) { box.innerHTML = String(html`<p class="err">${e.message}</p>`); }
  };
  const download = async (p) => {
    try {
      const res = await api(`${sp(name)}/fs/read?path=${encodeURIComponent(p)}`, { raw: true });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(await res.blob());
      a.download = p.split('/').pop();
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 5000);
    } catch (e) { fail(e); }
  };
  root.addEventListener('click', (e) => {
    const t = e.target.closest('[data-dl],[data-p],[data-f]');
    if (!t) return;
    e.preventDefault();
    if (t.dataset.dl) download(t.dataset.dl);
    else if (t.dataset.p) load(t.dataset.p);
    else view(t.dataset.f);
  });
  return {
    update(s) {
      if (started) return;
      if (needsWake(root, s, 'Browsing files', () => { started = true; load(path); })) return;
      started = true;
      load(path);
    },
  };
}

function CheckpointsTab(root, name) {
  root.innerHTML = `<section class="card"><header><h2>Checkpoints</h2><span class="note">copy-on-write disk snapshots</span></header>
    <form class="row" id="cp-form" style="margin-bottom:12px"><input name="comment" placeholder="Comment (optional)" class="grow"><button class="primary" type="submit">Create checkpoint</button>
    <label class="row faint" style="font-size:12.5px;gap:6px;white-space:nowrap"><input type="checkbox" id="cp-auto" style="width:auto"> show automatic</label></form>
    <div id="cp-list"></div></section>`;
  const load = async () => {
    try {
      const list = await api(`${sp(name)}/checkpoints?includeAuto=${$('#cp-auto').checked}`);
      const cps = (Array.isArray(list) ? list : list.checkpoints || []).slice().sort((a, b) => new Date(b.create_time) - new Date(a.create_time));
      $('#cp-list').innerHTML = cps.length ? String(html`<div class="table-wrap"><table><thead><tr><th>ID</th><th>Created</th><th>Comment</th><th></th></tr></thead><tbody>
        ${cps.map((c) => html`<tr><td class="mono">${c.id}${c.is_auto ? html` <span class="pill">auto</span>` : ''}</td><td>${new Date(c.create_time).toLocaleString()} <span class="faint">· ${ago(c.create_time)}</span></td><td>${c.comment || ''}</td>
          <td class="actions"><button class="small" data-restore="${c.id}">Restore</button> <button class="small" data-clone="${c.id}">Clone to new sprite</button> <button class="small danger" data-del="${c.id}">Delete</button></td></tr>`)}
        </tbody></table></div>`) : '<p class="faint">No checkpoints yet.</p>';
    } catch (e) { $('#cp-list').innerHTML = String(html`<p class="err">${e.message}</p>`); }
  };
  $('#cp-auto').onchange = load;
  $('#cp-form').onsubmit = async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector('button');
    btn.disabled = true; btn.innerHTML = '<span class="spin"></span> Creating…';
    try {
      const lines = await api(`${sp(name)}/checkpoint`, { method: 'POST', body: { comment: e.target.comment.value }, ndjson: true });
      toast(lines.filter((l) => l.type === 'complete').map((l) => l.data).join(' ') || 'Checkpoint created');
      e.target.comment.value = '';
    } catch (err) { fail(err); }
    btn.disabled = false; btn.textContent = 'Create checkpoint';
    load(); refresh();
  };
  root.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-restore],[data-del],[data-clone]');
    if (!b) return;
    if (b.dataset.clone) return createSprite({ sprite: name, checkpoint: b.dataset.clone });
    if (b.dataset.restore) {
      const ok = await modal(html`<h2>Restore ${b.dataset.restore}?</h2><p class="muted">The sprite's disk is replaced with this checkpoint and the VM restarts. An automatic checkpoint of the current state is taken first.</p>`, { submit: 'Restore', danger: true });
      if (!ok) return;
      toast(`Restoring ${b.dataset.restore}…`);
      try { await api(`${sp(name)}/checkpoints/${encodeURIComponent(b.dataset.restore)}/restore`, { method: 'POST', ndjson: true }); toast('Restored'); } catch (err) { fail(err); }
    } else {
      const ok = await modal(html`<h2>Delete checkpoint ${b.dataset.del}?</h2><p class="muted">This cannot be undone.</p>`, { submit: 'Delete', danger: true });
      if (!ok) return;
      try { await api(`${sp(name)}/checkpoints/${encodeURIComponent(b.dataset.del)}`, { method: 'DELETE' }); toast('Deleted'); } catch (err) { fail(err); }
    }
    load(); refresh();
  });
  load();
  return {};
}

function ServicesTab(root, name) {
  let started = false;
  const load = async () => {
    try {
      const list = await api(`${sp(name)}/services`);
      const svcs = Array.isArray(list) ? list : list.services || [];
      root.innerHTML = String(html`<section class="card"><header><h2>Services</h2><span class="note">long-running processes the guest supervises</span></header>
        ${svcs.length ? html`<div class="table-wrap"><table><thead><tr><th>Name</th><th>Status</th><th>Command</th><th class="num">HTTP port</th><th class="num">Restarts</th><th></th></tr></thead><tbody>
        ${svcs.map((s) => { const st = s.state || {}; return html`<tr><td class="mono">${s.name}</td>
          <td><span class="pill ${st.status === 'running' ? 'good' : st.status === 'failed' ? 'bad' : ''}">${st.status || 'unknown'}</span>${st.error ? html` <span class="err" style="font-size:12px">${st.error}</span>` : ''}</td>
          <td class="mono" style="font-size:12px">${[s.cmd, ...(s.args || [])].join(' ')}</td><td class="num">${s.http_port ?? ''}</td><td class="num">${st.restart_count || 0}</td>
          <td class="actions">${st.status === 'running' ? html`<button class="small" data-svc="restart" data-n="${s.name}">Restart</button> <button class="small" data-svc="stop" data-n="${s.name}">Stop</button>` : html`<button class="small" data-svc="start" data-n="${s.name}">Start</button>`}</td></tr>`; })}
        </tbody></table></div>` : html`<p class="faint" style="margin:0">No services. Create them with <code>sprite-env services</code> inside the sprite, or through the API.</p>`}</section>`);
    } catch (e) { root.innerHTML = String(html`<div class="card empty err">${e.message}</div>`); }
  };
  root.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-svc]');
    if (!b) return;
    b.disabled = true;
    try { await api(`${sp(name)}/services/${encodeURIComponent(b.dataset.n)}/${b.dataset.svc}`, { method: 'POST', raw: true }).then((r) => r.text()); toast(`${b.dataset.n}: ${b.dataset.svc} sent`); }
    catch (err) { fail(err); }
    load();
  });
  return {
    update(s) {
      if (started) return;
      if (needsWake(root, s, 'The services list', () => { started = true; load(); })) return;
      started = true;
      load();
    },
  };
}

function PolicyTab(root, name) {
  const kinds = [
    ['network', 'Network', 'Egress rules. {"rules": []} means open egress.'],
    ['privileges', 'Privileges', 'What the guest user may do: privilege profile and devices.'],
    ['resources', 'Resources', 'Memory limit for the guest.'],
    ['spawn', 'Spawn', 'Whether this sprite may create child sprites from inside.'],
  ];
  root.innerHTML = String(html`<div class="grid two">${kinds.map(([k, t, d]) => html`<section class="card"><header><h2>${t}</h2></header>
    <p class="faint" style="margin:0 0 8px;font-size:12.5px">${d}</p><textarea id="pol-${k}" spellcheck="false"></textarea>
    <div class="row end" style="margin-top:8px">${k !== 'network' ? html`<button class="small danger" data-clear="${k}">Clear</button>` : ''}<button class="small primary" data-save="${k}">Save</button></div></section>`)}</div>`);
  const load = async (k) => {
    try { $(`#pol-${k}`).value = JSON.stringify(await api(`${sp(name)}/policy/${k}`), null, 2); }
    catch (e) { $(`#pol-${k}`).value = `// ${e.message}`; }
  };
  kinds.forEach(([k]) => load(k));
  root.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-save],[data-clear]');
    if (!b) return;
    const k = b.dataset.save || b.dataset.clear;
    try {
      if (b.dataset.clear) await api(`${sp(name)}/policy/${k}`, { method: 'DELETE' });
      else {
        let body;
        try { body = JSON.parse($(`#pol-${k}`).value); } catch { throw new Error('That is not valid JSON'); }
        await api(`${sp(name)}/policy/${k}`, { method: 'POST', body });
      }
      toast(`${k} policy saved`);
      load(k); refresh();
    } catch (err) { fail(err); }
  });
  return {};
}

function RawTab(root, name) {
  root.innerHTML = '<div class="grid two"><section class="card"><header><h2>API record</h2><span class="note">GET /v1/sprites/{name}</span></header><pre class="json" id="raw-api"></pre></section><section class="card"><header><h2>Operator status</h2><span class="note">spritesd status</span></header><pre class="json" id="raw-st"></pre></section></div>';
  return {
    update(s) {
      $('#raw-api').textContent = JSON.stringify(s.api, null, 2);
      $('#raw-st').textContent = JSON.stringify((data.status.sprites || []).find((x) => x.name === name), null, 2);
    },
  };
}

// ---------- boot ----------

refresh(true);
