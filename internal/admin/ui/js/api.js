export class ApiError extends Error {
  constructor(status, payload = {}) {
    const code = payload.error || (status === 401 ? 'unauthorized' : 'request_failed');
    const msg = payload.message || payload.error || `HTTP ${status}`;
    super(msg);
    this.status = status;
    this.code = code;
    this.problems = Array.isArray(payload.problems) ? payload.problems : [];
    this.currentHash = payload.current_hash || '';
    this.payload = payload;
  }
}

let unauthorizedHandler = null;

export function setUnauthorizedHandler(fn) {
  unauthorizedHandler = fn;
}

export async function request(method, path, body = undefined, opts = {}) {
  const headers = {};
  const upper = method.toUpperCase();
  if (upper !== 'GET' && upper !== 'HEAD') {
    headers['X-Gatehouse-Admin'] = '1';
  }
  if (opts && opts.background) {
    headers['X-Gatehouse-Background'] = '1';
  }
  const init = {
    method: upper,
    credentials: 'same-origin',
    headers,
  };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  let payload = {};
  const ct = res.headers.get('content-type') || '';
  if (ct.includes('application/json')) {
    try {
      payload = await res.json();
    } catch {
      payload = {};
    }
  } else {
    const txt = await res.text();
    if (txt) payload = { message: txt };
  }

  if (res.status === 401 && !opts.skipAuthRedirect) {
    if (unauthorizedHandler) unauthorizedHandler();
    throw new ApiError(401, payload);
  }
  if (!res.ok) {
    throw new ApiError(res.status, payload);
  }
  return payload;
}

function buildQuery(params = {}) {
  const qs = new URLSearchParams();
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined && v !== null && v !== '') qs.set(k, String(v));
  });
  const s = qs.toString();
  return s ? `?${s}` : '';
}

export const api = {
  me: () => request('GET', '/admin/api/me', undefined, { skipAuthRedirect: true }),
  login: (username, password) =>
    request('POST', '/admin/api/login', { username, password }, { skipAuthRedirect: true }),
  setupStatus: () => request('GET', '/admin/api/setup', undefined, { skipAuthRedirect: true }),
  setup: (code, username, password) =>
    request('POST', '/admin/api/setup', { code, username, password }, { skipAuthRedirect: true }),
  getBranding: () => request('GET', '/admin/api/branding', undefined, { skipAuthRedirect: true }),
  getBrandingSettings: () => request('GET', '/admin/api/branding/settings'),
  putBrandingSettings: (data) => request('PUT', '/admin/api/branding/settings', data),
  putBrandingLogo: (dataUrl) => request('PUT', '/admin/api/branding/logo', { data_url: dataUrl }),
  deleteBrandingLogo: () => request('DELETE', '/admin/api/branding/logo', {}),
  stats: (params) => request('GET', `/admin/api/stats${buildQuery(params)}`),
  resetKeyUsage: (id) => request('POST', `/admin/api/keys/${encodeURIComponent(id)}/reset-usage`, {}),
  logout: () => request('POST', '/admin/api/logout', {}),
  changePassword: (oldPwd, newPwd) =>
    request('POST', '/admin/api/password', { old: oldPwd, new: newPwd }),

  flow: (range = '24h') => request('GET', `/admin/api/flow${buildQuery({ range })}`),
  overview: () => request('GET', '/admin/api/overview'),

  listKeys: () => request('GET', '/admin/api/keys'),
  createKey: (data) => request('POST', '/admin/api/keys', data),
  getKeySecret: (id) => request('GET', `/admin/api/keys/${encodeURIComponent(id)}/secret`),
  updateKey: (id, data) => request('PATCH', `/admin/api/keys/${encodeURIComponent(id)}`, data),
  resetKey: (id) => request('POST', `/admin/api/keys/${encodeURIComponent(id)}/reset`, {}),
  deleteKey: (id) => request('DELETE', `/admin/api/keys/${encodeURIComponent(id)}`),

  logs: (params) => request('GET', `/admin/api/logs${buildQuery(params)}`),
  usage: (params) => request('GET', `/admin/api/usage${buildQuery(params)}`),
  channels: () => request('GET', '/admin/api/channels'),
  redaction: () => request('GET', '/admin/api/redaction'),
  redactionTry: (data) => request('POST', '/admin/api/redaction/try', data),
  astrlinkStatus: () => request('GET', '/admin/api/redaction/astrlink'),
  astrlinkInstall: (data) => request('POST', '/admin/api/redaction/astrlink/install', data),

  getConfig: (opts) => request('GET', '/admin/api/config', undefined, opts),
  putSections: (baseHash, sections) =>
    request('PUT', '/admin/api/config/sections', {
      base_hash: baseHash,
      sections,
    }),
  putSection: (section, baseHash, data) =>
    request('PUT', `/admin/api/config/sections/${encodeURIComponent(section)}`, {
      base_hash: baseHash,
      data,
    }),
  getConfigYAML: () => request('GET', '/admin/api/config/yaml'),
  putConfigYAML: (baseHash, yaml) =>
    request('PUT', '/admin/api/config/yaml', { base_hash: baseHash, yaml }),
  validateConfig: (payload) => request('POST', '/admin/api/config/validate', payload),
  reloadConfig: () => request('POST', '/admin/api/config/reload', {}),
  configHistory: () => request('GET', '/admin/api/config/history'),
  configHistoryFile: (name) =>
    request('GET', `/admin/api/config/history/${encodeURIComponent(name)}`),
  rollbackConfig: (name) => request('POST', '/admin/api/config/rollback', { name }),

  testProvider: (provider, model) =>
    request('POST', '/admin/api/providers/test', { provider, model }),
  putProviderKey: (name, key, baseHash = '') =>
    request('PUT', `/admin/api/providers/${encodeURIComponent(name)}/key`, {
      key,
      base_hash: baseHash,
    }),
  deleteProviderKey: (name) =>
    request('DELETE', `/admin/api/providers/${encodeURIComponent(name)}/key`),

  tieredStatus: (opts) => request('GET', '/admin/api/tiered', undefined, opts),
  tieredStarters: (provider, opts) =>
    request('GET', `/admin/api/tiered/${encodeURIComponent(provider)}/starters`, undefined, opts),
  pinTieredStarter: (provider, data) =>
    request('POST', `/admin/api/tiered/${encodeURIComponent(provider)}/starters/pin`, data),
  deleteTieredStarter: (provider, hash) =>
    request('DELETE', `/admin/api/tiered/${encodeURIComponent(provider)}/starters/${encodeURIComponent(hash)}`),
  tieredCheckpoints: (provider, opts) =>
    request('GET', `/admin/api/tiered/${encodeURIComponent(provider)}/checkpoints`, undefined, opts),
  tieredDeleteCheckpoint: (provider, id) =>
    request('DELETE', `/admin/api/tiered/${encodeURIComponent(provider)}/checkpoints/${encodeURIComponent(id)}`),
  tieredClearCheckpoints: (provider) =>
    request('DELETE', `/admin/api/tiered/${encodeURIComponent(provider)}/checkpoints?all=1`),
  warmTieredStarters: (provider, data = {}) =>
    request('POST', `/admin/api/tiered/${encodeURIComponent(provider)}/starters/warm`, data),
  resetTieredShadow: (provider) =>
    request('POST', `/admin/api/tiered/${encodeURIComponent(provider)}/shadow/reset`, {}),

  updaterStatus: (opts) => request('GET', '/admin/api/updater', undefined, opts),
  updaterCheck: () => request('POST', '/admin/api/updater/check', {}),
  updaterApply: (data = {}) => request('POST', '/admin/api/updater/apply', data),
  updaterRollback: (data = {}) => request('POST', '/admin/api/updater/rollback', data),
  putUpdaterSecret: (kind, value, baseHash = '') =>
    request('PUT', '/admin/api/updater/secret', { kind, value, base_hash: baseHash }),
  deleteUpdaterSecret: (kind, baseHash = '') =>
    request('DELETE', `/admin/api/updater/secret/${encodeURIComponent(kind)}`, { base_hash: baseHash }),
};
