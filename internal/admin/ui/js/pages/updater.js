import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import { store } from '/admin/js/store.js';
import {
  clear,
  clone,
  closeModal,
  createSegmented,
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

const RESTARTABLE_STATES = new Set([
  'downloading',
  'verifying',
  'draining',
  'applying',
  'restarting',
]);

export function shouldRetryUpdaterPoll(fromPoll, state, reconnecting) {
  return Boolean(fromPoll && (reconnecting || RESTARTABLE_STATES.has(state)));
}

function stateMeta(state) {
  switch (state) {
    case 'available':
      return { label: '发现新版本', cls: 'pill-brass' };
    case 'checking':
      return { label: '检查中', cls: 'pill-warn' };
    case 'downloading':
      return { label: '拉取更新包', cls: 'pill-warn' };
    case 'verifying':
      return { label: '校验与预检', cls: 'pill-warn' };
    case 'draining':
      return { label: '等待请求排空', cls: 'pill-warn' };
    case 'applying':
      return { label: '切换版本', cls: 'pill-warn' };
    case 'restarting':
      return { label: '正在重启切换', cls: 'pill-warn' };
    case 'error':
      return { label: '检查/更新异常', cls: 'pill-err' };
    default:
      return { label: '已是最新', cls: 'pill-ok' };
  }
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
      } else if (prevReconnecting) {
        paint();
      }

      if (ACTIVE_STATES.has(res.state)) {
        if (RESTARTABLE_STATES.has(res.state) && !versionBeforeRestart) {
          versionBeforeRestart = res.current_version || store.server?.app_version || '';
        }
        schedulePoll(1500);
      }
    } catch (e) {
      if (e.status === 401) return;
      if (shouldRetryUpdaterPoll(fromPoll, status?.state, reconnecting)) {
        if (!versionBeforeRestart) {
          versionBeforeRestart = status?.current_version || store.server?.app_version || '';
        }
        reconnecting = true;
        reconnectTries += 1;
        if (reconnectTries === 1) paint();
        if (reconnectTries <= 40) {
          schedulePoll(1500);
          return;
        }
        reconnecting = false;
        if (status && ACTIVE_STATES.has(status.state)) {
          status = { ...status, state: 'error', last_error: '等待网关重启超时，请刷新页面确认服务状态' };
        }
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

  const triggerApply = async () => {
    try {
      versionBeforeRestart = status?.current_version || store.server?.app_version || '';
      status = await api.updaterApply({ force: false });
      toast('已开始拉取更新，校验通过后将自动切换重启...', 'ok');
      paint();
      schedulePoll(1000);
    } catch (e) {
      toast(`启动升级失败: ${e.message}`, 'err');
    }
  };

  const triggerCheck = async () => {
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
  };

  const setUpdater = (fn) => {
    const next = clone(store.getSection('updater') || {});
    fn(next);
    store.setDraft('updater', next);
  };

  const renderSecretItem = ({ kind, fieldLabel, actionLabel, configured, ref, clearToast }) =>
    el('div', { className: 'opt-item' },
      el('label', { className: 'field-label' }, fieldLabel),
      el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
        el('span', { className: `pill ${configured ? 'pill-ok' : ''}` }, configured ? '已配置' : '未配置'),
        el('button', { type: 'button', className: 'btn btn-sm', onClick: () => openSecretModal(kind) }, `${configured ? '修改' : '设置'}${actionLabel}`),
        configured && String(ref || '').startsWith('db:')
          ? el('button', {
            type: 'button',
            className: 'btn btn-danger btn-sm',
            onClick: async () => {
              try {
                const res = await api.deleteUpdaterSecret(kind, store.baseHash || store.server?.hash || '');
                await store.syncAfterUpdaterSecret(kind, '', res?.hash || '');
                await loadStatus(false);
                toast(clearToast, 'ok');
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
    );

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
    const currentVer = st.current_version || store.server?.app_version || '-';

    const banners = [];
    if (reconnecting) {
      banners.push(
        el('div', { className: 'banner banner-warn' },
          el('div', { className: 'banner-main' },
            icon('refresh', 16),
            el('span', null, '网关正在重启并切换到新版本，页面正在自动重连检测中...')
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
              avail.published_at
                ? el('div', { className: 'mono muted', style: { fontSize: '12px', marginTop: '2px' } }, `发布于 ${fmtDateTime(avail.published_at)}`)
                : null
            )
          ),
          el('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            disabled: isBusy || !canRestart,
            title: canRestart ? '' : '需要以 gatehouse supervise 托管模式运行',
            onClick: () => triggerApply(),
          }, '立即更新')
        )
      );
    }

    const hasTok = Boolean(cfgRaw.has_token || st.config?.has_token);

    const sourceFields = activeSrc === 'oci'
      ? [
        el('div', { className: 'opt-item', style: { gridColumn: '1 / -1' } },
          el('label', { className: 'field-label' }, '镜像地址'),
          el('input', {
            className: 'input mono',
            placeholder: 'registry.cn-chengdu.aliyuncs.com/smallti/gatehouse',
            value: u.image || '',
            onChange: (e) => setUpdater((n) => { n.image = e.target.value.trim(); }),
          })
        ),
      ]
      : [
        el('div', { className: 'opt-item' },
          el('label', { className: 'field-label' }, 'GitHub 仓库'),
          el('input', {
            className: 'input mono',
            placeholder: 'yiyiSmallti/precinct-gatehouse',
            value: u.github_repo || '',
            onChange: (e) => setUpdater((n) => { n.github_repo = e.target.value.trim(); }),
          })
        ),
        renderSecretItem({
          kind: 'token',
          fieldLabel: '访问令牌（可选）',
          actionLabel: '令牌',
          configured: hasTok,
          ref: u.token_ref || cfgRaw.token_ref,
          clearToast: '已清除 GitHub 令牌',
        }),
      ];

    const sheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
            el('h3', { className: 'sheet-title' }, '系统更新'),
            el('span', { className: 'pill mono' }, currentVer),
            el('span', { className: `pill ${sm.cls}` }, sm.label)
          ),
          el('p', { className: 'sheet-sub' },
            st.last_check_at
              ? `上次检查 ${fmtDateTime(st.last_check_at)}`
              : (st.message || '支持切换更新源并手动检查远端版本')
          )
        ),
        el('div', { className: 'sheet-actions' },
          el('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            disabled: checking || isBusy,
            onClick: triggerCheck,
          }, icon('refresh', 14), checking ? '检查中...' : '检查更新')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        ...banners,
        el('div', { className: 'opt-grid' },
          el('div', { className: 'opt-item', style: { gridColumn: '1 / -1' } },
            el('label', { className: 'field-label' }, '更新源'),
            createSegmented({
              ariaLabel: '选择更新源',
              value: activeSrc,
              options: [
                { value: 'oci', label: '国内 OCI 镜像' },
                { value: 'github', label: 'GitHub Releases' },
              ],
              onChange: (val) => {
                setUpdater((n) => { n.source = val; });
                paint();
              },
            })
          ),
          ...sourceFields
        ),
        el('p', { className: 'field-hint', style: { margin: 0 } }, '切换更新源或修改地址后，点击页面底部“应用并热重载”生效。')
      )
    );

    root.appendChild(sheet);
  };

  paint();
  loadStatus(false);
}
