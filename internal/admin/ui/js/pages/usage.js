import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import { clear, createSegmented, el, fmtNum, toast } from '/admin/js/ui.js';
import {
  compact,
  heatLegend,
  heatmap,
  legend,
  lineChart,
  rankBars,
  responsive,
  segmentBar,
  sparkline,
  stackedColumns,
} from '/admin/js/charts.js';

// Fixed role colors: identity never depends on rank or on which filter is on.
const C = {
  input: 'var(--series-1)',
  cached: 'var(--series-3)',
  output: 'var(--series-2)',
  requests: 'var(--series-1)',
  errors: 'var(--series-8)',
  latency: 'var(--series-7)',
  ttft: 'var(--series-3)',
};

const RANGES = [
  { value: 1, label: '24 小时' },
  { value: 7, label: '7 天' },
  { value: 14, label: '14 天' },
  { value: 30, label: '30 天' },
  { value: 90, label: '90 天' },
];

function fmtMs(v) {
  const n = Number(v || 0);
  if (n >= 10000) return `${(n / 1000).toFixed(1)}s`;
  if (n >= 1000) return `${(n / 1000).toFixed(2)}s`;
  return `${Math.round(n)}ms`;
}

function pct(a, b) {
  return b > 0 ? (a / b) * 100 : 0;
}

// Signed change vs the previous window; upIsGood decides the color.
function deltaBadge(cur, prev, upIsGood = true) {
  if (!prev) return el('span', { className: 'delta' }, '新');
  const d = ((cur - prev) / prev) * 100;
  if (!Number.isFinite(d)) return null;
  const up = d >= 0;
  const good = Math.abs(d) < 0.5 ? '' : (up === upIsGood ? 'good' : 'bad');
  return el('span', { className: `delta ${good}`, title: '与上一同等时段相比' },
    icon(up ? 'arrowUp' : 'arrowDown', 11), Math.abs(d) >= 1000 ? `${(Math.abs(d) / 100 + 1).toFixed(0)}×` : `${Math.abs(d).toFixed(Math.abs(d) < 10 ? 1 : 0)}%`);
}

function statTile({ label, ic, value, accent, foot, spark, sparkColor }) {
  const tile = el('div', { className: 'stat-tile' },
    el('div', { className: 'stat-label' }, icon(ic, 15), label),
    el('div', { className: 'stat-row' },
      el('div', { className: 'stat-value num' }, value),
      spark ? sparkline(spark, { color: sparkColor || accent }) : null
    ),
    el('div', { className: 'stat-foot' }, ...foot)
  );
  tile.style.setProperty('--tile-accent', accent);
  return tile;
}

function card(title, sub, body, extra = null) {
  return el('section', { className: 'sheet chart-card' },
    el('div', { className: 'sheet-head' },
      el('div', { className: 'sheet-title-group' },
        el('h3', { className: 'sheet-title' }, title),
        sub ? el('p', { className: 'sheet-sub' }, sub) : null
      ),
      extra ? el('div', { className: 'chart-card-head-extra' }, extra) : null
    ),
    el('div', { className: 'sheet-body' }, body)
  );
}

function csvEscape(v) {
  const s = String(v ?? '');
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function renderUsage(container, ctx) {
  const q = ctx?.query || {};
  let days = Number(q.days) || 14;
  let keyFilter = q.key || '';
  let modelFilter = q.model || '';
  let data = null;
  let keyOptions = [];
  let modelOptions = [];
  const cleanups = [];
  ctx?.registerCleanup?.(() => cleanups.splice(0).forEach((fn) => fn()));

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '18px' } });
  const filterHost = el('div');
  const bodyHost = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '18px' } });
  root.append(filterHost, bodyHost);
  container.appendChild(root);

  const syncHash = () => {
    const params = { days };
    if (keyFilter) params.key = keyFilter;
    if (modelFilter) params.model = modelFilter;
    const qs = new URLSearchParams(params).toString();
    history.replaceState(null, '', `#/usage?${qs}`);
  };

  const load = async () => {
    bodyHost.classList.add('is-refetching');
    try {
      const params = { days };
      if (keyFilter) params.key = keyFilter;
      if (modelFilter) params.model = modelFilter;
      const [stats, keysRes] = await Promise.all([
        api.stats(params),
        keyOptions.length ? Promise.resolve(null) : api.listKeys().catch(() => null),
      ]);
      data = stats;
      if (keysRes?.keys) keyOptions = keysRes.keys.map((k) => ({ id: k.id, name: k.name }));
      if (!modelFilter) {
        const seen = new Set(modelOptions);
        (stats.by_model || []).forEach((m) => seen.add(m.label));
        modelOptions = [...seen].filter((m) => m && m !== '(unknown)').sort();
      }
      syncHash();
      paintFilters();
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`读取统计失败: ${e.message}`, 'err');
    } finally {
      bodyHost.classList.remove('is-refetching');
    }
  };

  const exportCSV = () => {
    if (!data) return;
    const head = ['时间', '请求', '异常', '输入Token', '缓存命中Token', '输出Token', '平均耗时ms', '平均首字ms'];
    const lines = [head.join(',')].concat(
      data.series.map((b) => [b.t, b.requests, b.errors, b.prompt_tokens, b.cached_tokens, b.completion_tokens,
        Math.round(b.avg_latency_ms), Math.round(b.avg_ttft_ms)].map(csvEscape).join(','))
    );
    const blob = new Blob([`﻿${lines.join('\n')}`], { type: 'text/csv;charset=utf-8' });
    const a = el('a', { href: URL.createObjectURL(blob), download: `gatehouse-usage-${days}d.csv` });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  };

  const paintFilters = () => {
    clear(filterHost);
    const keySel = el('select', {
      className: 'select',
      'aria-label': '按密钥筛选',
      onChange: (e) => {
        keyFilter = e.target.value;
        load();
      },
    },
      el('option', { value: '' }, '全部密钥'),
      ...keyOptions.map((k) => el('option', { value: k.id, selected: k.id === keyFilter }, k.name))
    );
    const modelSel = el('select', {
      className: 'select',
      'aria-label': '按模型筛选',
      onChange: (e) => {
        modelFilter = e.target.value;
        load();
      },
    },
      el('option', { value: '' }, '全部模型'),
      ...modelOptions.map((m) => el('option', { value: m, selected: m === modelFilter }, m))
    );
    filterHost.appendChild(
      el('div', { className: 'filter-row' },
        createSegmented({
          ariaLabel: '时间范围',
          value: days,
          options: RANGES,
          onChange: (v) => {
            days = v;
            load();
          },
        }),
        keySel,
        modelSel,
        el('span', { className: 'filter-spacer' }),
        el('button', { type: 'button', className: 'btn btn-sm', onClick: exportCSV }, icon('download', 14), '导出 CSV'),
        el('button', { type: 'button', className: 'btn btn-sm', onClick: load }, icon('refresh', 14), '刷新')
      )
    );
  };

  const xLabel = (t) => (data.bucket === 'hour' ? t.slice(11, 16) : t.slice(5));
  const titleLabel = (t) => (data.bucket === 'hour' ? `${t.slice(5, 10)} ${t.slice(11, 16)}` : t);

  const paint = () => {
    cleanups.splice(0).forEach((fn) => fn());
    clear(bodyHost);
    if (!data) return;
    const t = data.totals;
    const p = data.prev_totals;
    const s = data.series;
    const tokens = t.prompt_tokens + t.completion_tokens;
    const prevTokens = p.prompt_tokens + p.completion_tokens;
    const okRate = pct(t.requests - t.errors, t.requests);
    const prevOk = pct(p.requests - p.errors, p.requests);
    const rangeName = RANGES.find((r) => r.value === days)?.label || `${days} 天`;

    // Stat tiles
    bodyHost.appendChild(
      el('div', { className: 'stat-grid' },
        statTile({
          label: 'Token 总量', ic: 'token', accent: C.input,
          value: compact(tokens),
          spark: s.map((b) => b.prompt_tokens + b.completion_tokens),
          foot: [deltaBadge(tokens, prevTokens, true), el('span', null, `输入 ${compact(t.prompt_tokens)} · 输出 ${compact(t.completion_tokens)}`)],
        }),
        statTile({
          label: '请求数', ic: 'pulse', accent: 'var(--series-7)',
          value: compact(t.requests),
          spark: s.map((b) => b.requests),
          foot: [deltaBadge(t.requests, p.requests, true), el('span', null, `活跃密钥 ${t.active_keys} · 模型 ${t.active_models}`)],
        }),
        statTile({
          label: '成功率', ic: 'check', accent: 'var(--status-good)',
          value: `${okRate.toFixed(okRate >= 99.95 || okRate === 0 ? 0 : 1)}%`,
          foot: [
            p.requests ? el('span', { className: `delta ${okRate >= prevOk ? 'good' : 'bad'}` }, `${okRate >= prevOk ? '+' : '−'}${Math.abs(okRate - prevOk).toFixed(1)} pt`) : null,
            el('span', null, `异常 ${fmtNum(t.errors)} · 限流 ${fmtNum(t.rate_limited)}`),
          ],
        }),
        statTile({
          label: '平均耗时', ic: 'clock', accent: C.latency,
          value: fmtMs(t.avg_latency_ms),
          spark: s.map((b) => b.avg_latency_ms),
          foot: [deltaBadge(t.avg_latency_ms, p.avg_latency_ms, false), el('span', null, `首字 ${fmtMs(t.avg_ttft_ms)}`)],
        })
      )
    );

    if (!t.requests) {
      bodyHost.appendChild(
        el('div', { className: 'sheet' },
          el('div', { className: 'empty-state' },
            icon('usage', 28),
            el('h4', null, `${rangeName}内没有请求`),
            el('p', null, '有请求经过网关后，这里会出现 Token 趋势、模型与密钥排行和时段分布。')
          )
        )
      );
      return;
    }

    // Token trend (stacked) + model ranking
    const tokenHost = el('div', { className: 'chart-host' });
    cleanups.push(responsive(tokenHost, (w) => {
      tokenHost.appendChild(stackedColumns({
        width: w,
        height: 270,
        labels: s.map((b) => b.t),
        series: [
          { label: '输入（未命中缓存）', color: C.input, values: s.map((b) => Math.max(0, b.prompt_tokens - b.cached_tokens)) },
          { label: '输入（缓存命中）', color: C.cached, values: s.map((b) => b.cached_tokens) },
          { label: '输出', color: C.output, values: s.map((b) => b.completion_tokens) },
        ],
        fmtX: xLabel,
        fmtTitle: titleLabel,
      }));
    }));
    const cacheRate = pct(t.cached_tokens, t.prompt_tokens);

    const modelRows = (data.by_model || []).slice(0, 8).map((m) => ({
      label: m.label,
      sub: `${compact(m.requests)} 次`,
      total: m.prompt_tokens + m.completion_tokens,
      parts: [
        { label: '输入', value: m.prompt_tokens, color: C.input },
        { label: '输出', value: m.completion_tokens, color: C.output },
      ],
    }));

    bodyHost.appendChild(
      el('div', { className: 'viz-grid' },
        card('Token 用量趋势', `${rangeName} · 缓存命中率 ${cacheRate.toFixed(1)}%`,
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
            legend([
              { label: '输入（未命中缓存）', color: C.input },
              { label: '输入（缓存命中）', color: C.cached },
              { label: '输出', color: C.output },
            ]),
            tokenHost
          )
        ),
        card('模型 Token 排行', '点击模型可单独查看',
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '14px' } },
            legend([{ label: '输入', color: C.input }, { label: '输出', color: C.output }]),
            modelRows.length
              ? rankBars(modelRows, {
                onClick: (r) => {
                  modelFilter = modelFilter === r.label ? '' : r.label;
                  load();
                },
              })
              : el('p', { className: 'muted' }, '暂无数据')
          )
        )
      )
    );

    // Requests & latency trend
    const reqHost = el('div', { className: 'chart-host' });
    cleanups.push(responsive(reqHost, (w) => {
      reqHost.appendChild(lineChart({
        width: w,
        height: 220,
        labels: s.map((b) => b.t),
        series: [
          { label: '请求', color: C.requests, values: s.map((b) => b.requests), area: true },
          { label: '异常', color: C.errors, values: s.map((b) => b.errors) },
        ],
        fmtX: xLabel,
        fmtTitle: titleLabel,
        fmtValue: (v) => fmtNum(v),
      }));
    }));
    const latHost = el('div', { className: 'chart-host' });
    cleanups.push(responsive(latHost, (w) => {
      latHost.appendChild(lineChart({
        width: w,
        height: 220,
        labels: s.map((b) => b.t),
        series: [
          { label: '平均耗时', color: C.latency, values: s.map((b) => Math.round(b.avg_latency_ms)) },
          { label: '平均首字', color: C.ttft, values: s.map((b) => Math.round(b.avg_ttft_ms)) },
        ],
        fmtX: xLabel,
        fmtTitle: titleLabel,
        fmtValue: fmtMs,
        fmtTick: fmtMs,
      }));
    }));
    bodyHost.appendChild(
      el('div', { className: 'viz-grid-even' },
        card('请求量与异常', null,
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
            legend([{ label: '请求', color: C.requests, kind: 'line' }, { label: '异常（≥400）', color: C.errors, kind: 'line' }]),
            reqHost
          )
        ),
        card('响应耗时', '成功请求的平均值',
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
            legend([{ label: '平均耗时', color: C.latency, kind: 'line' }, { label: '平均首字（TTFT）', color: C.ttft, kind: 'line' }]),
            latHost
          )
        )
      )
    );

    // Key ranking + status mix + latency distribution
    const keyRows = (data.by_key || []).slice(0, 8).map((k) => ({
      label: k.label,
      id: k.id,
      sub: `${compact(k.requests)} 次${k.errors ? ` · ${compact(k.errors)} 异常` : ''}`,
      total: k.prompt_tokens + k.completion_tokens,
      parts: [
        { label: '输入', value: k.prompt_tokens, color: C.input },
        { label: '输出', value: k.completion_tokens, color: C.output },
      ],
    }));
    const st = data.status || {};
    const latTotal = (data.latency || []).reduce((a, b) => a + b.count, 0);
    const latHistHost = el('div', { className: 'chart-host' });
    cleanups.push(responsive(latHistHost, (w) => {
      latHistHost.appendChild(stackedColumns({
        width: w,
        height: 170,
        labels: (data.latency || []).map((b) => b.label),
        series: [{ label: '请求', color: C.latency, values: (data.latency || []).map((b) => b.count) }],
        fmtValue: (v) => `${fmtNum(v)}（${pct(v, latTotal).toFixed(1)}%）`,
      }));
    }));

    bodyHost.appendChild(
      el('div', { className: 'viz-grid' },
        card('密钥用量排行', '点击密钥可单独查看',
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '14px' } },
            legend([{ label: '输入', color: C.input }, { label: '输出', color: C.output }]),
            keyRows.length
              ? rankBars(keyRows, {
                onClick: (r) => {
                  if (!r.id) return;
                  keyFilter = keyFilter === r.id ? '' : r.id;
                  load();
                },
              })
              : el('p', { className: 'muted' }, '暂无数据')
          )
        ),
        card('响应状态', null,
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '22px' } },
            segmentBar([
              { label: '成功 2xx', value: st['2xx'] || 0, color: 'var(--status-good)' },
              { label: '客户端错误 4xx', value: st['4xx'] || 0, color: 'var(--status-warning)' },
              { label: '限流 429', value: st['429'] || 0, color: 'var(--status-serious)' },
              { label: '上游/网关 5xx', value: st['5xx'] || 0, color: 'var(--status-critical)' },
            ]),
            el('div', null,
              el('div', { className: 'field-label', style: { marginBottom: '6px' } }, '耗时分布（成功请求）'),
              latHistHost
            )
          )
        )
      )
    );

    // Heatmap
    const heatHost = el('div', { className: 'chart-host' });
    cleanups.push(responsive(heatHost, (w) => heatHost.appendChild(heatmap({ width: w, matrix: data.heatmap }))));
    bodyHost.appendChild(card('时段分布', '按星期与小时统计请求数（本地时间）', heatHost, heatLegend()));

    // Table view: every value in the charts is reachable here too.
    bodyHost.appendChild(
      card('明细数据', data.bucket === 'hour' ? '按小时' : '按日期',
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, data.bucket === 'hour' ? '时间' : '日期'),
                el('th', { className: 'th-right' }, '请求'),
                el('th', { className: 'th-right' }, '异常'),
                el('th', { className: 'th-right' }, '输入 Token'),
                el('th', { className: 'th-right' }, '缓存命中'),
                el('th', { className: 'th-right' }, '输出 Token'),
                el('th', { className: 'th-right' }, '平均耗时'),
                el('th', { className: 'th-right' }, '平均首字')
              )
            ),
            el('tbody', null,
              ...[...s].reverse().filter((b) => b.requests > 0).map((b) =>
                el('tr', null,
                  el('td', { className: 'mono num' }, titleLabel(b.t)),
                  el('td', { className: 'td-right num' }, fmtNum(b.requests)),
                  el('td', { className: 'td-right num', style: b.errors ? { color: 'var(--oxblood)' } : null }, fmtNum(b.errors)),
                  el('td', { className: 'td-right num' }, fmtNum(b.prompt_tokens)),
                  el('td', { className: 'td-right num' }, fmtNum(b.cached_tokens)),
                  el('td', { className: 'td-right num' }, fmtNum(b.completion_tokens)),
                  el('td', { className: 'td-right num' }, fmtMs(b.avg_latency_ms)),
                  el('td', { className: 'td-right num' }, fmtMs(b.avg_ttft_ms))
                )
              )
            )
          )
        ),
        null
      )
    );
  };

  paintFilters();
  load();
}
