import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  createSegmented,
  el,
  fmtCompact,
  fmtNum,
  toast,
} from '/admin/js/ui.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

function svgNode(tag, attrs = {}, text = null) {
  const n = document.createElementNS(SVG_NS, tag);
  Object.entries(attrs).forEach(([k, v]) => {
    if (v !== undefined && v !== null) n.setAttribute(k, String(v));
  });
  if (text !== null && text !== undefined) n.textContent = String(text);
  return n;
}

let flowAnimated = false;

function strokeWidthFor(count, total) {
  if (!total || count <= 0) return 2;
  const ratio = Math.max(0.06, Math.min(1, count / total));
  return +(2 + ratio * 2.5).toFixed(2);
}

export function renderOverview(container, ctx) {
  let range = '24h';
  let selectedGate = null;
  let flowData = null;
  let ovData = null;

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  container.appendChild(root);

  const load = async () => {
    try {
      const [f, o] = await Promise.all([api.flow(range), api.overview()]);
      flowData = f;
      ovData = o;
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`加载概览失败: ${e.message}`, 'err');
    }
  };

  const paint = () => {
    clear(root);
    if (!flowData || !ovData) {
      root.appendChild(el('div', { className: 'sheet' }, el('div', { className: 'sheet-body muted' }, '正在加载概览数据...')));
      return;
    }

    const total = Number(flowData.total || 0);
    const authRej = Number(flowData.auth_rejected || 0);
    const polRej = Number(flowData.policy_rejected || 0);
    const rateRej = Number(flowData.rate_limited || 0);
    const polRateRej = polRej + rateRej;
    const redBlk = Number(flowData.redaction_blocked || 0);
    const upFail = Number(flowData.upstream_failed || 0) + Number(flowData.other_failed || 0);
    const succeeded = Number(flowData.succeeded || 0);

    const afterAuth = Math.max(0, total - authRej);
    const afterPol = Math.max(0, afterAuth - polRateRej);
    const afterRed = Math.max(0, afterPol - redBlk);
    const passRate = total > 0 ? ((succeeded / total) * 100).toFixed(1) : '100.0';

    const gates = [
      {
        id: 'auth',
        x: 172,
        cx: 230,
        title: '密钥鉴权',
        sub: '身份与过期校验',
        branchLabel: '鉴权拒绝',
        rejected: authRej,
        remainingAfter: afterAuth,
        detail: `未通过密钥校验 ${fmtNum(authRej)} 次`,
        topCodes: flowData.top_errors?.auth || [],
        logStatus: 'error',
      },
      {
        id: 'policy',
        x: 336,
        cx: 394,
        title: '策略与限流',
        sub: '模型授权与 RPM',
        branchLabel: '超限或禁止',
        rejected: polRateRej,
        remainingAfter: afterPol,
        detail: `策略禁止 ${fmtNum(polRej)} 次，限流并发拒绝 ${fmtNum(rateRej)} 次`,
        topCodes: [...(flowData.top_errors?.policy || []), ...(flowData.top_errors?.rate || [])],
        logStatus: 'error',
      },
      {
        id: 'redact',
        x: 500,
        cx: 558,
        title: '隐私脱敏',
        sub: '敏感检测与阻断',
        branchLabel: '敏感阻断',
        rejected: redBlk,
        remainingAfter: afterRed,
        detail: `触发阻断策略 ${fmtNum(redBlk)} 次`,
        topCodes: flowData.top_errors?.redaction || [],
        logStatus: 'error',
      },
      {
        id: 'upstream',
        x: 664,
        cx: 722,
        title: '上游调度',
        sub: '故障转移与转发',
        branchLabel: '上游异常',
        rejected: upFail,
        remainingAfter: succeeded,
        detail: `上游失败 ${fmtNum(flowData.upstream_failed || 0)} 次，其他异常 ${fmtNum(flowData.other_failed || 0)} 次`,
        topCodes: flowData.top_errors?.upstream || [],
        logStatus: 'error',
      },
    ];

    if (!selectedGate) {
      selectedGate = gates.find((g) => g.rejected > 0)?.id || 'auth';
    }

    // Build Hero SVG
    const playAnim = !flowAnimated;
    flowAnimated = true;

    const svg = svgNode('svg', {
      viewBox: '0 56 960 202',
      class: 'flow-svg',
      role: 'img',
      'aria-label': '请求门禁流水线统计图',
    });

    // Entry box
    const entryG = svgNode('g');
    entryG.appendChild(svgNode('rect', { x: 16, y: 74, width: 116, height: 70, rx: 6, class: 'flow-node-box entry' }));
    entryG.appendChild(svgNode('text', { x: 74, y: 98, 'text-anchor': 'middle', fill: 'var(--ink-3)', 'font-size': 12 }, '入站请求'));
    entryG.appendChild(svgNode('text', {
      x: 74, y: 126, 'text-anchor': 'middle', fill: 'var(--ink)', 'font-size': 22, 'font-weight': 600,
      style: 'font-variant-numeric: tabular-nums',
    }, fmtCompact(total)));
    svg.appendChild(entryG);

    // Trunk segments (from entry -> gate1 -> gate2 -> gate3 -> gate4 -> exit)
    const segCoords = [
      { x1: 132, x2: 172, count: total },
      { x1: 288, x2: 336, count: afterAuth },
      { x1: 452, x2: 500, count: afterPol },
      { x1: 616, x2: 664, count: afterRed },
      { x1: 780, x2: 826, count: succeeded },
    ];
    segCoords.forEach((seg) => {
      const w = strokeWidthFor(seg.count, total);
      svg.appendChild(svgNode('path', {
        d: `M ${seg.x1} 109 L ${seg.x2} 109`,
        class: `flow-trunk${playAnim ? ' flow-trunk-anim' : ''}`,
        'stroke-width': w,
        opacity: seg.count > 0 || total === 0 ? 1 : 0.35,
      }));
      if (total > 0) {
        svg.appendChild(svgNode('text', {
          x: (seg.x1 + seg.x2) / 2,
          y: 96,
          'text-anchor': 'middle',
          fill: 'var(--ink-3)',
          'font-size': 11,
          style: 'font-variant-numeric: tabular-nums',
        }, fmtCompact(seg.count)));
      }
    });

    // Gate stations + rejection branches
    gates.forEach((g) => {
      const isHot = g.rejected > 0;
      const isActive = selectedGate === g.id;
      const group = svgNode('g', {
        class: 'flow-gate-hit',
        tabindex: '0',
        role: 'button',
        'aria-label': `${g.title}，拦截或失败 ${g.rejected} 次`,
      });
      group.addEventListener('click', () => {
        selectedGate = g.id;
        paint();
      });
      group.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          selectedGate = g.id;
          paint();
        }
      });

      // Branch line downwards
      const branchW = isHot ? Math.min(4.5, Math.max(1.8, (g.rejected / Math.max(1, total)) * 8)) : 1.4;
      group.appendChild(svgNode('path', {
        d: `M ${g.cx} 141 L ${g.cx} 194`,
        class: `flow-branch${isHot ? ' hot' : ''}${playAnim ? ' flow-branch-anim' : ''}`,
        'stroke-width': branchW.toFixed(1),
      }));

      // Gate station box
      group.appendChild(svgNode('rect', {
        x: g.x,
        y: 77,
        width: 116,
        height: 64,
        rx: 6,
        class: `flow-node-box${isActive ? ' active' : ''}`,
      }));
      group.appendChild(svgNode('text', {
        x: g.cx,
        y: 102,
        'text-anchor': 'middle',
        fill: 'var(--ink)',
        'font-size': 13,
        'font-weight': 600,
      }, g.title));
      group.appendChild(svgNode('text', {
        x: g.cx,
        y: 123,
        'text-anchor': 'middle',
        fill: 'var(--ink-2)',
        'font-size': 12,
      }, g.sub));

      // Rejection pill
      group.appendChild(svgNode('rect', {
        x: g.x - 2,
        y: 194,
        width: 120,
        height: 52,
        rx: 6,
        class: `flow-reject-pill${isHot ? ' hot' : ''}`,
      }));
      group.appendChild(svgNode('text', {
        x: g.cx,
        y: 215,
        'text-anchor': 'middle',
        fill: isHot ? 'var(--oxblood)' : 'var(--ink-3)',
        'font-size': 11,
        'font-weight': isHot ? 500 : 400,
      }, g.branchLabel));
      group.appendChild(svgNode('text', {
        x: g.cx,
        y: 236,
        'text-anchor': 'middle',
        fill: isHot ? 'var(--oxblood)' : 'var(--ink-2)',
        'font-size': 15,
        'font-weight': 600,
        style: 'font-variant-numeric: tabular-nums',
      }, fmtNum(g.rejected)));

      svg.appendChild(group);
    });

    // Exit Node
    const exitG = svgNode('g');
    exitG.appendChild(svgNode('rect', { x: 826, y: 74, width: 118, height: 70, rx: 6, class: 'flow-node-box ok' }));
    exitG.appendChild(svgNode('text', { x: 885, y: 98, 'text-anchor': 'middle', fill: 'var(--verdigris)', 'font-size': 12, 'font-weight': 500 }, '成功交付'));
    exitG.appendChild(svgNode('text', {
      x: 885, y: 126, 'text-anchor': 'middle', fill: 'var(--verdigris)', 'font-size': 22, 'font-weight': 600,
      style: 'font-variant-numeric: tabular-nums',
    }, fmtCompact(succeeded)));
    svg.appendChild(exitG);

    const activeGate = gates.find((g) => g.id === selectedGate) || gates[0];
    const codeTags = activeGate.topCodes.map((tc) =>
      el('span', { className: 'pill pill-err mono num' }, `${tc.code} (${tc.count})`)
    );

    const heroSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '门禁流'),
          el('p', { className: 'sheet-sub' }, '入站请求沿四级检查推进，点击任意关卡查看分流明细')
        ),
        el('div', { className: 'sheet-actions' },
          el('span', { className: 'pill pill-ok num' }, `放行率 ${passRate}%`),
          createSegmented({
            ariaLabel: '门禁流时间范围',
            value: range,
            options: [
              { value: '1h', label: '近 1 小时' },
              { value: '24h', label: '近 24 小时' },
            ],
            onChange: (val) => {
              range = val;
              load();
            },
          }),
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: load,
          }, icon('refresh', 14), '刷新')
        )
      ),
      el('div', { className: 'hero-flow' },
        svg,
        el('div', { className: 'flow-inspector' },
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' } },
            el('span', { className: 'pill pill-brass' }, activeGate.title),
            el('span', { className: 'num' }, activeGate.detail),
            ...codeTags
          ),
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: () => ctx.navigate('logs', { status: 'error' }),
          }, '查看异常日志')
        )
      )
    );

    // KPI Row
    const td = ovData.today || {};
    const totalTokens = Number(td.prompt_tokens || 0) + Number(td.completion_tokens || 0);
    const ttftMs = Math.round(Number(flowData.avg_ttft_ms || td.avg_ttft_ms || 0));

    const kpis = el('section', { className: 'kpi-grid' },
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '今日请求'),
        el('div', { className: 'kpi-value' }, fmtCompact(td.requests || 0)),
        el('div', { className: 'kpi-foot' },
          el('span', { className: 'num' }, `累计 ${fmtNum(td.requests || 0)} 次`),
          Number(td.errors || 0) > 0
            ? el('span', { className: 'pill pill-err num' }, `异常 ${fmtNum(td.errors)}`)
            : el('span', { className: 'pill pill-ok' }, '无异常')
        )
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '今日 Token'),
        el('div', { className: 'kpi-value' }, fmtCompact(totalTokens)),
        el('div', { className: 'kpi-foot num' },
          el('span', null, `输入 ${fmtCompact(td.prompt_tokens || 0)}`),
          el('span', null, `输出 ${fmtCompact(td.completion_tokens || 0)}`),
          Number(td.cached_tokens || 0) > 0 ? el('span', null, `缓存 ${fmtCompact(td.cached_tokens)}`) : null
        )
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '当前在途并发'),
        el('div', { className: 'kpi-value' }, fmtNum(ovData.inflight_total || 0)),
        el('div', { className: 'kpi-foot num' }, `活跃密钥 ${(ovData.keys || []).length} 个`)
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '平均首字延迟 TTFT'),
        el('div', { className: 'kpi-value' }, ttftMs ? `${ttftMs}ms` : '-'),
        el('div', { className: 'kpi-foot' }, `统计窗口 ${range === '1h' ? '近 1 小时' : '近 24 小时'}`)
      )
    );

    // Hourly SVG chart + Model Breakdown
    const hourly = ovData.hourly || [];
    const hoverReadout = el('span', { className: 'muted num', style: { fontSize: '12px' } }, '悬停柱条查看时段详情');
    const maxHour = Math.max(1, ...hourly.map((h) => Number(h.requests || 0)));

    const chartSvg = svgNode('svg', { viewBox: '0 0 600 170', style: 'width:100%;height:auto;display:block;' });
    const barW = hourly.length ? Math.max(8, Math.floor(540 / Math.max(1, hourly.length)) - 6) : 14;
    const stepW = hourly.length ? 556 / hourly.length : 24;
    chartSvg.appendChild(svgNode('line', { x1: 24, y1: 140, x2: 584, y2: 140, stroke: 'var(--border-strong)', 'stroke-width': 1 }));

    hourly.forEach((h, idx) => {
      const reqs = Number(h.requests || 0);
      const errs = Number(h.errors || 0);
      const okCount = Math.max(0, reqs - errs);
      const totalH = Math.round((reqs / maxHour) * 116);
      const errH = reqs > 0 ? Math.min(totalH, Math.round((errs / maxHour) * 116)) : 0;
      const okH = Math.max(0, totalH - errH);
      const bx = Math.round(28 + idx * stepW + (stepW - barW) / 2);

      const gBar = svgNode('g', { style: 'cursor:pointer;' });
      if (okH > 0) {
        gBar.appendChild(svgNode('rect', {
          x: bx, y: 140 - okH, width: barW, height: okH, rx: 2,
          fill: 'var(--verdigris)', opacity: '0.88',
        }));
      }
      if (errH > 0) {
        gBar.appendChild(svgNode('rect', {
          x: bx, y: 140 - okH - errH, width: barW, height: errH, rx: 2,
          fill: 'var(--oxblood)',
        }));
      }
      if (totalH === 0) {
        gBar.appendChild(svgNode('rect', {
          x: bx, y: 138, width: barW, height: 2, rx: 1, fill: 'var(--border-strong)',
        }));
      }
      if (idx % 4 === 0 || idx === hourly.length - 1) {
        const lbl = String(h.hour || '').slice(-5) || String(idx);
        gBar.appendChild(svgNode('text', {
          x: bx + barW / 2, y: 158, 'text-anchor': 'middle', fill: 'var(--ink-3)', 'font-size': 10,
        }, lbl));
      }
      gBar.addEventListener('mouseenter', () => {
        hoverReadout.textContent = `${h.hour}  总请求 ${fmtNum(reqs)}  异常 ${fmtNum(errs)}`;
      });
      chartSvg.appendChild(gBar);
    });

    const hourlySheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '24 小时请求节奏'),
          hoverReadout
        ),
        el('div', { style: { display: 'flex', gap: '12px', fontSize: '12px' } },
          el('span', { style: { display: 'inline-flex', alignItems: 'center', gap: '5px', color: 'var(--verdigris)' } }, el('span', { className: 'dot' }), '成功'),
          el('span', { style: { display: 'inline-flex', alignItems: 'center', gap: '5px', color: 'var(--oxblood)' } }, el('span', { className: 'dot' }), '异常')
        )
      ),
      el('div', { className: 'sheet-body' },
        hourly.length ? chartSvg : el('p', { className: 'muted' }, '暂无小时级统计数据')
      )
    );

    const byModel = ovData.by_model?.length ? ovData.by_model : (flowData.by_model || []);
    const maxModelReq = Math.max(1, ...byModel.map((m) => Number(m.requests || m.total || 0)));
    const modelBody = byModel.length
      ? el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
        ...byModel.slice(0, 6).map((m) => {
          const cnt = Number(m.requests ?? m.total ?? 0);
          const err = Number(m.errors ?? (m.total !== undefined && m.succeeded !== undefined ? m.total - m.succeeded : 0));
          const pct = Math.min(100, Math.round((cnt / maxModelReq) * 100));
          const toks = Number(m.prompt_tokens || 0) + Number(m.completion_tokens || 0);
          return el('div', { className: 'meter-line' },
            el('div', { className: 'meter-meta' },
              el('span', { className: 'mono', style: { color: 'var(--ink)' } }, m.model || '(未识别模型)'),
              el('span', { className: 'num' }, `${fmtNum(cnt)} 次${toks ? ` / ${fmtCompact(toks)} tokens` : ''}${err ? ` / 异常 ${err}` : ''}`)
            ),
            el('div', { className: 'meter-track' },
              el('div', { className: `meter-fill${err > cnt * 0.3 ? ' danger' : ''}`, style: { width: `${pct}%` } })
            )
          );
        })
      )
      : el('p', { className: 'muted', style: { margin: 0 } }, '暂无模型调用记录');

    const modelSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '模型流量分布'),
          el('p', { className: 'sheet-sub' }, '按请求量与 Token 吞吐排列')
        ),
        el('button', { type: 'button', className: 'btn btn-ghost btn-sm', onClick: () => ctx.navigate('usage') }, '完整用量')
      ),
      el('div', { className: 'sheet-body' }, modelBody)
    );

    // Keys Realtime Table
    const keyRows = ovData.keys || [];
    const keySheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '密钥实时负荷'),
          el('p', { className: 'sheet-sub' }, '活跃访问密钥的当前并发、RPM 与今日计数')
        ),
        el('button', { type: 'button', className: 'btn btn-ghost btn-sm', onClick: () => ctx.navigate('keys') }, '管理密钥')
      ),
      el('div', { className: 'table-wrap' },
        keyRows.length
          ? el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, '密钥名称'),
                el('th', { className: 'th-right' }, '在途并发'),
                el('th', { className: 'th-right' }, '当前 RPM'),
                el('th', { className: 'th-right' }, '今日请求'),
                el('th', { className: 'th-right' }, '明细')
              )
            ),
            el('tbody', null,
              ...keyRows.map((k) =>
                el('tr', null,
                  el('td', null, el('strong', null, k.name || k.id)),
                  el('td', { className: 'td-right num' }, fmtNum(k.inflight || 0)),
                  el('td', { className: 'td-right num' }, fmtNum(k.rpm_used || 0)),
                  el('td', { className: 'td-right num' }, fmtNum(k.today_used || 0)),
                  el('td', { className: 'td-right' },
                    el('button', {
                      type: 'button',
                      className: 'btn btn-ghost btn-sm',
                      onClick: () => ctx.navigate('logs', { key: k.id }),
                    }, '日志')
                  )
                )
              )
            )
          )
          : el('div', { className: 'sheet-body muted' }, '暂无启用中的 API 密钥')
      )
    );

    root.append(
      heroSheet,
      kpis,
      el('div', { className: 'split-2' }, hourlySheet, modelSheet),
      keySheet
    );
  };

  load();
}
