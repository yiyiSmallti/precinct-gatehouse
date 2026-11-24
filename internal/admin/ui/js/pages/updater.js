import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import { store } from '/admin/js/store.js';
import {
  clear,
  clone,
  closeModal,
  confirmModal,
  createSegmented,
  createSwitch,
  el,
  fmtDateTime,
  openModal,
  toast,
} from '/admin/js/ui.js';

const ACTIVE_STATES = new Set([
  'checking',
  'downloading',
  'verifying',
  'draining',
  'applying',
  'restarting',
]);

function stateMeta(state) {
  switch (state) {
    case 'available':
      return { label: '发现新版本', cls: 'pill-brass' };
    case 'checking':
      return { label: '正在检查', cls: 'pill-warn' };
    case 'downloading':
      return { label: '拉取更新包', cls: 'pill-warn' };
    case 'verifying':
      return { label: '校验与预检', cls: 'pill-warn' };
    case 'draining':
      return { label: '等待请求排空', cls: 'pill-warn' };
    case 'applying':
      return { label: '切换发布目录', cls: 'pill-warn' };
    case 'restarting':
      return { label: '正在重启切换', cls: 'pill-warn' };
    case 'error':
      return { label: '最近检查/更新异常', cls: 'pill-err' };
    default:
      return { label: '空闲 / 最新', cls: 'pill-ok' };
  }
}

function actionLabel(action) {
  switch (action) {
    case 'check':
      return '检查更新';
    case 'apply':
      return '安装升级';
    case 'rollback':
      return '版本回滚';
    case 'auto_rollback':
      return '启动异常自动回滚';
    default:
      return action || '-';
  }
}

function durationSelect({ options, value, onChange, customPlaceholder = '如 10m' }) {
  const isPreset = options.some((o) => String(o.value) === String(value || ''));
  const wrap = el('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } });
  const custom = el('input', {
    className: 'input mono',
    type: 'text',
    placeholder: customPlaceholder,
    value: isPreset ? '' : String(value || ''),
    style: { display: isPreset ? 'none' : 'block', maxWidth: '160px' },
    onChange: (e) => onChange(e.target.value.trim()),
  });
  const sel = el('select', {
    className: 'select',
    onChange: (e) => {
      if (e.target.value === '__custom') {
        custom.style.display = 'block';
        custom.focus();
        return;
      }
      custom.style.display = 'none';
      const opt = options[Number(e.target.value)];
      onChange(opt.value);
    },
  },
    ...options.map((o, i) => el('option', { value: String(i), selected: isPreset && String(o.value) === String(value || '') }, o.label)),
    el('option', { value: '__custom', selected: !isPreset }, '自定义…')
  );
  wrap.append(sel, custom);
  return wrap;
}

export function renderUpdater(container, ctx) {
  let status = null;
  let checking = false;
  let reconnecting = false;
  let reconnectTries = 0;
  let versionBeforeRestart = '';
  let pollTimer = null;

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  container.appendChild(root);

  const stopPoll = () => {
    if (pollTimer) {
      clearTimeout(pollTimer);
      pollTimer = null;
    }
  };

  if (ctx?.registerCleanup) {
    ctx.registerCleanup(stopPoll);
  }

  const schedulePoll = (delay = 1500) => {
    stopPoll();
    pollTimer = setTimeout(() => {
      loadStatus(true);
    }, delay);
  };

  const loadStatus = async (fromPoll = false) => {
    try {
      const res = await api.updaterStatus();
      const prevReconnecting = reconnecting;
      status = res;
      reconnecting = false;
      reconnectTries = 0;

      if (prevReconnecting) {
        await store.refresh().catch(() => {});
        if (versionBeforeRestart && res.current_version && res.current_version !== versionBeforeRestart) {
          toast(`网关已重启并切换至 ${res.current_version}`, 'ok', 5000);
        } else {
          toast(`网关已恢复在线 (${res.current_version || res.active_release || '-'})`, 'ok');
        }
        versionBeforeRestart = '';
      }

      if (!fromPoll || !store.isDirty('updater')) {
        paint();
      } else {
        // Avoid wiping unsaved text inputs; only repaint if explicitly triggered or restarting.
        if (prevReconnecting) paint();
      }

      if (ACTIVE_STATES.has(res.state)) {
        if (res.state === 'restarting' && !versionBeforeRestart) {
          versionBeforeRestart = res.current_version || '';
        }
        schedulePoll(1500);
      }
    } catch (e) {
      if (e.status === 401) return;
      if (fromPoll && (status?.state === 'restarting' || reconnecting)) {
        reconnecting = true;
        reconnectTries += 1;
        if (reconnectTries === 1) paint();
        if (reconnectTries <= 40) {
          schedulePoll(1500);
          return;
        }
        reconnecting = false;
        toast('等待网关重启超时，请刷新页面确认服务状态', 'warn', 6000);
        paint();
        return;
      }
      if (!fromPoll) {
        toast(`读取更新状态失败: ${e.message}`, 'err');
      }
    }
  };

  const openSecretModal = (kind) => {
    const label = kind === 'password' ? 'OCI 镜像仓库密码' : 'GitHub 访问令牌 (PAT)';
    const input = el('input', {
      className: 'input mono',
      type: 'password',
      placeholder: kind === 'password' ? '输入镜像仓库固定密码' : '输入 GitHub Token (repo 只读)',
      autocomplete: 'new-password',
    });
    const saveBtn = el('button', {
      type: 'button',
      className: 'btn btn-primary',
      onClick: async () => {
        const val = input.value.trim();
        if (!val) {
          toast('凭据不能为空', 'warn');
          return;
        }
        saveBtn.disabled = true;
        try {
          const res = await api.putUpdaterSecret(kind, val, store.baseHash || store.server?.hash || '');
          closeModal();
          await store.syncAfterUpdaterSecret(kind, `db:${kind}`, res?.hash || '');
          await loadStatus(false);
          toast(`${label}已安全保存并热重载生效`, 'ok');
        } catch (e) {
          if (e.status === 409) {
            store.markConflict(e.currentHash);
            closeModal();
            toast('配置在您编辑期间已被其他操作修改，请先处理页面底部的冲突提示后再试', 'warn', 5500);
            return;
          }
          toast(`保存失败: ${e.message}`, 'err');
        } finally {
          saveBtn.disabled = false;
        }
      },
    }, '保存凭据');

    openModal({
      title: `设置${label}`,
      body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } },
        el('p', { className: 'muted', style: { margin: 0, fontSize: '13px' } },
          `凭据将写入本地 SQLite settings 表（updater_secret:${kind}），并在配置中绑定为 db:${kind}，不会以明文返回给前端。`
        ),
        input
      ),
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: closeModal }, '取消'),
        saveBtn,
      ],
    });
    requestAnimationFrame(() => input.focus());
  };

  const triggerApply = async (force) => {
    if (force) {
      const inflight = Number(status?.inflight || 0);
      const ok = await confirmModal({
        title: '立即强制升级并重启',
        message: inflight > 0
          ? `当前仍有 ${inflight} 个在途请求，强制升级会立即切版并重启进程，可能中断正在进行的流式回复。确定继续？`
          : '将拉取最新构建、预检配置并立即重启切换到新版本。确定继续？',
        confirmText: '立即升级',
        danger: inflight > 0,
      });
      if (!ok) return;
    }
    try {
      versionBeforeRestart = status?.current_version || store.server?.app_version || '';
      status = await api.updaterApply({ force });
      toast(force ? '已开始强制升级流程...' : '已开始拉取更新，校验通过后将在空闲时自动切版...', 'ok');
      paint();
      schedulePoll(1000);
    } catch (e) {
      toast(`启动升级失败: ${e.message}`, 'err');
    }
  };

  const triggerRollback = async (targetVer = '') => {
    const label = targetVer || status?.previous_release || '上一版本';
    const ok = await confirmModal({
      title: `回滚到 ${label}`,
      message: `确定切换并重启网关到版本 “${label}”？系统会先校验该版本与当前 config.yaml 兼容性，并等待在途请求完成（或超时后切换）。`,
      confirmText: '确认回滚',
      danger: true,
    });
    if (!ok) return;
    try {
      versionBeforeRestart = status?.current_version || store.server?.app_version || '';
      status = await api.updaterRollback({ version: targetVer, force: false });
      toast(`正在准备回滚到 ${label}...`, 'ok');
      paint();
      schedulePoll(1000);
    } catch (e) {
      toast(`启动回滚失败: ${e.message}`, 'err');
    }
  };

  const paint = () => {
    clear(root);
    const st = status || {};
    const cfgRaw = store.server?.raw?.updater || {};
    const u = store.getSection('updater') || {};
    const activeSrc = (u.source || (u.image ? 'oci' : u.github_repo ? 'github' : 'oci')).toLowerCase();
    const sm = stateMeta(st.state);
    const avail = st.available;
    const isBusy = ACTIVE_STATES.has(st.state) || reconnecting;
    const canRestart = Boolean(status && (status.can_restart ?? status.supervised));
    const restartDisabled = isBusy || !canRestart;
    const restartTitle = canRestart ? '' : '需要以 gatehouse supervise 托管模式运行';

    // 1. Status & Actions Sheet
    const banners = [];
    if (status && !canRestart) {
      banners.push(
        el('div', { className: 'banner banner-warn' },
          el('div', { className: 'banner-main' },
            icon('alert', 16),
            el('div', null,
              el('strong', null, '当前为直接 serve 模式，仅支持检查更新'),
              el('div', { style: { fontSize: '12px', marginTop: '2px' } },
                '在线升级、自动应用 (auto_apply) 与版本回滚必须由 `gatehouse supervise` 托管进程执行，以免服务退出后无法自动拉起；如需手动升级，请执行 `gatehouse update apply` 后重启服务。'
              )
            )
          )
        )
      );
    }
    if (reconnecting) {
      banners.push(
        el('div', { className: 'banner banner-warn' },
          el('div', { className: 'banner-main' },
            icon('refresh', 16),
            el('span', null, '网关正在重启并切换到目标版本，页面正在自动重连检测中...')
          )
        )
      );
    }
    if (st.failed_update) {
      banners.push(
        el('div', { className: 'banner banner-err' },
          el('div', { className: 'banner-main' },
            icon('alert', 16),
            el('div', null,
              el('strong', null, `版本 ${st.failed_update.version || '-'} 启动自检失败，已自动回滚`),
              el('div', { className: 'mono', style: { fontSize: '12px', marginTop: '2px' } }, st.failed_update.reason || '')
            )
          )
        )
      );
    }
    if (st.last_error && st.state === 'error') {
      banners.push(
        el('div', { className: 'banner banner-err' },
          el('div', { className: 'banner-main' },
            icon('alert', 16),
            el('div', null,
              el('strong', null, '最近一次检查或更新出错'),
              el('div', { className: 'mono', style: { fontSize: '12px', marginTop: '2px' } }, st.last_error)
            )
          )
        )
      );
    }
    if (avail && avail.update_available) {
      banners.push(
        el('div', { className: 'banner banner-brass', style: { justifyContent: 'space-between', flexWrap: 'wrap', gap: '12px' } },
          el('div', { className: 'banner-main' },
            icon('download', 16),
            el('div', null,
              el('strong', null, `发现新版本：${avail.version || avail.tag || 'latest'}`),
              el('div', { className: 'mono muted', style: { fontSize: '12px', marginTop: '2px' } },
                [
                  avail.digest ? `Digest ${avail.digest.slice(0, 19)}` : '',
                  avail.published_at ? `发布于 ${fmtDateTime(avail.published_at)}` : '',
                  avail.signed ? '已签名' : '',
                ].filter(Boolean).join(' · ')
              )
            )
          ),
          el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
            el('button', {
              type: 'button',
              className: 'btn btn-primary btn-sm',
              disabled: restartDisabled,
              title: restartTitle,
              onClick: () => triggerApply(false),
            }, '等待空闲并升级'),
            el('button', {
              type: 'button',
              className: 'btn btn-sm',
              disabled: restartDisabled,
              title: restartTitle,
              onClick: () => triggerApply(true),
            }, '立即强制升级')
          )
        )
      );
    }

    const statusSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
            el('h3', { className: 'sheet-title' }, '网关版本与自更新状态'),
            el('span', { className: `pill ${sm.cls}` }, sm.label),
            el('span', { className: `pill ${st.supervised ? 'pill-ok' : ''}` }, st.supervised ? 'supervise 托管模式' : '直接运行模式')
          ),
          el('p', { className: 'sheet-sub' }, st.message || '支持定时检查远端发布、Ed25519 验签、在途长请求排空后切版与启动失败自动回滚')
        ),
        el('div', { className: 'sheet-actions' },
          st.previous_release
            ? el('button', {
              type: 'button',
              className: 'btn btn-sm',
              disabled: restartDisabled,
              title: restartTitle,
              onClick: () => triggerRollback(''),
            }, icon('history', 14), `回滚到 ${st.previous_release}`)
            : null,
          el('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            disabled: checking || isBusy,
            onClick: async () => {
              checking = true;
              paint();
              try {
                status = await api.updaterCheck();
                if (status.available?.update_available) {
                  toast(`发现新版本 ${status.available.version || status.available.tag}`, 'ok');
                } else {
                  toast('当前已是最新版本', 'ok');
                }
              } catch (e) {
                if (e.payload?.status) status = e.payload.status;
                toast(`检查更新失败: ${e.message}`, 'err');
              } finally {
                checking = false;
                paint();
              }
            },
          }, icon('refresh', 14), checking ? '检查中...' : '立即检查')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        ...banners,
        el('div', { className: 'kpi-grid' },
          el('div', { className: 'kpi-cell' },
            el('div', { className: 'kpi-label' }, '当前程序版本'),
            el('div', { className: 'kpi-value mono', style: { fontSize: '22px' } }, st.current_version || store.server?.app_version || '-'),
            el('div', { className: 'kpi-foot mono' }, st.current_digest ? st.current_digest.slice(0, 19) : (st.active_release ? `release: ${st.active_release}` : '未记录镜像摘要'))
          ),
          el('div', { className: 'kpi-cell' },
            el('div', { className: 'kpi-label' }, '在途请求 / 保护'),
            el('div', { className: 'kpi-value num', style: { fontSize: '22px' } }, String(st.inflight ?? 0)),
            el('div', { className: 'kpi-foot' }, st.supervised ? '退出码 75 秒级重启 + 健康回滚' : '未由 supervise 托管（建议启用）')
          ),
          el('div', { className: 'kpi-cell' },
            el('div', { className: 'kpi-label' }, '自动轮询计划'),
            el('div', { className: 'kpi-value', style: { fontSize: '20px' } }, u.enabled ? (u.auto_apply ? '自动升级已开' : '仅检查提醒') : '未开启'),
            el('div', { className: 'kpi-foot' }, st.last_check_at ? `上次 ${fmtDateTime(st.last_check_at)}` : '尚未检查')
          ),
          el('div', { className: 'kpi-cell' },
            el('div', { className: 'kpi-label' }, '安装根目录'),
            el('div', { className: 'kpi-value mono', style: { fontSize: '14px', wordBreak: 'break-all', lineHeight: 1.4 } }, st.home_dir || '-'),
            el('div', { className: 'kpi-foot' }, st.previous_release ? `可回滚至 ${st.previous_release}` : '暂无上一版备份')
          )
        )
      )
    );

    // 2. Configuration Sheet
    const setUpdater = (fn) => {
      const next = clone(store.getSection('updater') || {});
      fn(next);
      store.setDraft('updater', next);
    };

    const hasPwd = Boolean(cfgRaw.has_password || st.config?.has_password);
    const hasTok = Boolean(cfgRaw.has_token || st.config?.has_token);

    const sourceGrid = activeSrc === 'oci'
      ? [
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, 'OCI 镜像地址（不含 :tag）'),
          el('input', {
            className: 'input mono',
            placeholder: 'registry.cn-chengdu.aliyuncs.com/smallti/gatehouse',
            value: u.image || '',
            onChange: (e) => setUpdater((n) => { n.image = e.target.value.trim(); }),
          })
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, '追踪 Tag'),
          el('input', {
            className: 'input mono',
            placeholder: 'latest',
            value: u.tag || '',
            onChange: (e) => setUpdater((n) => { n.tag = e.target.value.trim(); }),
          })
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, '仓库用户名'),
          el('input', {
            className: 'input mono',
            placeholder: '如 aliyun9166...',
            value: u.username || '',
            onChange: (e) => setUpdater((n) => { n.username = e.target.value.trim(); }),
          })
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, '仓库密码 / 凭据'),
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
            el('span', { className: `pill ${hasPwd ? 'pill-ok' : ''}` }, hasPwd ? `已配置 (${u.password_ref || cfgRaw.password_ref || 'db:password'})` : '未配置'),
            el('button', { type: 'button', className: 'btn btn-sm', onClick: () => openSecretModal('password') }, hasPwd ? '修改密码' : '设置密码'),
            hasPwd && (u.password_ref || cfgRaw.password_ref || '').startsWith('db:')
              ? el('button', {
                type: 'button',
                className: 'btn btn-danger btn-sm',
                onClick: async () => {
                  try {
                    const res = await api.deleteUpdaterSecret('password', store.baseHash || store.server?.hash || '');
                    await store.syncAfterUpdaterSecret('password', '', res?.hash || '');
                    await loadStatus(false);
                    toast('已清除仓库密码', 'ok');
                  } catch (e) {
                    if (e.status === 409) {
                      store.markConflict(e.currentHash);
                      toast('配置在您编辑期间已被其他操作修改，请先处理页面底部的冲突提示后再试', 'warn', 5500);
                      return;
                    }
                    toast(`清除失败: ${e.message}`, 'err');
                  }
                },
              }, '清除')
              : null
          )
        ),
        el('div', { className: 'opt-item', style: { gridColumn: '1 / -1' } },
          el('label', { className: 'field-label' }, '兜底 DNS 服务器（逗号分隔）'),
          el('input', {
            className: 'input mono',
            placeholder: '留空使用默认 223.5.5.5, 119.29.29.29',
            value: Array.isArray(u.fallback_dns) ? u.fallback_dns.join(', ') : '',
            onChange: (e) => setUpdater((n) => {
              const items = e.target.value.split(',').map((s) => s.trim()).filter(Boolean);
              if (items.length) n.fallback_dns = items;
              else delete n.fallback_dns;
            }),
          }),
          el('p', { className: 'field-hint' }, '仅在系统 DNS 解析失败时自动使用 UDP 查询兜底，无需改动宿主机 /etc/resolv.conf。')
        ),
      ]
      : [
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, 'GitHub 仓库（owner/repo）'),
          el('input', {
            className: 'input mono',
            placeholder: 'yiyiSmallti/precinct-gatehouse',
            value: u.github_repo || '',
            onChange: (e) => setUpdater((n) => { n.github_repo = e.target.value.trim(); }),
          })
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, 'GitHub API 地址（留空用官方）'),
          el('input', {
            className: 'input mono',
            placeholder: 'https://api.github.com',
            value: u.github_api_url || '',
            onChange: (e) => setUpdater((n) => { n.github_api_url = e.target.value.trim(); }),
          })
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, '包含预发布版本 (Prerelease)'),
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', height: '34px' } },
            createSwitch({
              checked: Boolean(u.prerelease),
              label: '允许预发布版本',
              onChange: (on) => setUpdater((n) => { n.prerelease = on; }),
            }),
            el('span', { className: 'muted', style: { fontSize: '12px' } }, u.prerelease ? '包含 pre-release' : '仅稳定版 Latest')
          )
        ),
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, 'GitHub 访问令牌（私有仓库必填）'),
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
            el('span', { className: `pill ${hasTok ? 'pill-ok' : ''}` }, hasTok ? `已配置 (${u.token_ref || cfgRaw.token_ref || 'db:token'})` : '未配置'),
            el('button', { type: 'button', className: 'btn btn-sm', onClick: () => openSecretModal('token') }, hasTok ? '修改令牌' : '设置令牌'),
            hasTok && (u.token_ref || cfgRaw.token_ref || '').startsWith('db:')
              ? el('button', {
                type: 'button',
                className: 'btn btn-danger btn-sm',
                onClick: async () => {
                  try {
                    const res = await api.deleteUpdaterSecret('token', store.baseHash || store.server?.hash || '');
                    await store.syncAfterUpdaterSecret('token', '', res?.hash || '');
                    await loadStatus(false);
                    toast('已清除 GitHub 令牌', 'ok');
                  } catch (e) {
                    if (e.status === 409) {
                      store.markConflict(e.currentHash);
                      toast('配置在您编辑期间已被其他操作修改，请先处理页面底部的冲突提示后再试', 'warn', 5500);
                      return;
                    }
                    toast(`清除失败: ${e.message}`, 'err');
                  }
                },
              }, '清除')
              : null
          )
        ),
      ];

    const configSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '自动更新策略与源配置'),
          el('p', { className: 'sheet-sub' }, '全部参数支持热重载，保存后立即生效，无需重启网关')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '18px' } },
        el('div', { className: 'opt-grid' },
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '启用定时自动检查'),
            el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', height: '34px' } },
              createSwitch({
                checked: Boolean(u.enabled),
                label: '启用定时自动检查',
                onChange: (on) => setUpdater((n) => { n.enabled = on; }),
              }),
              el('span', { className: 'muted', style: { fontSize: '12px' } }, u.enabled ? '后台按周期轮询' : '仅手动检查')
            )
          ),
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '发现新版后自动排空并升级'),
            el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', height: '34px' } },
              createSwitch({
                checked: Boolean(u.auto_apply),
                label: '自动升级',
                onChange: (on) => setUpdater((n) => { n.auto_apply = on; }),
              }),
              el('span', { className: 'muted', style: { fontSize: '12px' } }, u.auto_apply ? '等待在途请求归零后自动切换' : '只提示，由管理员手动触发')
            )
          ),
          el('div', { className: 'opt-item', style: { gridColumn: '1 / -1' } },
            el('label', { className: 'field-label' }, '更新制品源'),
            createSegmented({
              ariaLabel: '选择更新源',
              value: activeSrc,
              options: [
                { value: 'oci', label: '国内 OCI 镜像 (ACR / CCR)' },
                { value: 'github', label: 'GitHub Releases' },
              ],
              onChange: (val) => {
                setUpdater((n) => { n.source = val; });
                paint();
              },
            })
          ),
          ...sourceGrid,
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '检查间隔'),
            durationSelect({
              value: u.interval || '',
              options: [
                { value: '2m', label: '每 2 分钟' },
                { value: '', label: '每 5 分钟（默认）' },
                { value: '15m', label: '每 15 分钟' },
                { value: '1h', label: '每 1 小时' },
              ],
              onChange: (v) => setUpdater((n) => { n.interval = v; }),
            })
          ),
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '长请求排空等待上限'),
            durationSelect({
              value: u.drain_timeout || '',
              options: [
                { value: '1m', label: '最多等 1 分钟' },
                { value: '', label: '最多等 5 分钟（默认）' },
                { value: '15m', label: '最多等 15 分钟' },
                { value: '30m', label: '最多等 30 分钟' },
              ],
              onChange: (v) => setUpdater((n) => { n.drain_timeout = v; }),
            })
          ),
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '保留历史版本数量'),
            el('input', {
              className: 'input num mono',
              type: 'number',
              min: 1,
              max: 20,
              value: String(u.keep_releases || 5),
              onChange: (e) => setUpdater((n) => { n.keep_releases = Math.max(1, Math.trunc(Number(e.target.value) || 5)); }),
            })
          ),
          el('div', { className: 'opt-item' },
            el('label', { className: 'field-label' }, '强制 Ed25519 签名验证'),
            el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', height: '34px' } },
              createSwitch({
                checked: u.require_signature ?? Boolean(u.public_key),
                label: '强制要求签名',
                onChange: (on) => setUpdater((n) => { n.require_signature = on; }),
              }),
              el('span', { className: 'muted', style: { fontSize: '12px' } }, (u.require_signature ?? Boolean(u.public_key)) ? '无有效签名将拒绝安装' : '仅校验 SHA-256（建议配置公钥后开启）')
            )
          ),
          el('div', { className: 'opt-item', style: { gridColumn: '1 / -1' } },
            el('label', { className: 'field-label' }, 'Ed25519 发布验签公钥'),
            el('input', {
              className: 'input mono',
              placeholder: 'ed25519:...（通过 gatehouse update keygen 生成）',
              value: u.public_key || '',
              onChange: (e) => setUpdater((n) => { n.public_key = e.target.value.trim(); }),
            })
          )
        ),
        el('p', { className: 'field-hint', style: { margin: 0 } }, '修改上述选项后，请点击页面底部待应用栏中的“应用并热重载”使之生效。')
      )
    );

    // 3. Installed Releases Sheet
    const rels = st.releases || [];
    const releasesSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '本地已安装版本'),
          el('p', { className: 'sheet-sub' }, '保留当前版本、上一版本及基线二进制，支持一键回滚切换')
        )
      ),
      rels.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, '版本'),
                el('th', null, '状态'),
                el('th', null, '签名'),
                el('th', null, '二进制 SHA-256'),
                el('th', null, '构建 / 更新时间'),
                el('th', null, '说明'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...rels.map((r) =>
                el('tr', null,
                  el('td', { className: 'mono nowrap', style: { fontWeight: 600 } }, r.version),
                  el('td', { className: 'nowrap' },
                    r.current
                      ? el('span', { className: 'pill pill-ok' }, '当前生效')
                      : r.previous
                        ? el('span', { className: 'pill pill-brass' }, '上一版本')
                        : r.baseline
                          ? el('span', { className: 'pill' }, '基线备份')
                          : el('span', { className: 'muted' }, '历史版本')
                  ),
                  el('td', { className: 'nowrap' },
                    r.signed ? el('span', { className: 'pill pill-ok' }, '已验签') : el('span', { className: 'muted' }, '-')
                  ),
                  el('td', { className: 'mono muted', title: r.binary_sha256 || '' }, r.binary_sha256 ? r.binary_sha256.slice(0, 12) : '-'),
                  el('td', { className: 'num muted nowrap' }, fmtDateTime(r.built_at || r.modified_at)),
                  el('td', { className: 'muted' }, r.note || r.short_sha || '-'),
                  el('td', { className: 'td-right nowrap' },
                    r.current
                      ? el('span', { className: 'muted', style: { fontSize: '12px' } }, '运行中')
                      : el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        disabled: restartDisabled,
                        title: restartTitle,
                        onClick: () => triggerRollback(r.version),
                      }, '切换到此版本')
                  )
                )
              )
            )
          )
        )
        : el('div', { className: 'sheet-body muted' }, '当前安装目录下还没有记录 releases/ 版本目录。执行一次升级后将自动建立版本历史。')
    );

    // 4. Update History Sheet
    const hist = st.history || [];
    const historySheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '最近更新记录'),
          el('p', { className: 'sheet-sub' }, '记录后台检查、安装、回滚与启动自检自动回滚事件（最多保留 30 条）')
        )
      ),
      hist.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, '时间'),
                el('th', null, '动作'),
                el('th', null, '版本'),
                el('th', null, '状态'),
                el('th', null, '明细')
              )
            ),
            el('tbody', null,
              ...hist.map((ev) =>
                el('tr', null,
                  el('td', { className: 'num muted nowrap' }, fmtDateTime(ev.at)),
                  el('td', { className: 'nowrap' }, actionLabel(ev.action)),
                  el('td', { className: 'mono nowrap' }, ev.version || '-'),
                  el('td', { className: 'nowrap' },
                    el('span', { className: `pill ${ev.status === 'ok' ? 'pill-ok' : ev.status === 'failed' ? 'pill-err' : ''}` }, ev.status)
                  ),
                  el('td', { className: 'mono muted', style: { fontSize: '12px' } }, ev.detail || '-')
                )
              )
            )
          )
        )
        : el('div', { className: 'sheet-body muted' }, '暂无更新事件记录。')
    );

    root.append(statusSheet, configSheet, releasesSheet, historySheet);
  };

  paint();
  loadStatus(false);
}
