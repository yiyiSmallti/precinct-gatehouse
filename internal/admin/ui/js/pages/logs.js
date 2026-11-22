import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  closeDrawer,
  el,
  fmtDateTime,
  fmtNum,
  openDrawer,
  toast,
} from '/admin/js/ui.js';

function statusBadge(status) {
  const s = Number(status || 0);
  if (s >= 200 && s < 300) return el('span', { className: 'pill pill-ok num' }, String(s));
  if (s === 429) return el('span', { className: 'pill pill-warn num' }, '429');
  return el('span', { className: 'pill pill-err num' }, String(s || '-'));
}

function kvRow(k, v) {
  return el('div', { style: { display: 'flex', justifyContent: 'space-between', gap: '12px', padding: '7px 0', borderBottom: '1px solid var(--border)' } },
    el('span', { className: 'muted', style: { fontSize: '13px' } }, k),
    el('span', { className: 'mono num', style: { fontSize: '13px', textAlign: 'right', wordBreak: 'break-all' } }, v)
  );
}

function sectionGroup(title, rows) {
  return el('div', null,
    el('div', { className: 'muted', style: { fontSize: '12px', fontWeight: 600, marginBottom: '4px' } }, title),
    el('div', null, ...rows)
  );
}

export function renderLogs(container, ctx) {
  let filters = {
    key: ctx.query?.key || '',
    model: ctx.query?.model || '',
    status: ctx.query?.status || '',
    protocol: ctx.query?.protocol || '',
  };
  let keyOptions = [];
  let rows = [];
  let hasMore = false;
  let nextBeforeId = 0;
  let loadingMore = false;

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } });
  container.appendChild(root);

  const openDetail = (r) => {
    openDrawer({
      title: `请求明细 / ${r.request_id || r.id}`,
      body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
          statusBadge(r.status),
          r.error_code ? el('span', { className: 'pill pill-err mono' }, r.error_code) : null,
          el('span', { className: 'pill mono' }, r.protocol || 'openai'),
          r.stream ? el('span', { className: 'pill' }, '流式 Stream') : el('span', { className: 'pill' }, '非流式')
        ),
        sectionGroup('请求标识', [
          kvRow('时间', fmtDateTime(r.ts)),
          kvRow('请求编号', r.request_id || '-'),
          kvRow('访问密钥', r.key_name ? `${r.key_name} (${r.key_id || ''})` : (r.key_id || '匿名或鉴权失败')),
          kvRow('客户端地址', r.client_ip || '-'),
          kvRow('准入状态', r.admitted ? '通过' : '拒绝'),
        ]),
        sectionGroup('路由与耗时', [
          kvRow('请求模型', r.requested_model || '-'),
          kvRow('路由模型', r.model || '-'),
          kvRow('路由后端', r.route_backend || '-'),
          kvRow('路由原因', r.route_reason || '-'),
          kvRow('输入 Token', fmtNum(r.prompt_tokens)),
          kvRow('输出 Token', fmtNum(r.completion_tokens)),
          kvRow('缓存命中 Token', fmtNum(r.cached_tokens)),
          kvRow('端到端耗时', `${fmtNum(r.latency_ms)} ms`),
          kvRow('首字延迟 TTFT', r.ttft_ms ? `${fmtNum(r.ttft_ms)} ms` : '-'),
        ]),
        sectionGroup('脱敏审计', [
          kvRow('脱敏策略', r.redaction_mode || 'none'),
          kvRow('敏感触发项', fmtNum(r.redaction_count)),
        ])
      ),
      footer: [
        el('button', { type: 'button', className: 'btn btn-primary', onClick: closeDrawer }, '关闭'),
      ],
    });
  };

  const fetchLogs = async (append = false) => {
    try {
      const params = {
        key: filters.key,
        model: filters.model.trim(),
        status: filters.status,
        protocol: filters.protocol,
        limit: 50,
      };
      if (append && nextBeforeId) params.before_id = nextBeforeId;
      const res = await api.logs(params);
      const list = res.logs || [];
      rows = append ? [...rows, ...list] : list;
      hasMore = Boolean(res.has_more);
      nextBeforeId = res.next_before_id || 0;
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`读取日志失败: ${e.message}`, 'err');
    }
  };

  const init = async () => {
    try {
      const kRes = await api.listKeys();
      keyOptions = kRes.keys || [];
    } catch {
      keyOptions = [];
    }
    await fetchLogs(false);
  };

  const paint = () => {
    clear(root);

    const keySel = el('select', {
      className: 'select',
      style: { width: '170px' },
      'aria-label': '按密钥筛选',
      onChange: (e) => {
        filters.key = e.target.value;
        fetchLogs(false);
      },
    },
      el('option', { value: '' }, '全部密钥'),
      ...keyOptions.map((k) => el('option', { value: k.id, selected: filters.key === k.id }, k.name))
    );

    const statusSel = el('select', {
      className: 'select',
      style: { width: '140px' },
      'aria-label': '按状态筛选',
      onChange: (e) => {
        filters.status = e.target.value;
        fetchLogs(false);
      },
    },
      ...[
        { v: '', l: '全部状态' },
        { v: 'ok', l: '仅成功 (2xx)' },
        { v: 'error', l: '仅异常 (>=400)' },
      ].map((o) => el('option', { value: o.v, selected: filters.status === o.v }, o.l))
    );

    const protoSel = el('select', {
      className: 'select',
      style: { width: '150px' },
      'aria-label': '按协议筛选',
      onChange: (e) => {
        filters.protocol = e.target.value;
        fetchLogs(false);
      },
    },
      ...[
        { v: '', l: '全部协议' },
        { v: 'openai', l: 'OpenAI Chat' },
        { v: 'anthropic', l: 'Claude Messages' },
        { v: 'responses', l: 'OpenAI Responses' },
      ].map((o) => el('option', { value: o.v, selected: filters.protocol === o.v }, o.l))
    );

    const modelIn = el('input', {
      className: 'input mono',
      type: 'search',
      placeholder: '模型名过滤...',
      value: filters.model,
      style: { width: '190px' },
      onKeyDown: (e) => {
        if (e.key === 'Enter') {
          filters.model = modelIn.value;
          fetchLogs(false);
        }
      },
      onInput: (e) => {
        filters.model = e.target.value;
      },
    });

    const toolbar = el('div', { className: 'section-bar' },
      el('div', { className: 'section-bar-left' },
        keySel,
        statusSel,
        protoSel,
        modelIn,
        el('button', { type: 'button', className: 'btn', onClick: () => fetchLogs(false) }, '查询')
      ),
      el('div', { className: 'section-bar-right' },
        el('span', { className: 'muted num', style: { fontSize: '12px' } }, `已显示 ${rows.length} 条`),
        el('button', { type: 'button', className: 'btn', onClick: () => fetchLogs(false) }, icon('refresh', 14), '刷新')
      )
    );

    const sheet = rows.length
      ? el('div', { className: 'sheet' },
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, '时间'),
                el('th', null, '密钥'),
                el('th', null, '协议'),
                el('th', null, '模型路径'),
                el('th', null, '路由'),
                el('th', null, '状态'),
                el('th', { className: 'th-right' }, 'Token (输入 / 输出)'),
                el('th', null, '脱敏'),
                el('th', { className: 'th-right' }, '耗时 / TTFT')
              )
            ),
            el('tbody', null,
              ...rows.map((r) =>
                el('tr', {
                  className: 'row-link',
                  onClick: () => openDetail(r),
                },
                  el('td', { className: 'num muted nowrap' }, fmtDateTime(r.ts)),
                  el('td', { className: 'nowrap' }, el('span', null, r.key_name || r.key_id || '-')),
                  el('td', { className: 'nowrap' }, el('span', { className: 'mono muted-2', style: { fontSize: '12px' } }, r.protocol || 'openai')),
                  el('td', { className: 'mono', style: { fontSize: '12px', maxWidth: '240px' } },
                    r.requested_model && r.model && r.requested_model !== r.model
                      ? [
                        el('div', { className: 'nowrap', style: { overflow: 'hidden', textOverflow: 'ellipsis' }, title: r.requested_model }, r.requested_model),
                        el('div', { className: 'muted nowrap', style: { overflow: 'hidden', textOverflow: 'ellipsis' }, title: r.model }, `-> ${r.model}`),
                      ]
                      : el('div', { className: 'nowrap', style: { overflow: 'hidden', textOverflow: 'ellipsis' }, title: r.model || r.requested_model || '' }, r.model || r.requested_model || '-')
                  ),
                  el('td', { className: 'nowrap' },
                    r.route_backend
                      ? el('span', { className: 'pill mono', title: r.route_reason || '' }, r.route_backend)
                      : el('span', { className: 'muted' }, '-')
                  ),
                  el('td', { className: 'nowrap' },
                    el('div', { style: { display: 'flex', alignItems: 'center', gap: '6px' } },
                      statusBadge(r.status),
                      r.error_code ? el('span', { className: 'mono muted', style: { fontSize: '12px' } }, r.error_code) : null
                    )
                  ),
                  el('td', { className: 'td-right num' },
                    `${fmtNum(r.prompt_tokens)} / ${fmtNum(r.completion_tokens)}`
                  ),
                  el('td', null,
                    r.redaction_count > 0 || (r.redaction_mode && r.redaction_mode !== 'off' && r.redaction_mode !== 'none')
                      ? el('span', { className: `pill ${r.redaction_mode === 'block' ? 'pill-err' : 'pill-brass'} num` },
                        `${r.redaction_mode}${r.redaction_count ? ` (${r.redaction_count})` : ''}`
                      )
                      : el('span', { className: 'muted' }, '-')
                  ),
                  el('td', { className: 'td-right num', style: { whiteSpace: 'nowrap' } },
                    `${fmtNum(r.latency_ms)}ms${r.ttft_ms ? ` / ${fmtNum(r.ttft_ms)}ms` : ''}`
                  )
                )
              )
            )
          )
        ),
        hasMore
          ? el('div', { style: { padding: '14px', borderTop: '1px solid var(--border)', display: 'flex', justifyContent: 'center' } },
            el('button', {
              type: 'button',
              className: 'btn',
              disabled: loadingMore,
              onClick: async () => {
                loadingMore = true;
                await fetchLogs(true);
                loadingMore = false;
              },
            }, loadingMore ? '加载中...' : '加载更多记录')
          )
          : null
      )
      : el('div', { className: 'sheet' },
        el('div', { className: 'sheet-body muted' }, '当前筛选条件下没有请求记录。点击任意行可查看完整报文元信息。')
      );

    root.append(toolbar, sheet);
  };

  init();
}
