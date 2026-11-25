import { fmtDateTime, fmtNum } from '/admin/js/ui.js';

export const TEMPLATE_VARS = [
  { token: '{{DISPLAY_NAME}}', label: '网关名称', desc: '首页配置的显示名称' },
  { token: '{{BASE_URL}}', label: '网关根地址', desc: '不含 /v1，适用于 Claude Code' },
  { token: '{{OPENAI_BASE_URL}}', label: 'OpenAI 地址', desc: '含 /v1，适用于 OpenAI / Codex' },
  { token: '{{API_KEY}}', label: '访问密钥', desc: '完整 sk-... 密钥（旧密钥显示占位符）' },
  { token: '{{KEY_NAME}}', label: '密钥名称', desc: '访问密钥的名称' },
  { token: '{{KEY_PREFIX}}', label: '密钥前缀', desc: 'sk-... 前缀' },
  { token: '{{DEFAULT_MODEL}}', label: '推荐模型', desc: '首个允许模型 / 路由组或 auto' },
  { token: '{{MODELS}}', label: '可用范围', desc: '允许模型与路由组列表' },
  { token: '{{QUOTA}}', label: '配额说明', desc: 'RPM / RPD / 并发 / Token 配额' },
  { token: '{{EXPIRES_AT}}', label: '有效期', desc: '到期时间或“长期有效”' },
  { token: '{{NOTE}}', label: '备注', desc: '密钥备注信息' },
];

export const DEFAULT_DIST_TEMPLATE = `=== {{DISPLAY_NAME}} 接入配置 ===
密钥名称：{{KEY_NAME}}
有效期：{{EXPIRES_AT}}
可用模型：{{MODELS}}
配额限制：{{QUOTA}}

1. 通用 OpenAI 兼容客户端（Chat Completions / 桌面端）
Base URL：{{OPENAI_BASE_URL}}
API Key：{{API_KEY}}
推荐模型：{{DEFAULT_MODEL}}

2. Claude Code（Anthropic Messages）
export ANTHROPIC_BASE_URL="{{BASE_URL}}"
export ANTHROPIC_API_KEY="{{API_KEY}}"

3. Codex CLI（OpenAI Responses）
# ~/.codex/config.toml
base_url = "{{OPENAI_BASE_URL}}"
wire_api = "responses"
# 环境变量
export OPENAI_API_KEY="{{API_KEY}}"`;

function stripTrailingV1(urlStr) {
  let u = String(urlStr || '').trim().replace(/\/+$/, '');
  if (u.toLowerCase().endsWith('/v1')) {
    u = u.slice(0, -3).replace(/\/+$/, '');
  }
  return u;
}

function parseListenHostPort(listenAddr) {
  const s = String(listenAddr || '').trim();
  if (!s) return { host: '', port: '7788' };
  if (s.startsWith('[')) {
    const close = s.indexOf(']');
    if (close > 0) {
      const host = s.slice(1, close);
      const rest = s.slice(close + 1);
      const port = rest.startsWith(':') ? rest.slice(1) : '7788';
      return { host, port: port || '7788' };
    }
  }
  const idx = s.lastIndexOf(':');
  if (idx < 0) return { host: s, port: '7788' };
  return { host: s.slice(0, idx), port: s.slice(idx + 1) || '7788' };
}

export function resolveGatewayURLs(publicUrl, listenAddr, loc = typeof window !== 'undefined' ? window.location : null) {
  const { host: rawHost, port } = parseListenHostPort(listenAddr);
  let host = rawHost;
  if (!host || host === '0.0.0.0' || host === '::') {
    host = loc?.hostname || '127.0.0.1';
  }
  if (host.includes(':') && !host.startsWith('[')) {
    host = `[${host}]`;
  }
  const inferredBaseUrl = `http://${host}:${port}`;
  const cleanPublic = stripTrailingV1(publicUrl);
  const baseUrl = cleanPublic || inferredBaseUrl;
  return {
    baseUrl,
    openaiBaseUrl: `${baseUrl}/v1`,
    inferredBaseUrl,
  };
}

export function resolveKeyModelInfo(keyObj = {}, rawCfg = {}) {
  const models = Array.isArray(keyObj.models) ? keyObj.models.filter(Boolean) : [];
  const groups = Array.isArray(keyObj.groups) ? keyObj.groups.filter(Boolean) : [];
  const scopes = [...groups, ...models];

  let defaultModel = 'auto';
  if (groups.length > 0) {
    defaultModel = groups[0];
  } else if (models.length > 0) {
    defaultModel = models[0];
  } else if (rawCfg?.default_group) {
    defaultModel = rawCfg.default_group;
  } else if (Array.isArray(rawCfg?.providers) && rawCfg.providers.length > 0) {
    const firstProv = rawCfg.providers.find((p) => Array.isArray(p.models) && p.models.length > 0);
    if (firstProv) {
      defaultModel = `${firstProv.name}/${firstProv.models[0]}`;
    }
  }

  let modelsText = '全部可用模型';
  if (scopes.length > 0) {
    const parts = [];
    if (groups.length > 0) parts.push(`路由组: ${groups.join(', ')}`);
    if (models.length > 0) parts.push(`模型: ${models.join(', ')}`);
    modelsText = parts.join('；');
  }

  return { defaultModel, modelsText };
}

function fmtTokenLimit(n) {
  const v = Number(n || 0);
  if (v >= 1e8) return `${+(v / 1e8).toFixed(2)} 亿`;
  if (v >= 1e4) return `${+(v / 1e4).toFixed(1)} 万`;
  return fmtNum(v);
}

export function formatKeyQuota(keyObj = {}) {
  const parts = [];
  if (Number(keyObj.rpm) > 0) parts.push(`RPM ${fmtNum(keyObj.rpm)}`);
  if (Number(keyObj.rpd) > 0) parts.push(`日请求 ${fmtNum(keyObj.rpd)}`);
  if (Number(keyObj.concurrency) > 0) parts.push(`并发 ${fmtNum(keyObj.concurrency)}`);
  if (Number(keyObj.tokens_daily) > 0) parts.push(`日 Token ${fmtTokenLimit(keyObj.tokens_daily)}`);
  if (Number(keyObj.tokens_monthly) > 0) parts.push(`月 Token ${fmtTokenLimit(keyObj.tokens_monthly)}`);
  if (Number(keyObj.tokens_total) > 0) parts.push(`总 Token ${fmtTokenLimit(keyObj.tokens_total)}`);
  return parts.length ? parts.join(' / ') : '不限';
}

export function renderDistributionTemplate({
  key = {},
  plainKey = '',
  brandingState = {},
  rawConfig = {},
  templateOverride = null,
}) {
  const template = (templateOverride !== null && templateOverride !== undefined
    ? String(templateOverride)
    : String(brandingState.dist_template || '')) || DEFAULT_DIST_TEMPLATE;

  const { baseUrl, openaiBaseUrl } = resolveGatewayURLs(brandingState.public_url, rawConfig.listen);
  const { defaultModel, modelsText } = resolveKeyModelInfo(key, rawConfig);
  const prefix = key.key_prefix ? `${key.key_prefix}...` : (key.id || '');
  const apiKeyVal = plainKey || `<请替换为完整密钥 (前缀: ${prefix})>`;
  const expiresText = Number(key.expires_at) > 0 ? fmtDateTime(Number(key.expires_at)) : '长期有效';

  const vars = {
    DISPLAY_NAME: brandingState.display_name || 'Precinct Gatehouse',
    BASE_URL: baseUrl,
    OPENAI_BASE_URL: openaiBaseUrl,
    API_KEY: apiKeyVal,
    KEY_NAME: key.name || '-',
    KEY_PREFIX: prefix || '-',
    DEFAULT_MODEL: defaultModel,
    MODELS: modelsText,
    QUOTA: formatKeyQuota(key),
    EXPIRES_AT: expiresText,
    NOTE: key.note || '-',
  };

  return template.replace(/\{\{([A-Z0-9_]+)\}\}/g, (match, name) => (
    Object.prototype.hasOwnProperty.call(vars, name) ? vars[name] : match
  ));
}

export async function copyToClipboard(text) {
  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall back to execCommand below when running on non-HTTPS or restricted origins.
    }
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-9999px';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, text.length);
    const ok = document.execCommand('copy');
    ta.remove();
    return Boolean(ok);
  } catch {
    return false;
  }
}
