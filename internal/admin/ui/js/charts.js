// Minimal SVG charts for the admin console. No dependencies: the console is
// embedded in the binary and must work offline.
//
// Conventions (see the stats page): colors come from CSS roles (--series-N,
// --grid, --axis); labels use text tokens, never series colors; every chart
// has a hover tooltip and the page also renders a table view.
import { clear, el } from '/admin/js/ui.js';

const NS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs = {}, ...children) {
  const n = document.createElementNS(NS, tag);
  Object.entries(attrs).forEach(([k, v]) => {
    if (v !== undefined && v !== null && v !== false) n.setAttribute(k, String(v));
  });
  children.flat().forEach((c) => {
    if (c === null || c === undefined || c === false) return;
    n.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  });
  return n;
}

export function compact(n) {
  const v = Number(n || 0);
  const a = Math.abs(v);
  if (a >= 1e9) return `${+(v / 1e9).toFixed(2)}B`;
  if (a >= 1e6) return `${+(v / 1e6).toFixed(2)}M`;
  if (a >= 1e4) return `${+(v / 1e3).toFixed(1)}K`;
  return Math.round(v).toLocaleString('en-US');
}

function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / exp;
  const nf = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return nf * exp;
}

// Pick a nice step first and stop at the first multiple that covers max, so
// the plot uses most of its height (1.15M → 0..1.5M, not 0..2M).
function ticks(max, count = 4) {
  if (max <= 0) return { top: 1, ticks: [0, 1] };
  // Counts are integers: never draw a 0.5 gridline that would label as "1" twice.
  const step = max >= 1 ? Math.max(1, niceMax(max / count)) : niceMax(max / count);
  const n = Math.max(1, Math.ceil(max / step - 1e-9));
  const out = [];
  for (let i = 0; i <= n; i += 1) out.push(step * i);
  return { top: step * n, ticks: out };
}

// Shared tooltip, positioned in viewport coordinates.
let tipEl = null;
function tooltip() {
  if (!tipEl) {
    tipEl = el('div', { className: 'chart-tip', role: 'tooltip' });
    document.body.appendChild(tipEl);
  }
  return tipEl;
}

export function showTip(evt, title, rows) {
  const t = tooltip();
  clear(t);
  t.appendChild(el('div', { className: 'chart-tip-title' }, title));
  rows.forEach((r) => {
    t.appendChild(
      el('div', { className: 'chart-tip-row' },
        r.color ? el('span', { className: 'chart-tip-key', style: { background: r.color } }) : el('span', { className: 'chart-tip-key chart-tip-key-none' }),
        el('span', { className: 'chart-tip-val' }, r.value),
        el('span', { className: 'chart-tip-label' }, r.label)
      )
    );
  });
  t.style.display = 'block';
  const pad = 14;
  const rect = t.getBoundingClientRect();
  let x = evt.clientX + pad;
  let y = evt.clientY + pad;
  if (x + rect.width > window.innerWidth - 8) x = evt.clientX - rect.width - pad;
  if (y + rect.height > window.innerHeight - 8) y = evt.clientY - rect.height - pad;
  t.style.left = `${Math.max(8, x)}px`;
  t.style.top = `${Math.max(8, y)}px`;
}

export function hideTip() {
  if (tipEl) tipEl.style.display = 'none';
}

// Re-render on container resize; returns a cleanup function.
export function responsive(host, draw) {
  let lastW = 0;
  const run = () => {
    const w = Math.floor(host.clientWidth);
    if (w > 0 && w !== lastW) {
      lastW = w;
      clear(host);
      draw(w);
    }
  };
  const ro = new ResizeObserver(run);
  ro.observe(host);
  requestAnimationFrame(run);
  return () => ro.disconnect();
}

export function legend(items) {
  return el('div', { className: 'chart-legend' },
    ...items.map((it) =>
      el('span', { className: 'chart-legend-item' },
        el('span', { className: `chart-legend-key ${it.kind === 'line' ? 'is-line' : ''}`, style: { background: it.color } }),
        it.label
      )
    )
  );
}

function roundTopRect(x, y, w, h, r) {
  if (h <= 0) return '';
  const rr = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + rr}Q${x},${y} ${x + rr},${y}H${x + w - rr}Q${x + w},${y} ${x + w},${y + rr}V${y + h}Z`;
}

function axisLabelFor(labels, i, n, w) {
  // Show at most ~w/70 labels, always the first and last.
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(w / 70))));
  if (i === n - 1) return labels[i];
  // Drop a regular tick that would crowd the always-shown last label.
  if (n - 1 - i < every) return null;
  return i % every === 0 ? labels[i] : null;
}

/**
 * Stacked column chart.
 * opts: { width, height, labels: [], series: [{ key, label, color, values: [] }],
 *         fmtX(label) -> short label, fmtTitle(label), fmtValue(v) }
 */
export function stackedColumns(opts) {
  const { width, height = 260, labels, series } = opts;
  const fmtX = opts.fmtX || ((l) => l);
  const fmtTitle = opts.fmtTitle || ((l) => l);
  const fmtValue = opts.fmtValue || compact;
  const m = { t: 14, r: 12, b: 28, l: 52 };
  const iw = width - m.l - m.r;
  const ih = height - m.t - m.b;
  const n = labels.length;
  const totals = labels.map((_, i) => series.reduce((a, s) => a + Number(s.values[i] || 0), 0));
  const { top, ticks: tk } = ticks(Math.max(...totals, 0));
  const y = (v) => m.t + ih - (v / top) * ih;
  const band = iw / Math.max(1, n);
  const bw = Math.max(3, Math.min(24, band * 0.62));

  const svg = svgEl('svg', { class: 'chart-svg', viewBox: `0 0 ${width} ${height}`, width, height, role: 'img' });
  tk.forEach((t) => {
    svg.appendChild(svgEl('line', { x1: m.l, x2: width - m.r, y1: y(t), y2: y(t), class: t === 0 ? 'chart-baseline' : 'chart-grid' }));
    svg.appendChild(svgEl('text', { x: m.l - 8, y: y(t) + 4, class: 'chart-tick', 'text-anchor': 'end' }, compact(t)));
  });
  const hoverBand = svgEl('rect', { class: 'chart-hover-band', y: m.t, height: ih, width: band, x: -999 });
  svg.appendChild(hoverBand);

  labels.forEach((lab, i) => {
    const cx = m.l + band * i + band / 2;
    let acc = 0;
    const g = svgEl('g', { class: 'chart-col' });
    series.forEach((s, si) => {
      const v = Number(s.values[i] || 0);
      if (v <= 0) return;
      const y0 = y(acc);
      const y1 = y(acc + v);
      acc += v;
      const isTop = series.slice(si + 1).every((ss) => Number(ss.values[i] || 0) <= 0);
      // 2px surface gap between stacked segments.
      const hgt = Math.max(0, y0 - y1 - (acc - v > 0 ? 2 : 0));
      if (hgt <= 0) return;
      g.appendChild(svgEl('path', {
        d: isTop ? roundTopRect(cx - bw / 2, y1, bw, hgt, 4) : `M${cx - bw / 2},${y1}h${bw}v${hgt}h${-bw}Z`,
        fill: s.color,
      }));
    });
    svg.appendChild(g);
    const xl = axisLabelFor(labels, i, n, iw);
    if (xl !== null) {
      svg.appendChild(svgEl('text', { x: cx, y: height - 8, class: 'chart-tick', 'text-anchor': 'middle' }, fmtX(xl)));
    }
    const hit = svgEl('rect', { x: m.l + band * i, y: m.t, width: band, height: ih, fill: 'transparent', tabindex: 0, class: 'chart-hit' });
    const show = (evt) => {
      hoverBand.setAttribute('x', m.l + band * i);
      const rows = [...series].reverse().map((s) => ({ color: s.color, label: s.label, value: fmtValue(s.values[i] || 0) }));
      rows.push({ label: '合计', value: fmtValue(totals[i]) });
      showTip(evt, fmtTitle(lab), rows);
    };
    hit.addEventListener('pointermove', show);
    hit.addEventListener('focus', (e) => {
      const r = hit.getBoundingClientRect();
      show({ clientX: r.left + r.width / 2, clientY: r.top + 20, ...e });
    });
    const leave = () => {
      hoverBand.setAttribute('x', -999);
      hideTip();
    };
    hit.addEventListener('pointerleave', leave);
    hit.addEventListener('blur', leave);
    svg.appendChild(hit);
  });
  return svg;
}

/**
 * Multi-line chart with area wash on the first series and a crosshair.
 * opts: { width, height, labels, series: [{ label, color, values, area? }], fmtX, fmtTitle, fmtValue, fmtTick }
 */
export function lineChart(opts) {
  const { width, height = 220, labels, series } = opts;
  const fmtX = opts.fmtX || ((l) => l);
  const fmtTitle = opts.fmtTitle || ((l) => l);
  const fmtValue = opts.fmtValue || compact;
  const fmtTick = opts.fmtTick || compact;
  const m = { t: 14, r: 16, b: 28, l: 52 };
  const iw = width - m.l - m.r;
  const ih = height - m.t - m.b;
  const n = labels.length;
  const maxV = Math.max(0, ...series.flatMap((s) => s.values.map(Number)));
  const { top, ticks: tk } = ticks(maxV);
  const x = (i) => m.l + (n <= 1 ? iw / 2 : (iw * i) / (n - 1));
  const y = (v) => m.t + ih - (Number(v || 0) / top) * ih;

  const svg = svgEl('svg', { class: 'chart-svg', viewBox: `0 0 ${width} ${height}`, width, height, role: 'img' });
  tk.forEach((t) => {
    svg.appendChild(svgEl('line', { x1: m.l, x2: width - m.r, y1: y(t), y2: y(t), class: t === 0 ? 'chart-baseline' : 'chart-grid' }));
    svg.appendChild(svgEl('text', { x: m.l - 8, y: y(t) + 4, class: 'chart-tick', 'text-anchor': 'end' }, fmtTick(t)));
  });
  labels.forEach((lab, i) => {
    const xl = axisLabelFor(labels, i, n, iw);
    if (xl !== null) svg.appendChild(svgEl('text', { x: x(i), y: height - 8, class: 'chart-tick', 'text-anchor': 'middle' }, fmtX(xl)));
  });

  series.forEach((s) => {
    const pts = s.values.map((v, i) => `${x(i)},${y(v)}`);
    if (s.area) {
      svg.appendChild(svgEl('path', {
        d: `M${x(0)},${y(0)}L${pts.join('L')}L${x(n - 1)},${y(0)}Z`,
        fill: s.color,
        'fill-opacity': 0.1,
      }));
    }
    svg.appendChild(svgEl('path', { d: `M${pts.join('L')}`, class: 'chart-line', stroke: s.color }));
    // End marker with a surface ring.
    if (n > 0) svg.appendChild(svgEl('circle', { cx: x(n - 1), cy: y(s.values[n - 1]), r: 4, fill: s.color, class: 'chart-dot' }));
  });

  const cross = svgEl('line', { y1: m.t, y2: m.t + ih, x1: -999, x2: -999, class: 'chart-cross' });
  svg.appendChild(cross);
  const dots = series.map((s) => {
    const d = svgEl('circle', { r: 4, cx: -999, cy: -999, fill: s.color, class: 'chart-dot' });
    svg.appendChild(d);
    return d;
  });
  const overlay = svgEl('rect', { x: m.l, y: m.t, width: iw, height: ih, fill: 'transparent', class: 'chart-hit' });
  overlay.addEventListener('pointermove', (evt) => {
    const box = svg.getBoundingClientRect();
    const px = ((evt.clientX - box.left) / box.width) * width;
    const i = n <= 1 ? 0 : Math.max(0, Math.min(n - 1, Math.round(((px - m.l) / iw) * (n - 1))));
    cross.setAttribute('x1', x(i));
    cross.setAttribute('x2', x(i));
    series.forEach((s, si) => {
      dots[si].setAttribute('cx', x(i));
      dots[si].setAttribute('cy', y(s.values[i]));
    });
    showTip(evt, fmtTitle(labels[i]), series.map((s) => ({ color: s.color, label: s.label, value: fmtValue(s.values[i] || 0) })));
  });
  overlay.addEventListener('pointerleave', () => {
    cross.setAttribute('x1', -999);
    cross.setAttribute('x2', -999);
    dots.forEach((d) => d.setAttribute('cx', -999));
    hideTip();
  });
  svg.appendChild(overlay);
  return svg;
}

/** Simple column histogram (single series). */
export function columns(opts) {
  return stackedColumns({
    ...opts,
    series: [{ label: opts.seriesLabel || '数量', color: opts.color, values: opts.values }],
  });
}

/** Tiny sparkline for stat tiles (de-emphasis line, accent on the last point). */
export function sparkline(values, { width = 120, height = 32, color = 'var(--series-1)' } = {}) {
  const n = values.length;
  const max = Math.max(1, ...values.map(Number));
  const x = (i) => (n <= 1 ? width / 2 : 2 + ((width - 6) * i) / (n - 1));
  const y = (v) => height - 3 - (Number(v || 0) / max) * (height - 8);
  const pts = values.map((v, i) => `${x(i)},${y(v)}`).join('L');
  return svgEl('svg', { class: 'spark', viewBox: `0 0 ${width} ${height}`, width, height, 'aria-hidden': 'true' },
    n ? svgEl('path', { d: `M${x(0)},${height}L${pts}L${x(n - 1)},${height}Z`, fill: color, 'fill-opacity': 0.1 }) : null,
    n ? svgEl('path', { d: `M${pts}`, fill: 'none', stroke: color, 'stroke-width': 1.5, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }) : null,
    n ? svgEl('circle', { cx: x(n - 1), cy: y(values[n - 1]), r: 3, fill: color }) : null
  );
}

/**
 * Weekday × hour heatmap. matrix[weekday 0=Sun][hour].
 */
export function heatmap(opts) {
  const { width, matrix } = opts;
  const order = [1, 2, 3, 4, 5, 6, 0];
  const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  const m = { t: 6, r: 4, b: 22, l: 40 };
  const cw = (width - m.l - m.r) / 24;
  const ch = Math.max(14, Math.min(26, cw * 0.9));
  const height = m.t + ch * 7 + m.b;
  const max = Math.max(1, ...matrix.flat());
  const steps = ['var(--seq-0)', 'var(--seq-1)', 'var(--seq-2)', 'var(--seq-3)', 'var(--seq-4)', 'var(--seq-5)', 'var(--seq-6)'];
  const svg = svgEl('svg', { class: 'chart-svg', viewBox: `0 0 ${width} ${height}`, width, height, role: 'img' });
  order.forEach((d, row) => {
    svg.appendChild(svgEl('text', { x: m.l - 8, y: m.t + ch * row + ch / 2 + 4, class: 'chart-tick', 'text-anchor': 'end' }, names[d]));
    for (let h = 0; h < 24; h += 1) {
      const v = matrix[d]?.[h] || 0;
      const lvl = v === 0 ? 0 : Math.min(6, 1 + Math.floor((v / max) * 5.999));
      const cell = svgEl('rect', {
        x: m.l + cw * h + 1, y: m.t + ch * row + 1, width: Math.max(1, cw - 2), height: ch - 2, rx: 3,
        fill: steps[lvl], class: 'chart-cell', tabindex: 0,
      });
      const show = (evt) => showTip(evt, `${names[d]} ${String(h).padStart(2, '0')}:00–${String(h + 1).padStart(2, '0')}:00`, [{ label: '请求', value: compact(v) }]);
      cell.addEventListener('pointermove', show);
      cell.addEventListener('focus', () => {
        const r = cell.getBoundingClientRect();
        show({ clientX: r.right, clientY: r.bottom });
      });
      cell.addEventListener('pointerleave', hideTip);
      cell.addEventListener('blur', hideTip);
      svg.appendChild(cell);
    }
  });
  [0, 6, 12, 18, 23].forEach((h) => {
    svg.appendChild(svgEl('text', { x: m.l + cw * h + cw / 2, y: height - 6, class: 'chart-tick', 'text-anchor': 'middle' }, `${h}时`));
  });
  return svg;
}

export function heatLegend() {
  return el('div', { className: 'heat-legend' },
    el('span', null, '少'),
    ...[0, 1, 2, 3, 4, 5, 6].map((i) => el('span', { className: 'heat-legend-step', style: { background: `var(--seq-${i})` } })),
    el('span', null, '多')
  );
}

/**
 * Ranked horizontal bars, each split into stacked parts.
 * rows: [{ label, sub?, parts: [{ value, color, label }], total }]
 */
export function rankBars(rows, { fmtValue = compact, onClick = null } = {}) {
  const max = Math.max(1, ...rows.map((r) => r.total));
  return el('div', { className: 'rank-list' },
    ...rows.map((r) => {
      const bar = el('div', { className: 'rank-track' },
        ...r.parts.filter((p) => p.value > 0).map((p) =>
          el('span', { className: 'rank-seg', style: { width: `${(p.value / max) * 100}%`, background: p.color } })
        )
      );
      const row = el(onClick ? 'button' : 'div', {
        type: onClick ? 'button' : undefined,
        className: `rank-row${onClick ? ' is-link' : ''}`,
        onClick: onClick ? () => onClick(r) : undefined,
      },
        el('div', { className: 'rank-head' },
          el('span', { className: 'rank-label', title: r.label }, r.label),
          r.sub ? el('span', { className: 'rank-sub' }, r.sub) : null,
          el('span', { className: 'rank-val num' }, fmtValue(r.total))
        ),
        bar
      );
      row.addEventListener('pointermove', (evt) => showTip(evt, r.label, [
        ...r.parts.map((p) => ({ color: p.color, label: p.label, value: fmtValue(p.value) })),
        { label: '合计', value: fmtValue(r.total) },
      ]));
      row.addEventListener('pointerleave', hideTip);
      return row;
    })
  );
}

/**
 * One whole split into parts (status mix). parts: [{ label, value, color }]
 */
export function segmentBar(parts, { fmtValue = compact } = {}) {
  const total = parts.reduce((a, p) => a + p.value, 0) || 1;
  return el('div', { className: 'segbar-wrap' },
    el('div', { className: 'segbar' },
      ...parts.filter((p) => p.value > 0).map((p) => {
        const seg = el('span', { className: 'segbar-seg', style: { flexGrow: String(p.value), background: p.color } });
        seg.addEventListener('pointermove', (evt) => showTip(evt, p.label, [{ color: p.color, label: '占比', value: `${((p.value / total) * 100).toFixed(1)}%` }, { label: '请求', value: fmtValue(p.value) }]));
        seg.addEventListener('pointerleave', hideTip);
        return seg;
      })
    ),
    el('div', { className: 'segbar-legend' },
      ...parts.map((p) =>
        el('span', { className: 'segbar-legend-item' },
          el('span', { className: 'chart-legend-key', style: { background: p.color } }),
          el('span', null, p.label),
          el('b', { className: 'num' }, fmtValue(p.value)),
          el('span', { className: 'muted num' }, `${((p.value / total) * 100).toFixed(1)}%`)
        )
      )
    )
  );
}
