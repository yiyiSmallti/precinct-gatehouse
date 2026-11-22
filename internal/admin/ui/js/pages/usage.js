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

function daysAgoStr(days) {
  const d = new Date(Date.now() - days * 86400 * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

export function renderUsage(container) {
  let by = 'day';
  let since = daysAgoStr(14);
  let rows = [];

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } });
  container.appendChild(root);

  const load = async () => {
    try {
      const res = await api.usage({ by, since });
      rows = res.rows || [];
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`读取用量失败: ${e.message}`, 'err');
    }
  };

  const paint = () => {
    clear(root);

    const totalReqs = rows.reduce((a, r) => a + Number(r.requests || 0), 0);
    const totalErrs = rows.reduce((a, r) => a + Number(r.errors || 0), 0);
    const totalIn = rows.reduce((a, r) => a + Number(r.prompt_tokens || 0), 0);
    const totalOut = rows.reduce((a, r) => a + Number(r.completion_tokens || 0), 0);
    const totalCached = rows.reduce((a, r) => a + Number(r.cached_tokens || 0), 0);
    const maxReqs = Math.max(1, ...rows.map((r) => Number(r.requests || 0)));

    const toolbar = el('div', { className: 'section-bar' },
      el('div', { className: 'section-bar-left' },
        createSegmented({
          ariaLabel: '用量分组维度',
          value: by,
          options: [
            { value: 'day', label: '按日期' },
            { value: 'key', label: '按密钥' },
            { value: 'model', label: '按模型' },
          ],
          onChange: (v) => {
            by = v;
            load();
          },
        }),
        el('div', { style: { display: 'inline-flex', alignItems: 'center', gap: '6px' } },
          el('span', { className: 'muted', style: { fontSize: '12px' } }, '起始日期'),
          el('input', {
            className: 'input mono num',
            type: 'text',
            pattern: '\\d{4}-\\d{2}-\\d{2}',
            placeholder: 'YYYY-MM-DD',
            value: since,
            style: { width: '132px' },
            onChange: (e) => {
              const v = e.target.value.trim();
              if (!/^\d{4}-\d{2}-\d{2}$/.test(v)) {
                toast('请输入 YYYY-MM-DD 格式日期', 'warn');
                e.target.value = since;
                return;
              }
              since = v;
              load();
            },
          })
        ),
        ...[7, 14, 30].map((d) =>
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: () => {
              since = daysAgoStr(d);
              load();
            },
          }, `近 ${d} 天`)
        )
      ),
      el('div', { className: 'section-bar-right' },
        el('button', { type: 'button', className: 'btn', onClick: load }, icon('refresh', 14), '刷新')
      )
    );

    const summaryStrip = el('div', { className: 'kpi-grid' },
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '区间请求总量'),
        el('div', { className: 'kpi-value' }, fmtCompact(totalReqs)),
        el('div', { className: 'kpi-foot num' }, `异常 ${fmtNum(totalErrs)} 次`)
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '输入 Token'),
        el('div', { className: 'kpi-value' }, fmtCompact(totalIn)),
        el('div', { className: 'kpi-foot num' }, `含缓存命中 ${fmtCompact(totalCached)}`)
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '输出 Token'),
        el('div', { className: 'kpi-value' }, fmtCompact(totalOut)),
        el('div', { className: 'kpi-foot num' }, `总计 ${fmtCompact(totalIn + totalOut)}`)
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '统计条目'),
        el('div', { className: 'kpi-value' }, fmtNum(rows.length)),
        el('div', { className: 'kpi-foot' }, `${since} 起`)
      )
    );

    const colLabel = by === 'day' ? '日期' : by === 'key' ? '访问密钥' : '模型';
    const tableSheet = rows.length
      ? el('div', { className: 'sheet' },
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, colLabel),
                el('th', null, '请求占比'),
                el('th', { className: 'th-right' }, '请求 / 异常'),
                el('th', { className: 'th-right' }, '输入 Token'),
                el('th', { className: 'th-right' }, '输出 Token'),
                el('th', { className: 'th-right' }, '缓存 Token'),
                el('th', { className: 'th-right' }, '平均耗时 / TTFT')
              )
            ),
            el('tbody', null,
              ...rows.map((r) => {
                const pct = Math.min(100, Math.round((Number(r.requests || 0) / maxReqs) * 100));
                return el('tr', null,
                  el('td', null,
                    el('span', { className: by === 'day' ? 'mono num' : 'mono' }, r.label || '-')
                  ),
                  el('td', { style: { minWidth: '140px' } },
                    el('div', { className: 'meter-track' },
                      el('div', { className: 'meter-fill', style: { width: `${pct}%` } })
                    )
                  ),
                  el('td', { className: 'td-right num' },
                    `${fmtNum(r.requests)}`,
                    r.errors > 0 ? el('span', { style: { color: 'var(--oxblood)', marginLeft: '6px' } }, `(${r.errors} 错)`) : null
                  ),
                  el('td', { className: 'td-right num' }, fmtNum(r.prompt_tokens)),
                  el('td', { className: 'td-right num' }, fmtNum(r.completion_tokens)),
                  el('td', { className: 'td-right num' }, fmtNum(r.cached_tokens)),
                  el('td', { className: 'td-right num' }, `${Math.round(r.avg_latency_ms || 0)}ms / ${Math.round(r.avg_ttft_ms || 0)}ms`)
                );
              })
            )
          )
        )
      )
      : el('div', { className: 'sheet' },
        el('div', { className: 'sheet-body muted' }, '所选区间内暂无用量记录。')
      );

    root.append(toolbar, summaryStrip, tableSheet);
  };

  load();
}
