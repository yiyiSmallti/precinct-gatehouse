import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  clone,
  confirmModal,
  el,
  fmtCompact,
  fmtDateTime,
  sourceLabel,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

function computeLineDiff(oldText, newText) {
  const a = String(oldText || '').split('\n');
  const b = String(newText || '').split('\n');
  const m = a.length;
  const n = b.length;

  // Standard LCS DP matrix (guard against huge files > 1500 lines gracefully)
  if (m * n > 900_000) {
    return [
      ...a.map((line, i) => ({ type: 'del', left: i + 1, right: '', text: line })),
      ...b.map((line, i) => ({ type: 'add', left: '', right: i + 1, text: line })),
    ];
  }

  const dp = Array.from({ length: m + 1 }, () => new Uint16Array(n + 1));
  for (let i = m - 1; i >= 0; i -= 1) {
    for (let j = n - 1; j >= 0; j -= 1) {
      if (a[i] === b[j]) dp[i][j] = 1 + dp[i + 1][j + 1];
      else dp[i][j] = dp[i + 1][j] >= dp[i][j + 1] ? dp[i + 1][j] : dp[i][j + 1];
    }
  }

  const out = [];
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      out.push({ type: 'ctx', left: i + 1, right: j + 1, text: a[i] });
      i += 1;
      j += 1;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ type: 'del', left: i + 1, right: '', text: a[i] });
      i += 1;
    } else {
      out.push({ type: 'add', left: '', right: j + 1, text: b[j] });
      j += 1;
    }
  }
  while (i < m) {
    out.push({ type: 'del', left: i + 1, right: '', text: a[i] });
    i += 1;
  }
  while (j < n) {
    out.push({ type: 'add', left: '', right: j + 1, text: b[j] });
    j += 1;
  }
  return out;
}

export function renderSettings(container) {
  let yamlText = '';
  let yamlHash = '';
  let yamlFeedback = null;
  let historyList = [];
  let selectedHist = null;
  let selectedHistYAML = '';

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  container.appendChild(root);

  const loadYAMLAndHistory = async () => {
    try {
      const [yRes, hRes] = await Promise.all([api.getConfigYAML(), api.configHistory()]);
      yamlText = yRes.yaml || '';
      yamlHash = yRes.hash || '';
      historyList = hRes.history || [];
      if (historyList.length && !selectedHist) {
        await selectHistory(historyList[0].name, false);
      }
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`读取配置与历史失败: ${e.message}`, 'err');
    }
  };

  const selectHistory = async (name, shouldPaint = true) => {
    selectedHist = name;
    try {
      const res = await api.configHistoryFile(name);
      selectedHistYAML = res.yaml || '';
    } catch (e) {
      selectedHistYAML = '';
      toast(`读取历史版本失败: ${e.message}`, 'err');
    }
    if (shouldPaint) paint();
  };

  const paint = () => {
    clear(root);
    const cfg = store.server || {};
    const raw = cfg.raw || {};
    const gen = store.getSection('general') || {};

    // 1. General Runtime Settings + Live Status
    const maxBodyIn = el('input', {
      className: 'input num',
      type: 'number',
      min: 0,
      value: String(gen.max_body_mb ?? 32),
      onChange: (e) => {
        const next = clone(store.getSection('general'));
        const rawVal = e.target.value.trim();
        const parsed = rawVal === '' ? 32 : Number(rawVal);
        next.max_body_mb = Number.isFinite(parsed) ? Math.max(0, Math.trunc(parsed)) : 32;
        store.setDraft('general', next);
      },
    });
    const logDaysIn = el('input', {
      className: 'input num',
      type: 'number',
      min: 0,
      value: String(gen.request_log_days ?? 90),
      onChange: (e) => {
        const next = clone(store.getSection('general'));
        const rawVal = e.target.value.trim();
        const parsed = rawVal === '' ? 90 : Number(rawVal);
        next.request_log_days = Number.isFinite(parsed) ? Math.max(0, Math.trunc(parsed)) : 90;
        store.setDraft('general', next);
      },
    });
    const ttlIn = el('input', {
      className: 'input mono',
      value: gen.tokens?.default_ttl || '720h',
      placeholder: '720h',
      onChange: (e) => {
        const next = clone(store.getSection('general'));
        next.tokens = { ...(next.tokens || {}), default_ttl: e.target.value.trim() };
        store.setDraft('general', next);
      },
    });

    const generalSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '常规参数与热重载状态'),
          el('p', { className: 'sheet-sub' }, '管理单请求体限制、日志保留周期与实时配置版本')
        ),
        el('div', { className: 'sheet-actions' },
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: async () => {
              const ok = await confirmModal({
                title: '从磁盘重新加载配置',
                message: '将读取配置文件当前内容校验并原子切换生效快照，适合在手动修改磁盘文件后立刻同步。是否继续？',
                confirmText: '立即重载',
              });
              if (!ok) return;
              try {
                await api.reloadConfig();
                store.discardAll();
                await store.refresh();
                await loadYAMLAndHistory();
                toast('已从磁盘重新加载配置', 'ok');
              } catch (e) {
                toast(`重载失败: ${e.message}`, 'err');
              }
            },
          }, icon('refresh', 14), '从磁盘重载')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '18px' } },
        el('div', { className: 'field-row-3' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '请求体上限 (MB)'),
            maxBodyIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '日志保留天数'),
            logDaysIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '默认令牌 TTL'),
            ttlIn
          )
        ),
        el('div', { style: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(190px, 1fr))', gap: '12px', padding: '12px 14px', borderRadius: 'var(--radius)', background: 'var(--surface-2)', border: '1px solid var(--border)' } },
          el('div', null,
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '生效版本 / 来源'),
            el('div', { className: 'mono num', style: { fontSize: '13px', fontWeight: 600 } }, `v${cfg.version || 1} (${sourceLabel(cfg.source)})`)
          ),
          el('div', null,
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '配置指纹 SHA-256'),
            el('div', { className: 'mono', style: { fontSize: '13px' } }, (cfg.hash || '').slice(0, 16) || '-')
          ),
          el('div', null,
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '数据面 / 管理面监听（需重启变更）'),
            el('div', { className: 'mono', style: { fontSize: '13px' } }, `${raw.listen || '-'} / ${raw.admin?.listen || '-'}`)
          ),
          el('div', { style: { gridColumn: '1 / -1', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px', flexWrap: 'wrap' } },
            el('div', { style: { minWidth: 0 } },
              el('div', { className: 'muted', style: { fontSize: '12px' } }, '配置文件路径'),
              el('div', { className: 'mono wrap-any', style: { fontSize: '12px' } }, cfg.path || '-')
            ),
            cfg.path
              ? el('button', {
                type: 'button',
                className: 'btn btn-sm nowrap',
                onClick: async () => {
                  try {
                    await navigator.clipboard.writeText(cfg.path);
                    toast('已复制配置文件路径', 'ok');
                  } catch {
                    toast('复制失败，请手动选中路径', 'warn');
                  }
                },
              }, icon('copy', 13), '复制路径')
              : null
          )
        ),
        el('p', { className: 'field-hint', style: { margin: 0 } }, '修改上方常规参数后，请在底部待应用栏中统一应用并热重载。'),
        el('div', { className: 'banner banner-brass', style: { padding: '9px 12px', fontSize: '12px' } },
          el('div', { className: 'banner-main' },
            icon('alert', 15),
            el('span', null, '注意：通过控制台应用保存写回 config.yaml 时，系统会重写结构化 YAML 并自动在 config-history 目录保留最近 30 份历史备份，原有文件中的手写 YAML 注释将被移除。')
          )
        )
      )
    );

    // 2. Raw YAML Editor with synchronized line numbers
    const gutter = el('pre', { className: 'yaml-gutter', 'aria-hidden': 'true' });
    const yamlTA = el('textarea', {
      className: 'yaml-textarea',
      spellcheck: false,
      value: yamlText,
      'aria-label': '原始 YAML 配置编辑器',
    });

    const syncLines = () => {
      const count = Math.max(1, yamlTA.value.split('\n').length);
      const nums = [];
      for (let i = 1; i <= count; i += 1) nums.push(String(i));
      gutter.textContent = nums.join('\n');
    };
    syncLines();

    yamlTA.addEventListener('input', () => {
      yamlText = yamlTA.value;
      syncLines();
    });
    yamlTA.addEventListener('scroll', () => {
      gutter.scrollTop = yamlTA.scrollTop;
    });
    yamlTA.addEventListener('keydown', (e) => {
      if (e.key === 'Tab') {
        e.preventDefault();
        const start = yamlTA.selectionStart;
        const end = yamlTA.selectionEnd;
        yamlTA.value = `${yamlTA.value.slice(0, start)}  ${yamlTA.value.slice(end)}`;
        yamlTA.selectionStart = yamlTA.selectionEnd = start + 2;
        yamlText = yamlTA.value;
        syncLines();
      }
    });

    const fbNode = yamlFeedback
      ? el('div', { className: `banner ${yamlFeedback.cls}` },
        el('div', { className: 'banner-main' },
          icon(yamlFeedback.cls === 'banner-ok' ? 'check' : 'alert', 15),
          el('div', null,
            el('div', { style: { fontWeight: 500 } }, yamlFeedback.title),
            ...(yamlFeedback.lines || []).map((l) => el('div', { className: 'mono', style: { fontSize: '12px', marginTop: '2px' } }, l))
          )
        ),
        yamlFeedback.conflict
          ? el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: async () => {
              yamlFeedback = null;
              await store.refresh();
              await loadYAMLAndHistory();
              toast('已同步最新 YAML 内容', 'ok');
            },
          }, '载入最新配置')
          : null
      )
      : null;

    const yamlSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '原始 YAML 编辑器'),
          el('p', { className: 'sheet-sub' }, '支持完整 YAML 编辑、校验与原子热重载')
        ),
        el('div', { className: 'sheet-actions' },
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: async () => {
              yamlFeedback = null;
              await loadYAMLAndHistory();
              toast('已拉取当前生效 YAML', 'ok');
            },
          }, '拉取最新'),
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: async () => {
              try {
                const res = await api.validateConfig({ yaml: yamlTA.value });
                const rr = res.restart_required || [];
                yamlFeedback = {
                  cls: rr.length ? 'banner-warn' : 'banner-ok',
                  title: rr.length ? '校验通过（包含需重启生效的字段）' : 'YAML 语法与运行构建校验通过',
                  lines: rr.length ? [`需重启生效: ${rr.join(', ')}`] : [],
                };
              } catch (e) {
                yamlFeedback = {
                  cls: 'banner-err',
                  title: e.status === 422 ? '组件构建检查失败' : '配置校验未通过',
                  lines: e.problems?.length ? e.problems : [e.message],
                };
              }
              paint();
            },
          }, '校验语法'),
          el('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            onClick: async () => {
              try {
                const res = await api.putConfigYAML(yamlHash || cfg.hash || '', yamlTA.value);
                const rr = res.restart_required || [];
                yamlFeedback = {
                  cls: rr.length ? 'banner-warn' : 'banner-ok',
                  title: `已热重载生效 (v${res.version})`,
                  lines: rr.length ? [`以下启动字段需重启后生效: ${rr.join(', ')}`] : [],
                };
                store.discardAll();
                selectedHist = null;
                await store.refresh();
                await loadYAMLAndHistory();
                toast('YAML 配置已热重载生效', rr.length ? 'warn' : 'ok');
              } catch (e) {
                yamlFeedback = {
                  cls: e.status === 409 ? 'banner-warn' : 'banner-err',
                  title: e.status === 409 ? '配置版本冲突：磁盘或他人已先保存更新' : '应用 YAML 失败',
                  lines: e.problems?.length ? e.problems : [e.message],
                  conflict: e.status === 409,
                };
                paint();
              }
            },
          }, '应用 YAML')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
        fbNode,
        el('div', { className: 'yaml-editor-wrap' }, gutter, yamlTA)
      )
    );

    // 3. History List & LCS Diff + Rollback
    const diffLines = selectedHist ? computeLineDiff(yamlText, selectedHistYAML) : [];
    const changedOnly = diffLines.some((d) => d.type !== 'ctx');

    const historySheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '版本历史与回滚'),
          el('p', { className: 'sheet-sub' }, '每次保存自动备份；点击任一备份对比当前配置差异并支持一键回滚')
        ),
        selectedHist
          ? el('button', {
            type: 'button',
            className: 'btn btn-danger btn-sm',
            onClick: async () => {
              const ok = await confirmModal({
                title: '回滚配置版本',
                message: `确定回滚至备份 “${selectedHist}”？当前配置会自动备份后被替换，并立刻热重载生效。`,
                confirmText: '确认回滚',
                danger: true,
              });
              if (!ok) return;
              try {
                await api.rollbackConfig(selectedHist);
                store.discardAll();
                selectedHist = null;
                await store.refresh();
                await loadYAMLAndHistory();
                toast('已回滚并热重载', 'ok');
              } catch (e) {
                toast(`回滚失败: ${e.message}`, 'err');
              }
            },
          }, '回滚到此版本')
          : null
      ),
      el('div', { className: 'sheet-body' },
        historyList.length
          ? el('div', { className: 'diff-layout' },
            el('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px', maxHeight: '420px', overflowY: 'auto' } },
              ...historyList.map((h) =>
                el('button', {
                  type: 'button',
                  className: `history-item${selectedHist === h.name ? ' active' : ''}`,
                  onClick: () => selectHistory(h.name, true),
                },
                  el('div', null,
                    el('div', { className: 'mono', style: { fontSize: '12px', fontWeight: 600 } }, h.name),
                    el('div', { className: 'muted num', style: { fontSize: '12px' } }, `${fmtDateTime(h.at)}  ${fmtCompact(h.size)} B`)
                  ),
                  selectedHist === h.name ? el('span', { className: 'pill pill-brass nowrap' }, '对比中') : null
                )
              )
            ),
            el('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px', minWidth: 0 } },
              el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '8px', flexWrap: 'wrap', fontSize: '12px' } },
                el('span', { className: 'muted mono' }, selectedHist ? `回滚变更预览 (${selectedHist})` : '选择左侧备份查看差异'),
                el('span', { className: 'muted nowrap' }, changedOnly ? '红色移除当前 / 绿色恢复备份' : '与当前完全一致')
              ),
              el('div', { className: 'diff-box' },
                ...diffLines.map((dl) =>
                  el('div', { className: `diff-line ${dl.type}` },
                    el('span', { className: 'diff-ln' }, dl.left),
                    el('span', { className: 'diff-ln' }, dl.right),
                    el('span', { className: 'diff-sig' }, dl.type === 'add' ? '+' : dl.type === 'del' ? '-' : ' '),
                    el('span', { className: 'diff-code' }, dl.text || ' ')
                  )
                )
              )
            )
          )
          : el('p', { className: 'muted', style: { margin: 0 } }, '尚未产生配置修改备份。修改并保存任意配置后，此处会自动记录可回滚的历史快照。')
      )
    );

    // 4. Change Admin Password
    const oldIn = el('input', { className: 'input', type: 'password', autocomplete: 'current-password', placeholder: '输入当前管理员密码' });
    const newIn = el('input', { className: 'input', type: 'password', autocomplete: 'new-password', placeholder: '至少 8 个字符的新密码' });
    const repIn = el('input', { className: 'input', type: 'password', autocomplete: 'new-password', placeholder: '再次输入新密码' });

    const pwdSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '修改管理员密码'),
          el('p', { className: 'sheet-sub' }, '变更后立即持久化至 admin.password_hash 并保持当前会话')
        )
      ),
      el('form', {
        className: 'sheet-body',
        style: { display: 'flex', flexDirection: 'column', gap: '14px' },
        onSubmit: async (e) => {
          e.preventDefault();
          if (newIn.value.length < 8) {
            toast('新密码长度至少需要 8 个字符', 'warn');
            return;
          }
          if (newIn.value !== repIn.value) {
            toast('两次输入的新密码不一致', 'warn');
            return;
          }
          try {
            await api.changePassword(oldIn.value, newIn.value);
            oldIn.value = '';
            newIn.value = '';
            repIn.value = '';
            toast('管理员密码已更新', 'ok');
          } catch (err) {
            toast(`修改密码失败: ${err.message}`, 'err');
          }
        },
      },
        el('div', { className: 'field-row-3' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '当前密码'),
            oldIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '新密码'),
            newIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '确认新密码'),
            repIn
          )
        ),
        el('div', { style: { display: 'flex', justifyContent: 'flex-end' } },
          el('button', { type: 'submit', className: 'btn btn-primary btn-sm' }, icon('lock', 14), '更新密码')
        )
      )
    );

    root.append(generalSheet, yamlSheet, historySheet, pwdSheet);
  };

  loadYAMLAndHistory();
}
