import { api, setUnauthorizedHandler } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  applyFieldProblems,
  clear,
  clearFieldErrors,
  closeModal,
  el,
  openModal,
  sectionLabel,
  sourceLabel,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

import { renderOverview } from '/admin/js/pages/overview.js';
import { renderKeys } from '/admin/js/pages/keys.js';
import { renderChannels } from '/admin/js/pages/channels.js';
import { renderRouting } from '/admin/js/pages/routing.js';
import { renderTiered } from '/admin/js/pages/tiered.js';
import { renderRedaction } from '/admin/js/pages/redaction.js';
import { renderLogs } from '/admin/js/pages/logs.js';
import { renderUsage } from '/admin/js/pages/usage.js';
import { renderSettings } from '/admin/js/pages/settings.js';

const NAV_GROUPS = [
  {
    label: '运行监测',
    items: [
      { id: 'overview', title: '概览与门禁流', ic: 'overview' },
      { id: 'logs', title: '请求日志', ic: 'logs' },
      { id: 'usage', title: '用量统计', ic: 'usage' },
    ],
  },
  {
    label: '策略与配置',
    items: [
      { id: 'keys', title: '访问密钥', ic: 'keys' },
      { id: 'channels', title: '模型渠道', ic: 'channels', sec: 'providers' },
      { id: 'routing', title: '路由与别名', ic: 'routing', sec: ['routing', 'aliases'] },
      { id: 'tiered', title: '智能路由', ic: 'tiers', sec: 'providers' },
      { id: 'redaction', title: '隐私脱敏', ic: 'redaction', sec: 'redaction' },
      { id: 'settings', title: '系统与配置', ic: 'settings', sec: 'general' },
    ],
  },
];

const ROUTE_TITLES = {
  overview: '概览与门禁流',
  logs: '请求日志',
  usage: '用量统计',
  keys: '访问密钥',
  channels: '模型渠道',
  routing: '路由与别名',
  tiered: '智能路由',
  redaction: '隐私脱敏',
  settings: '系统与配置',
};

const appRoot = document.getElementById('app');
const pendingRoot = document.getElementById('pending-root');

let state = {
  user: null,
  route: 'overview',
  query: {},
  sidebarOpen: false,
  theme: localStorage.getItem('gh_theme') || '',
  pollTimer: null,
};

function applyTheme(pref) {
  if (pref === 'light' || pref === 'dark') {
    document.documentElement.setAttribute('data-theme', pref);
  } else {
    document.documentElement.removeAttribute('data-theme');
  }
}

function effectiveThemeIsDark() {
  if (state.theme === 'dark') return true;
  if (state.theme === 'light') return false;
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
}

function toggleTheme() {
  const next = effectiveThemeIsDark() ? 'light' : 'dark';
  state.theme = next;
  localStorage.setItem('gh_theme', next);
  applyTheme(next);
  renderShell();
}

function parseHash() {
  const raw = window.location.hash.replace(/^#\/?/, '') || 'overview';
  const [pathPart, qPart] = raw.split('?');
  const route = ROUTE_TITLES[pathPart] ? pathPart : 'overview';
  const query = {};
  if (qPart) {
    new URLSearchParams(qPart).forEach((v, k) => {
      query[k] = v;
    });
  }
  return { route, query };
}

export function navigate(route, query = {}) {
  const qs = new URLSearchParams(query).toString();
  window.location.hash = `#/${route}${qs ? `?${qs}` : ''}`;
}

function renderLogin() {
  stopPolling();
  clear(pendingRoot);
  clear(appRoot);

  let errorBox = el('div', { className: 'banner banner-err', style: { display: 'none' } });
  const userInput = el('input', {
    className: 'input',
    type: 'text',
    name: 'username',
    value: 'admin',
    autocomplete: 'username',
    required: true,
  });
  const passInput = el('input', {
    className: 'input',
    type: 'password',
    name: 'password',
    placeholder: '输入管理员密码',
    autocomplete: 'current-password',
    required: true,
  });
  const submitBtn = el('button', { type: 'submit', className: 'btn btn-primary', style: { width: '100%' } }, '登录控制台');

  const form = el('form', {
    className: 'login-form',
    onSubmit: async (e) => {
      e.preventDefault();
      errorBox.style.display = 'none';
      submitBtn.disabled = true;
      submitBtn.textContent = '验证中...';
      try {
        const res = await api.login(userInput.value.trim() || 'admin', passInput.value);
        state.user = res.username || 'admin';
        await store.refresh();
        startPolling();
        renderApp();
      } catch (err) {
        errorBox.textContent = err.status === 429 ? '尝试次数过多，请稍后再试' : '用户名或密码不正确';
        errorBox.style.display = 'flex';
      } finally {
        submitBtn.disabled = false;
        submitBtn.textContent = '登录控制台';
      }
    },
  },
    errorBox,
    el('div', { className: 'field' },
      el('label', { className: 'field-label', for: 'login-user' }, '管理员账号'),
      Object.assign(userInput, { id: 'login-user' })
    ),
    el('div', { className: 'field' },
      el('label', { className: 'field-label', for: 'login-pass' }, '访问密码'),
      Object.assign(passInput, { id: 'login-pass' })
    ),
    submitBtn
  );

  appRoot.appendChild(
    el('div', { className: 'login-shell' },
      el('div', { className: 'login-card' },
        el('div', { className: 'login-head' },
          el('div', { className: 'brand-mark' }, icon('gatehouse', 18)),
          el('div', null,
            el('h1', { className: 'login-title' }, '门禁网关'),
            el('p', { className: 'login-sub' }, 'Precinct Gatehouse 控制面板')
          )
        ),
        form
      )
    )
  );
  passInput.focus();
}

function buildPendingBar() {
  clear(pendingRoot);
  const dirty = store.dirtySections();
  if (!store.problems.length) clearFieldErrors(document);
  if (!dirty.length && !store.conflict && !store.buildError && !store.problems.length) return;

  const chips = dirty.map((sec) => el('span', { className: 'pill pill-brass' }, sectionLabel(sec)));

  const extraNotice = [];
  if (store.conflict) {
    extraNotice.push(
      el('div', { className: 'banner banner-warn' },
        el('div', { className: 'banner-main' },
          icon('alert', 16),
          el('span', null, store.conflict.message)
        ),
        el('button', {
          type: 'button',
          className: 'btn btn-sm',
          onClick: async () => {
            await store.reloadAfterConflict();
            renderPage();
          },
        }, '重新加载最新配置')
      )
    );
  }
  if (store.buildError) {
    extraNotice.push(
      el('div', { className: 'banner banner-err' },
        el('div', { className: 'banner-main' },
          icon('alert', 16),
          el('span', null, store.buildError)
        )
      )
    );
  }
  if (store.problems.length) {
    const unmapped = applyFieldProblems(store.problems, document);
    const list = unmapped.length ? unmapped : store.problems;
    extraNotice.push(
      el('div', { className: 'banner banner-err' },
        el('div', { className: 'banner-main' },
          icon('alert', 16),
          el('div', null,
            el('div', { style: { fontWeight: 500 } }, '配置校验未通过'),
            ...list.map((p) => el('div', { className: 'mono', style: { fontSize: '12px', marginTop: '2px' } }, p))
          )
        )
      )
    );
  }

  const bar = el('div', { className: 'pending-bar', role: 'region', 'aria-label': '待应用配置修改' },
    ...extraNotice,
    el('div', { className: 'pending-bar-row' },
      el('div', { className: 'pending-bar-info' },
        el('strong', null, `${dirty.length} 项配置修改尚未生效`),
        ...chips
      ),
      el('div', { className: 'pending-bar-actions' },
        el('button', {
          type: 'button',
          className: 'btn btn-ghost btn-sm',
          disabled: store.applying,
          onClick: () => {
            store.discardAll();
            renderPage();
            toast('已放弃未应用的草稿', 'warn');
          },
        }, '放弃修改'),
        el('button', {
          type: 'button',
          className: 'btn btn-sm',
          disabled: store.applying,
          onClick: () => openDraftPreview(),
        }, '预览改动'),
        el('button', {
          type: 'button',
          className: 'btn btn-primary btn-sm',
          disabled: store.applying || !dirty.length,
          onClick: async () => {
            try {
              await store.applyAll();
              renderPage();
            } catch {
              // errors already placed on store
            }
          },
        }, store.applying ? '正在热重载...' : '应用并热重载')
      )
    )
  );
  pendingRoot.appendChild(bar);
}

function openDraftPreview() {
  const dirty = store.dirtySections();
  const blocks = dirty.map((sec) =>
    el('div', { className: 'field' },
      el('div', { className: 'field-label' }, sectionLabel(sec)),
      el('pre', { className: 'try-pre' }, JSON.stringify(store.drafts[sec], null, 2))
    )
  );
  openModal({
    title: '待应用配置预览',
    wide: true,
    body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '14px' } }, ...blocks),
    footer: [
      el('button', { type: 'button', className: 'btn', onClick: closeModal }, '关闭'),
    ],
  });
}

let pageHost = null;
let alertsHost = null;
let pillHost = null;
let sidebarNavHost = null;
let titleHost = null;

function renderTopbarStatus() {
  if (!pillHost) return;
  clear(pillHost);
  const cfg = store.server;
  if (!cfg) return;

  const hasErr = Boolean(cfg.last_error?.msg);
  const rr = cfg.restart_required || [];
  const hasWarn = !hasErr && rr.length > 0;

  const pill = el('button', {
    type: 'button',
    className: `config-pill${hasErr ? ' has-err' : hasWarn ? ' has-warn' : ''}`,
    title: hasErr ? `最近加载错误: ${cfg.last_error.msg}` : `配置文件: ${cfg.path || ''}`,
    onClick: () => navigate('settings'),
  },
    el('span', { className: 'status-dot' }),
    el('span', { className: 'mono num' }, `v${cfg.version || 1}`),
    el('span', { className: 'mono muted' }, (cfg.hash || '').slice(0, 7)),
    el('span', { className: 'config-pill-source muted' }, sourceLabel(cfg.source))
  );
  pillHost.appendChild(pill);

  if (alertsHost) {
    clear(alertsHost);
    if (hasErr) {
      alertsHost.appendChild(
        el('div', { className: 'banner banner-err' },
          el('div', { className: 'banner-main' },
            icon('alert', 16),
            el('div', null,
              el('strong', null, '磁盘配置重载失败，当前继续使用上一有效版本'),
              el('div', { className: 'mono', style: { fontSize: '12px', marginTop: '2px' } }, cfg.last_error.msg)
            )
          ),
          el('button', { type: 'button', className: 'btn btn-sm', onClick: () => navigate('settings') }, '前往排查')
        )
      );
    }
    if (rr.length) {
      alertsHost.appendChild(
        el('div', { className: 'banner banner-warn' },
          el('div', { className: 'banner-main' },
            icon('alert', 16),
            el('span', null, `检测到需要重启进程才能生效的字段已更新：${rr.join('、')}`)
          )
        )
      );
    }
  }
}

function renderSidebarNav() {
  if (!sidebarNavHost) return;
  clear(sidebarNavHost);
  NAV_GROUPS.forEach((grp) => {
    const groupEl = el('div', { className: 'nav-group' },
      el('div', { className: 'nav-group-label' }, grp.label)
    );
    grp.items.forEach((item) => {
      const secs = Array.isArray(item.sec) ? item.sec : item.sec ? [item.sec] : [];
      const dirty = secs.some((s) => store.isDirty(s));
      const link = el('a', {
        href: `#/${item.id}`,
        className: `nav-item${state.route === item.id ? ' active' : ''}`,
        onClick: () => {
          state.sidebarOpen = false;
          updateMobileNav();
        },
      },
        icon(item.ic, 17),
        el('span', null, item.title),
        dirty ? el('span', { className: 'nav-dirty-dot', title: '存在待应用修改' }) : null
      );
      groupEl.appendChild(link);
    });
    sidebarNavHost.appendChild(groupEl);
  });
}

function updateMobileNav() {
  const sb = document.getElementById('app-sidebar');
  const bd = document.getElementById('app-sidebar-bd');
  if (sb) sb.classList.toggle('open', state.sidebarOpen);
  if (bd) bd.classList.toggle('open', state.sidebarOpen);
}

function renderShell() {
  clear(appRoot);
  sidebarNavHost = el('nav', { className: 'sidebar-nav', 'aria-label': '主导航' });
  pillHost = el('div');
  alertsHost = el('div', { className: 'global-alerts' });
  pageHost = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  titleHost = el('h2', { className: 'page-title' }, ROUTE_TITLES[state.route] || '');

  const modeVal = store.server?.raw?.mode || 'team';
  const listenVal = store.server?.raw?.listen || '-';

  const sidebar = el('aside', { id: 'app-sidebar', className: 'sidebar' },
    el('div', { className: 'sidebar-brand' },
      el('div', { className: 'brand-mark' }, icon('gatehouse', 17)),
      el('div', { className: 'brand-text' },
        el('span', { className: 'brand-title' }, '门禁网关'),
        el('span', { className: 'brand-sub' }, 'Precinct Gatehouse')
      )
    ),
    sidebarNavHost,
    el('div', { className: 'sidebar-footer' },
      el('div', { className: 'sidebar-footer-row' },
        el('span', null, '运行模式'),
        el('span', { className: 'mono' }, modeVal)
      ),
      el('div', { className: 'sidebar-footer-row' },
        el('span', null, '数据面监听'),
        el('span', { className: 'mono' }, listenVal)
      )
    )
  );

  const backdrop = el('div', {
    id: 'app-sidebar-bd',
    className: 'sidebar-backdrop',
    onClick: () => {
      state.sidebarOpen = false;
      updateMobileNav();
    },
  });

  const topbar = el('header', { className: 'topbar' },
    el('div', { className: 'topbar-left' },
      el('button', {
        type: 'button',
        className: 'nav-toggle',
        'aria-label': '展开导航菜单',
        onClick: () => {
          state.sidebarOpen = !state.sidebarOpen;
          updateMobileNav();
        },
      }, icon('menu', 18)),
      titleHost
    ),
    el('div', { className: 'topbar-right' },
      pillHost,
      el('button', {
        type: 'button',
        className: 'icon-btn',
        'aria-label': '切换明暗主题',
        title: effectiveThemeIsDark() ? '切换浅色主题' : '切换深色主题',
        onClick: toggleTheme,
      }, icon(effectiveThemeIsDark() ? 'sun' : 'moon', 17)),
      el('span', { className: 'user-chip' }, state.user || 'admin'),
      el('button', {
        type: 'button',
        className: 'icon-btn',
        'aria-label': '退出登录',
        title: '退出登录',
        onClick: async () => {
          try {
            await api.logout();
          } catch {
            // ignore
          }
          state.user = null;
          store.discardAll();
          renderLogin();
        },
      }, icon('logout', 16))
    )
  );

  const main = el('main', { className: 'main-col' },
    topbar,
    el('div', { className: 'page-wrap' }, alertsHost, pageHost)
  );

  appRoot.appendChild(el('div', { className: 'app-shell' }, sidebar, backdrop, main));
  renderSidebarNav();
  renderTopbarStatus();
  buildPendingBar();
  renderPage();
}

let pageCleanups = [];
function renderPage() {
  if (!pageHost) return;
  pageCleanups.forEach((fn) => fn());
  pageCleanups = [];
  clear(pageHost);
  if (titleHost) titleHost.textContent = ROUTE_TITLES[state.route] || '';
  renderSidebarNav();

  const ctx = {
    query: state.query,
    navigate,
    registerCleanup: (fn) => pageCleanups.push(fn),
    rerender: () => renderPage(),
  };

  switch (state.route) {
    case 'overview':
      renderOverview(pageHost, ctx);
      break;
    case 'keys':
      renderKeys(pageHost, ctx);
      break;
    case 'channels':
      renderChannels(pageHost, ctx);
      break;
    case 'routing':
      renderRouting(pageHost, ctx);
      break;
    case 'tiered':
      renderTiered(pageHost, ctx);
      break;
    case 'redaction':
      renderRedaction(pageHost, ctx);
      break;
    case 'logs':
      renderLogs(pageHost, ctx);
      break;
    case 'usage':
      renderUsage(pageHost, ctx);
      break;
    case 'settings':
      renderSettings(pageHost, ctx);
      break;
    default:
      renderOverview(pageHost, ctx);
  }
}

function renderApp() {
  const { route, query } = parseHash();
  state.route = route;
  state.query = query;
  renderShell();
}

function startPolling() {
  stopPolling();
  state.pollTimer = setInterval(async () => {
    if (!state.user) return;
    try {
      await store.refresh();
    } catch {
      // handled by unauthorizedHandler on 401
    }
  }, 15000);
}

function stopPolling() {
  if (state.pollTimer) {
    clearInterval(state.pollTimer);
    state.pollTimer = null;
  }
}

store.subscribe(() => {
  renderTopbarStatus();
  renderSidebarNav();
  buildPendingBar();
});

window.addEventListener('hashchange', () => {
  if (!state.user) return;
  const { route, query } = parseHash();
  state.route = route;
  state.query = query;
  renderPage();
});

setUnauthorizedHandler(() => {
  if (state.user !== null) {
    state.user = null;
    store.discardAll();
    renderLogin();
  }
});

async function boot() {
  applyTheme(state.theme);
  const { route, query } = parseHash();
  state.route = route;
  state.query = query;
  try {
    const me = await api.me();
    state.user = me.username || 'admin';
    await store.refresh();
    startPolling();
    renderShell();
  } catch {
    state.user = null;
    renderLogin();
  }
}

boot();
