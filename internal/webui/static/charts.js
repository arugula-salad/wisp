// Small SVG charts with no dependencies: time series (line or stacked area),
// state lanes, horizontal bars and sparklines. Colors are CSS variables so both
// themes work; marks follow fixed specs (2px lines, 4px rounded bar ends, 2px
// surface gaps) and every chart has a hover tooltip.

const NS = 'http://www.w3.org/2000/svg';

export function el(tag, attrs = {}, parent) {
  const e = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) if (v !== undefined && v !== null) e.setAttribute(k, v);
  if (parent) parent.appendChild(e);
  return e;
}

export const fmtTime = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
// axisTime picks a label format that tells ticks apart over the given span.
const axisTime = (span) => (span < 10 * 60e3 ? fmtTimeSec : fmtTime);
export const fmtTimeSec = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });

// niceTicks returns ~n clean tick values from 0 to at least max.
function niceTicks(max, n = 4) {
  if (!(max > 0)) return { ticks: [0, 1], top: 1 };
  const raw = max / n;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  const top = Math.ceil(max / step) * step;
  const ticks = [];
  for (let v = 0; v <= top + step / 2; v += step) ticks.push(+v.toPrecision(12));
  return { ticks, top };
}

// binaryTicks is niceTicks for bytes, stepping in powers of two of a unit.
function byteTicks(max, n = 4) {
  if (!(max > 0)) return niceTicks(max, n);
  const units = [1, 1024, 1024 ** 2, 1024 ** 3, 1024 ** 4];
  const unit = units.filter((u) => u <= max).pop() || 1;
  const { ticks, top } = niceTicks(max / unit, n);
  return { ticks: ticks.map((t) => t * unit), top: top * unit };
}

// swap puts a redrawn chart where the old one was. The old one must stay in
// place until then: measuring a container with its chart removed forces a
// layout with the page suddenly shorter, and browsers without scroll
// anchoring (Safari) clamp the scroll position to it, so every refresh
// yanked the page up.
function swap(container, old, svg) {
  if (old) old.replaceWith(svg);
  else container.prepend(svg);
}

function tipBox(container) {
  let tip = container.querySelector(':scope > .tip');
  if (!tip) {
    tip = document.createElement('div');
    tip.className = 'tip';
    tip.hidden = true;
    container.appendChild(tip);
  }
  return tip;
}

function placeTip(container, tip, x, y) {
  const w = container.clientWidth;
  tip.hidden = false;
  const tw = tip.offsetWidth;
  let left = x + 14;
  if (left + tw > w) left = x - tw - 14;
  tip.style.left = Math.max(0, left) + 'px';
  tip.style.top = Math.max(0, y - 10) + 'px';
}

function tipRows(title, rows) {
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  return `<div class="t">${esc(title)}</div>` + rows.map((r) =>
    `<div class="r"><i class="${r.square ? 'sq' : ''}" style="background:${r.color}"></i>${esc(r.label)}<b>${esc(r.value)}</b></div>`).join('');
}

/**
 * timeSeries draws one chart of values over time.
 * opts: { series: [{label, color, values: number[]}], times: ms[], stacked, height,
 *         format: v => string, bytes: bool, max: number (optional y ceiling), area: bool }
 */
export function timeSeries(container, opts) {
  const { series, times, stacked = false, height = 200, format = String, bytes = false } = opts;
  container.classList.add('chart');
  const old = container.querySelector(':scope > svg');
  const width = Math.max(container.clientWidth, 200);
  const m = { l: 44, r: 12, t: 10, b: 22 };
  const w = width - m.l - m.r, h = height - m.t - m.b;
  const svg = el('svg', { viewBox: `0 0 ${width} ${height}`, height, role: 'img', 'aria-label': opts.label || '' });
  swap(container, old, svg);
  if (times.length === 0) {
    el('text', { x: m.l + w / 2, y: m.t + h / 2, 'text-anchor': 'middle' }, svg).textContent = 'Collecting samples…';
    return;
  }

  // Cumulative tops for stacking.
  const n = times.length;
  const tops = series.map(() => new Array(n).fill(0));
  const bottoms = series.map(() => new Array(n).fill(0));
  for (let i = 0; i < n; i++) {
    let acc = 0;
    series.forEach((s, k) => {
      const v = s.values[i] ?? 0;
      bottoms[k][i] = stacked ? acc : 0;
      acc = stacked ? acc + v : v;
      tops[k][i] = acc;
    });
  }
  const max = opts.max ?? Math.max(opts.floor || 0, ...tops.flat());
  const { ticks, top } = bytes ? byteTicks(max) : niceTicks(max);
  const t0 = times[0], t1 = Math.max(times[n - 1], t0 + 1);
  const x = (t) => m.l + ((t - t0) / (t1 - t0)) * w;
  const y = (v) => m.t + h - (v / top) * h;

  for (const tv of ticks) {
    el('line', { class: tv === 0 ? 'baseline' : 'gridline', x1: m.l, x2: m.l + w, y1: y(tv), y2: y(tv) }, svg);
    el('text', { x: m.l - 8, y: y(tv) + 4, 'text-anchor': 'end' }, svg).textContent = format(tv);
  }
  const span = t1 - t0;
  const xticks = Math.min(6, Math.max(2, Math.floor(w / 110)));
  for (let i = 0; i <= xticks; i++) {
    const t = t0 + (span * i) / xticks;
    el('text', { x: x(t), y: height - 4, 'text-anchor': i === 0 ? 'start' : i === xticks ? 'end' : 'middle' }, svg).textContent = axisTime(span)(t);
  }

  // Gaps in time (the daemon restarted) break the line instead of bridging it.
  const gapMs = (opts.interval || 5000) * 3;
  const path = (k, useBottom) => {
    let d = '';
    for (let i = 0; i < n; i++) {
      const brk = i === 0 || times[i] - times[i - 1] > gapMs;
      d += `${brk ? 'M' : 'L'}${x(times[i]).toFixed(1)},${y(tops[k][i]).toFixed(1)}`;
    }
    return d;
  };
  const areaPath = (k) => {
    let d = '', start = 0;
    const flush = (end) => {
      d += `M${x(times[start]).toFixed(1)},${y(tops[k][start]).toFixed(1)}`;
      for (let i = start + 1; i <= end; i++) d += `L${x(times[i]).toFixed(1)},${y(tops[k][i]).toFixed(1)}`;
      for (let i = end; i >= start; i--) d += `L${x(times[i]).toFixed(1)},${y(bottoms[k][i]).toFixed(1)}`;
      d += 'Z';
    };
    for (let i = 1; i <= n; i++) if (i === n || times[i] - times[i - 1] > gapMs) { flush(i - 1); start = i; }
    return d;
  };
  series.forEach((s, k) => {
    if (stacked) el('path', { d: areaPath(k), fill: s.color, 'fill-opacity': 0.85 }, svg);
    else if (opts.area !== false) el('path', { d: areaPath(k), fill: s.color, 'fill-opacity': 0.1 }, svg);
  });
  if (stacked) {
    // 2px surface gap between stacked bands.
    series.forEach((s, k) => { if (k > 0) el('path', { d: path(k - 1), class: 'line', stroke: 'var(--surface)' }, svg); });
  } else {
    series.forEach((s, k) => el('path', { d: path(k), class: 'line', stroke: s.color }, svg));
    // End dot on the latest value.
    series.forEach((s, k) => el('circle', { class: 'dot', cx: x(times[n - 1]), cy: y(tops[k][n - 1]), r: 4, fill: s.color }, svg));
  }

  // Hover: crosshair + tooltip with every series at the nearest sample.
  const cross = el('line', { class: 'cross', y1: m.t, y2: m.t + h, visibility: 'hidden' }, svg);
  const dots = series.map((s) => el('circle', { class: 'dot', r: 4, fill: s.color, visibility: 'hidden' }, svg));
  const hit = el('rect', { class: 'hit', x: m.l, y: 0, width: w, height }, svg);
  const tip = tipBox(container);
  hit.addEventListener('pointermove', (ev) => {
    const r = svg.getBoundingClientRect();
    const px = (ev.clientX - r.left) * (width / r.width);
    const t = t0 + ((px - m.l) / w) * (t1 - t0);
    let i = 0, best = Infinity;
    for (let j = 0; j < n; j++) { const d = Math.abs(times[j] - t); if (d < best) { best = d; i = j; } }
    const cx = x(times[i]);
    cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.setAttribute('visibility', 'visible');
    dots.forEach((d, k) => { d.setAttribute('cx', cx); d.setAttribute('cy', y(tops[k][i])); d.setAttribute('visibility', 'visible'); });
    const rows = series.map((s) => ({ label: s.label, color: s.color, value: format(s.values[i] ?? 0), square: stacked })).reverse();
    if (stacked && series.length > 1) rows.push({ label: 'Total', color: 'transparent', value: format(tops[series.length - 1][i]) });
    tip.innerHTML = tipRows(fmtTimeSec(times[i]), rows);
    placeTip(container, tip, cx * (r.width / width), ev.clientY - r.top);
  });
  hit.addEventListener('pointerleave', () => {
    tip.hidden = true;
    cross.setAttribute('visibility', 'hidden');
    dots.forEach((d) => d.setAttribute('visibility', 'hidden'));
  });
}

/**
 * lanes draws one row per sprite, colored by its state over time.
 * opts: { names: string[], times: ms[], state: (name, i) => string|undefined, colors: {state: color}, interval }
 */
export function lanes(container, opts) {
  const { names, times, state, colors, onClick } = opts;
  container.classList.add('chart');
  const old = container.querySelector(':scope > svg');
  const width = Math.max(container.clientWidth, 200);
  const labelW = Math.min(160, Math.max(80, width * 0.18));
  const lane = 20, gap = 6;
  const m = { l: labelW, r: 12, t: 4, b: 22 };
  const height = m.t + m.b + Math.max(1, names.length) * (lane + gap);
  const w = width - m.l - m.r;
  const svg = el('svg', { viewBox: `0 0 ${width} ${height}`, height, role: 'img', 'aria-label': 'Sprite state over time' });
  swap(container, old, svg);
  if (!times.length || !names.length) {
    el('text', { x: m.l + w / 2, y: height / 2, 'text-anchor': 'middle' }, svg).textContent = names.length ? 'Collecting samples…' : 'No sprites yet';
    return;
  }
  const n = times.length;
  const t0 = times[0], t1 = Math.max(times[n - 1], t0 + 1);
  const step = (opts.interval || 5000);
  const x = (t) => m.l + ((t - t0) / (t1 - t0 + step)) * w;
  const tip = tipBox(container);

  names.forEach((name, row) => {
    const y0 = m.t + row * (lane + gap);
    const label = el('text', { class: 'lane-label', x: m.l - 10, y: y0 + lane / 2 + 4, 'text-anchor': 'end' }, svg);
    label.textContent = name.length > 20 ? name.slice(0, 19) + '…' : name;
    el('rect', { x: m.l, y: y0, width: w, height: lane, rx: 4, fill: 'var(--surface-2)' }, svg);
    // Runs of one state become one rect, with a 2px surface gap between runs.
    let i = 0;
    while (i < n) {
      const s = state(name, i);
      let j = i + 1;
      while (j < n && state(name, j) === s && times[j] - times[j - 1] <= step * 3) j++;
      if (s) {
        const xa = x(times[i]), xb = j < n ? x(times[j]) : x(times[n - 1] + step);
        const rect = el('rect', { x: xa, y: y0, width: Math.max(1, xb - xa - 2), height: lane, rx: 4, fill: colors[s] || 'var(--muted)' }, svg);
        const from = times[i], to = j < n ? times[j] : times[n - 1] + step;
        rect.addEventListener('pointermove', (ev) => {
          const r = svg.getBoundingClientRect();
          const mins = Math.max(1, Math.round((to - from) / 60000));
          tip.innerHTML = tipRows(name, [{ label: s, color: colors[s], value: `${fmtTime(from)}–${fmtTime(to)} · ${mins} min`, square: true }]);
          placeTip(container, tip, ev.clientX - r.left, ev.clientY - r.top);
        });
        rect.addEventListener('pointerleave', () => { tip.hidden = true; });
        if (onClick) { rect.style.cursor = 'pointer'; rect.addEventListener('click', () => onClick(name)); }
      }
      i = j;
    }
    if (onClick) { label.style.cursor = 'pointer'; label.addEventListener('click', () => onClick(name)); }
  });
  const y = height - 4;
  const xticks = Math.min(6, Math.max(2, Math.floor(w / 110)));
  for (let i = 0; i <= xticks; i++) {
    const t = t0 + ((t1 - t0) * i) / xticks;
    el('text', { x: x(t), y, 'text-anchor': i === 0 ? 'start' : i === xticks ? 'end' : 'middle' }, svg).textContent = axisTime(t1 - t0)(t);
  }
}

/**
 * bars draws horizontal bars, optionally stacked from segments.
 * opts: { rows: [{label, segments: [{value, color, label}], href}], format, legend }
 */
export function bars(container, opts) {
  const { rows, format = String } = opts;
  container.classList.add('chart');
  const old = container.querySelector(':scope > svg');
  const width = Math.max(container.clientWidth, 200);
  const labelW = Math.min(150, Math.max(80, width * 0.25));
  const valueW = 70;
  const bar = 16, gap = 12;
  const height = Math.max(1, rows.length) * (bar + gap) + 4;
  const w = width - labelW - valueW - 8;
  const svg = el('svg', { viewBox: `0 0 ${width} ${height}`, height, role: 'img', 'aria-label': opts.label || '' });
  swap(container, old, svg);
  if (!rows.length) {
    el('text', { x: width / 2, y: height / 2 + 4, 'text-anchor': 'middle' }, svg).textContent = opts.empty || 'Nothing to show';
    return;
  }
  const max = Math.max(1, ...rows.map((r) => r.segments.reduce((a, s) => a + s.value, 0)));
  const tip = tipBox(container);
  rows.forEach((r, i) => {
    const y0 = 2 + i * (bar + gap);
    const g = el('g', {}, svg);
    const label = el('text', { class: 'lane-label', x: labelW - 10, y: y0 + bar / 2 + 4, 'text-anchor': 'end' }, g);
    label.textContent = r.label.length > 18 ? r.label.slice(0, 17) + '…' : r.label;
    let acc = 0;
    const total = r.segments.reduce((a, s) => a + s.value, 0);
    const nonzero = r.segments.filter((s) => s.value > 0);
    nonzero.forEach((s, k) => {
      const xa = labelW + (acc / max) * w;
      const sw = (s.value / max) * w;
      acc += s.value;
      const last = k === nonzero.length - 1;
      const bw = Math.max(1, sw - (last ? 0 : 2));
      // Rounded data end (4px), square at the baseline.
      const rr = last ? Math.min(4, bw) : 0;
      const d = `M${xa},${y0}h${bw - rr}${rr ? `a${rr},${rr} 0 0 1 ${rr},${rr}v${bar - 2 * rr}a${rr},${rr} 0 0 1 -${rr},${rr}` : `v${bar}`}h-${bw - rr}Z`;
      el('path', { d, fill: s.color }, g);
    });
    el('text', { class: 'bar-value', x: labelW + (acc / max) * w + 8, y: y0 + bar / 2 + 4 }, g).textContent = format(total);
    const hit = el('rect', { class: 'hit', x: 0, y: y0 - gap / 2, width, height: bar + gap }, g);
    hit.addEventListener('pointermove', (ev) => {
      const b = svg.getBoundingClientRect();
      tip.innerHTML = tipRows(r.label, r.segments.map((s) => ({ label: s.label, color: s.color, value: format(s.value), square: true })));
      placeTip(container, tip, ev.clientX - b.left, ev.clientY - b.top);
    });
    hit.addEventListener('pointerleave', () => { tip.hidden = true; });
    if (r.href) { hit.style.cursor = 'pointer'; hit.addEventListener('click', () => { location.hash = r.href; }); }
  });
}

/** sparkline returns an <svg> of values (fixed 0..max scale when max given). */
export function sparkline(values, { color = 'var(--s-running)', max, height = 28, width = 200 } = {}) {
  const svg = el('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', class: 'spark', 'aria-hidden': 'true' });
  if (values.length < 2) return svg;
  const top = Math.max(max ?? 0, ...values) || 1;
  const n = values.length;
  const pts = values.map((v, i) => [(i / (n - 1)) * width, height - 2 - (v / top) * (height - 4)]);
  const d = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join('');
  el('path', { d: d + `L${width},${height}L0,${height}Z`, fill: color, 'fill-opacity': 0.12 }, svg);
  el('path', { d, fill: 'none', stroke: color, 'stroke-width': 2, 'vector-effect': 'non-scaling-stroke', 'stroke-linejoin': 'round' }, svg);
  return svg;
}
