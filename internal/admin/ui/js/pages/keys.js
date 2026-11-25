import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  closeDrawer,
  closeModal,
  confirmModal,
  createSwitch,
  createTagInput,
  el,
  fmtDateTime,
  fmtNum,
  openDrawer,
  openModal,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';
import { branding } from '/admin/js/branding.js';
import { copyToClipboard, renderDistributionTemplate } from '/admin/js/template.js';

function meterRow(label, used, limit) {
  const u = Number(used || 0);
  const l = Number(limit || 0);
  const ratio = l > 0 ? Math.min(100, Math.round((u / l) * 100)) : 0;
  const tone = l > 0 && u >= l ? 'danger' : l > 0 && u >= l * 0.8 ? 'warn' : '';
  return el('div', { className: 'meter-line' },
    el('div', { className: 'meter-meta' },
      el('span', null, label),
      el('span', { className: 'num' }, l > 0 ? `${fmtNum(u)} / ${fmtNum(l)}` : `${fmtNum(u)} / 不限`)
    ),
    l > 0
      ? el('div', { className: 'meter-track' },
        el('div', { className: `meter-fill ${tone}`, style: { width: `${ratio}%` } })
      )
      : null
  );
}

const TOKEN_PRESETS = [
  { value: 0, label: '不限' },
  { value: 100_000, label: '10 万' },
  { value: 1_000_000, label: '100 万' },
  { value: 10_000_000, label: '1000 万' },
  { value: 100_000_000, label: '1 亿' },
];

function fmtTokens(n) {
  const v = Number(n || 0);
  if (v >= 1e8) return `${+(v / 1e8).toFixed(2)} 亿`;
  if (v >= 1e4) return `${+(v / 1e4).toFixed(1)} 万`;
  return fmtNum(v);
}

// Option-style quota picker: preset chips plus a custom amount, so nobody has
// to type raw token counts for the common cases.
function quotaPicker(label, hint, initial) {
  let value = Math.max(0, Number(initial || 0));
  const custom = el('input', {
    className: 'input num input-sm',
    type: 'number',
    min: 0,
    step: 1000,
    placeholder: '自定义数量',
    value: TOKEN_PRESETS.some((p) => p.value === value) ? '' : String(value),
    style: { width: '140px' },
  });
  const chips = el('div', { className: 'quota-presets' });
  const paint = () => {
    clear(chips);
    TOKEN_PRESETS.forEach((p) => {
      chips.appendChild(el('button', {
        type: 'button',
        className: 'btn btn-sm',
        'aria-pressed': p.value === value ? 'true' : 'false',
        onClick: () => {
          value = p.value;
          custom.value = '';
          paint();
        },
      }, p.label));
    });
    chips.appendChild(custom);
  };
  custom.addEventListener('input', () => {
    const n = Math.trunc(Number(custom.value));
    value = Number.isFinite(n) && n > 0 ? n : 0;
    paint();
    custom.focus();
  });
  paint();
  return {
    node: el('div', { className: 'field' },
      el('label', { className: 'field-label' }, label),
      chips,
      el('p', { className: 'field-hint' }, hint)
    ),
    get: () => value,
  };
}

function quotaMeter(label, used, limit) {
  const u = Number(used || 0);
  const l = Number(limit || 0);
  const ratio = l > 0 ? Math.min(100, (u / l) * 100) : 0;
  const tone = l > 0 && u >= l ? 'crit' : l > 0 && u >= l * 0.8 ? 'warn' : '';
  return el('div', { className: 'quota-meter' },
    el('div', { className: 'quota-meter-head' },
      el('span', null, label),
      el('b', null, l > 0 ? `${fmtTokens(u)} / ${fmtTokens(l)}` : `${fmtTokens(u)} · 不限`)
    ),
    l > 0 ? el('div', { className: 'quota-track' }, el('div', { className: `quota-fill ${tone}`, style: { width: `${ratio}%` } })) : null
  );
}

const sessionSecrets = new Map();

export function clearKeySecretCache() {
  sessionSecrets.clear();
}

function effectiveRaw() {
  const base = store.server?.raw || {};
  const routing = store.getSection('routing') || {};
  return {
    ...base,
    providers: store.getSection('providers') || base.providers || [],
    default_group: routing.default_group ?? base.default_group ?? '',
    routing_groups: routing.routing_groups ?? base.routing_groups ?? {},
    model_aliases: store.getSection('aliases') || base.model_aliases || [],
  };
}

function showRenderedTemplateModal(keyInfo, text) {
  openModal({
    title: `配置模板 / ${keyInfo?.name || ''}`,
    wide: true,
    body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
      el('p', { className: 'muted', style: { margin: 0, fontSize: '13px' } }, '请复制下方配置并发送给使用方：'),
      el('pre', { className: 'try-pre', style: { maxHeight: '420px' } }, text)
    ),
    footer: [
      el('button', {
        type: 'button',
        className: 'btn btn-primary',
        onClick: async () => {
          const ok = await copyToClipboard(text);
          if (ok) {
            toast('配置模板已复制到剪贴板', 'ok');
            closeModal();
          } else {
            toast('浏览器禁止自动复制，请手动全选文本框内容复制', 'warn');
          }
        },
      }, icon('copy', 14), '复制配置模板'),
      el('button', { type: 'button', className: 'btn', onClick: closeModal }, '关闭'),
    ],
  });
}

function cacheSessionSecret(keyInfo, plainKey) {
  if (!keyInfo?.id || !plainKey) return;
  sessionSecrets.set(keyInfo.id, {
    key: plainKey,
    prefix: keyInfo.key_prefix || plainKey.slice(0, 9),
  });
}

function getCachedSessionSecret(keyObj) {
  if (!keyObj?.id) return '';
  const entry = sessionSecrets.get(keyObj.id);
  if (!entry) return '';
  if (keyObj.key_prefix && entry.prefix && entry.prefix !== keyObj.key_prefix) {
    sessionSecrets.delete(keyObj.id);
    return '';
  }
  return entry.key;
}

async function copyKeyConfig(keyObj, triggerBtn = null, knownPlain = '') {
  if (triggerBtn) triggerBtn.disabled = true;
  try {
    await branding.ensureSettingsLoaded().catch(() => {});
    let plainKey = knownPlain || '';
    let isPlaceholder = false;
    if (!plainKey && keyObj?.id && keyObj?.has_secret) {
      try {
        const res = await api.getKeySecret(keyObj.id);
        if (res?.has_secret && res.key) {
          plainKey = res.key;
          cacheSessionSecret(keyObj, plainKey);
        }
      } catch (e) {
        if (e.status !== 401) {
          toast(`读取密钥明文失败，已回退占位符: ${e.message}`, 'warn');
        }
      }
    }
    if (!plainKey) {
      plainKey = getCachedSessionSecret(keyObj);
    }
    if (!plainKey) {
      isPlaceholder = true;
    }

    const text = renderDistributionTemplate({
      key: keyObj,
      plainKey,
      brandingState: branding.state,
      rawConfig: effectiveRaw(),
    });
    const copied = await copyToClipboard(text);
    if (copied) {
      if (isPlaceholder) {
        toast('已复制配置模板（该旧密钥未保存明文，请手动替换占位符或轮换密钥后复制）', 'warn', 5000);
      } else {
        toast(`已复制 “${keyObj?.name || '密钥'}” 的配置模板`, 'ok');
      }
    } else {
      showRenderedTemplateModal(keyObj, text);
    }
  } finally {
    if (triggerBtn) triggerBtn.disabled = false;
  }
}

export function modelAndGroupSuggestions(rawOverride = null) {
  const raw = rawOverride || effectiveRaw();
  const models = [];
  (raw.providers || []).forEach((p) => {
    (p.models || []).forEach((m) => models.push(`${p.name}/${m}`));
  });
  const groups = Object.keys(raw.routing_groups || {});
  return { models: Array.from(new Set(models)), groups };
}

function globMatch(pattern, value) {
  if (!pattern) return false;
  if (pattern === value) return true;
  const escaped = pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&');
  const reStr = '^' + escaped.replace(/\*/g, '[^/]*').replace(/\?/g, '[^/]') + '$';
  try {
    return new RegExp(reStr).test(value);
  } catch {
    return false;
  }
}

export function normalizeKeyScopes(models, groups, raw = {}) {
  const aliases = raw.model_aliases || [];
  const routingGroups = raw.routing_groups || {};
  const defaultGroup = raw.default_group || '';
  const resolveAlias = (name) => {
    for (const a of aliases) {
      if (!a || !a.match || !a.use) continue;
      if (a.match === name || globMatch(a.match, name)) return a.use;
    }
    return name;
  };
  const outModels = [];
  const outGroups = (groups || []).map((g) => String(g || '').trim()).filter(Boolean);
  for (const rawModel of models || []) {
    const m = String(rawModel || '').trim();
    if (!m) continue;
    const target = resolveAlias(m);
    if (target === 'auto' && defaultGroup) {
      outGroups.push(defaultGroup);
    } else if (Object.prototype.hasOwnProperty.call(routingGroups, target)) {
      outGroups.push(target);
    } else {
      outModels.push(target);
    }
  }
  return {
    models: Array.from(new Set(outModels)),
    groups: Array.from(new Set(outGroups)),
  };
}

function showSecretModal(plainKey, keyInfo) {
  cacheSessionSecret(keyInfo, plainKey);
  openModal({
    title: '访问密钥已签发',
    body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
      el('div', { className: 'banner banner-warn' },
        el('div', { className: 'banner-main' },
          icon('alert', 16),
          el('span', null, '明文密钥仅在此窗口展示一次，请立即复制并妥善保存。')
        )
      ),
      el('div', { className: 'field' },
        el('span', { className: 'field-label' }, `密钥名称: ${keyInfo?.name || ''}`),
        el('div', { className: 'secret-box' }, plainKey)
      )
    ),
    footer: [
      el('button', {
        type: 'button',
        className: 'btn',
        onClick: async () => {
          const ok = await copyToClipboard(plainKey);
          toast(ok ? '已复制到剪贴板' : '请手动选中并复制密钥文本', ok ? 'ok' : 'warn');
        },
      }, icon('copy', 14), '复制密钥'),
      el('button', {
        type: 'button',
        className: 'btn',
        onClick: (e) => copyKeyConfig(keyInfo, e.currentTarget, plainKey),
      }, icon('brand', 14), '复制配置模板'),
      el('button', {
        type: 'button',
        className: 'btn btn-primary',
        onClick: closeModal,
      }, '我已保存'),
    ],
  });
}

function toDateInputValue(sec) {
  if (!sec) return '';
  const d = new Date(sec * 1000);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function renderKeys(container, ctx = {}) {
  let search = '';
  let keysList = [];

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } });
  container.appendChild(root);

  const load = async () => {
    try {
      const res = await api.listKeys();
      keysList = res.keys || [];
      paint();
    } catch (e) {
      if (e.status !== 401) toast(`读取密钥失败: ${e.message}`, 'err');
    }
  };

  const openKeyDrawer = (existing = null) => {
    const isEdit = Boolean(existing);
    const sug = modelAndGroupSuggestions();
    const formState = {
      name: existing?.name || '',
      models: [...(existing?.models || [])],
      groups: [...(existing?.groups || [])],
      rpm: Number(existing?.rpm || 0),
      rpd: Number(existing?.rpd || 0),
      concurrency: Number(existing?.concurrency || 0),
      expiresInput: toDateInputValue(existing?.expires_at || 0),
      note: existing?.note || '',
    };

    const errBanner = el('div', { className: 'banner banner-err', style: { display: 'none' } });
    const nameIn = el('input', { className: 'input', type: 'text', value: formState.name, placeholder: '例如 research-team-a', required: true });
    const rpmIn = el('input', { className: 'input num', type: 'number', min: 0, value: String(formState.rpm) });
    const rpdIn = el('input', { className: 'input num', type: 'number', min: 0, value: String(formState.rpd) });
    const concIn = el('input', { className: 'input num', type: 'number', min: 0, value: String(formState.concurrency) });
    const expIn = el('input', { className: 'input', type: 'datetime-local', value: formState.expiresInput });
    const qDaily = quotaPicker('每日 Token 配额', '按自然日统计输入 + 输出 Token，次日 0 点重置。', existing?.tokens_daily);
    const qMonthly = quotaPicker('每月 Token 配额', '按自然月统计，每月 1 日重置。', existing?.tokens_monthly);
    const qTotal = quotaPicker('总 Token 配额', '累计用量，不自动重置；用完后可在列表中清零或提高额度。', existing?.tokens_total);
    const noteIn = el('textarea', { className: 'textarea', value: formState.note, placeholder: '可选用途说明、负责人等', style: { minHeight: '72px' } });

    const submitBtn = el('button', { type: 'button', className: 'btn btn-primary' }, isEdit ? '保存更改' : '签发新密钥');

    const save = async () => {
      const name = nameIn.value.trim();
      if (!name) {
        errBanner.textContent = '请填写密钥名称';
        errBanner.style.display = 'flex';
        nameIn.focus();
        return;
      }
      let expiresAt = 0;
      if (expIn.value) {
        const ms = Date.parse(expIn.value);
        if (!Number.isNaN(ms)) expiresAt = Math.floor(ms / 1000);
      }
      const scoped = normalizeKeyScopes(formState.models, formState.groups, effectiveRaw());
      const payload = {
        name,
        models: scoped.models,
        groups: scoped.groups,
        rpm: Math.max(0, Number(rpmIn.value || 0)),
        rpd: Math.max(0, Number(rpdIn.value || 0)),
        concurrency: Math.max(0, Number(concIn.value || 0)),
        tokens_daily: qDaily.get(),
        tokens_monthly: qMonthly.get(),
        tokens_total: qTotal.get(),
        expires_at: expiresAt,
        note: noteIn.value.trim(),
      };
      submitBtn.disabled = true;
      errBanner.style.display = 'none';
      try {
        if (isEdit) {
          await api.updateKey(existing.id, payload);
          closeDrawer();
          toast('密钥配置已更新', 'ok');
          await load();
        } else {
          const res = await api.createKey(payload);
          closeDrawer();
          await load();
          showSecretModal(res.key, res.api_key || payload);
        }
      } catch (e) {
        errBanner.textContent = e.message || '操作失败';
        errBanner.style.display = 'flex';
      } finally {
        submitBtn.disabled = false;
      }
    };

    submitBtn.addEventListener('click', save);

    openDrawer({
      title: isEdit ? `编辑密钥 / ${existing.name}` : '签发新访问密钥',
      body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        errBanner,
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '密钥名称'),
          nameIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '允许模型（留空不限制）'),
          createTagInput({
            values: formState.models,
            placeholder: '选择或输入 provider/model，回车添加',
            suggestions: sug.models,
            onChange: (vals) => { formState.models = vals; },
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '允许路由组（留空使用系统全部权限）'),
          createTagInput({
            values: formState.groups,
            placeholder: '输入路由组名，回车添加',
            suggestions: sug.groups,
            onChange: (vals) => { formState.groups = vals; },
          })
        ),
        el('div', { className: 'field-row-3' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '每分钟请求 RPM'),
            rpmIn,
            el('p', { className: 'field-hint' }, '0 表示不限')
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '每日请求 RPD'),
            rpdIn,
            el('p', { className: 'field-hint' }, '0 表示不限')
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '最大并发'),
            concIn,
            el('p', { className: 'field-hint' }, '0 表示不限')
          )
        ),
        el('div', { className: 'sublist-head', style: { marginTop: '4px' } }, el('strong', null, 'Token 配额'),
          el('span', { className: 'muted', style: { fontSize: '12px' } }, '达到任一配额后新请求返回 429，进行中的请求不受影响')),
        qDaily.node,
        qMonthly.node,
        qTotal.node,
        isEdit
          ? el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '密钥明文'),
            el('div', { style: { display: 'flex', alignItems: 'center', gap: '10px' } },
              el('button', {
                type: 'button',
                className: 'btn btn-sm',
                onClick: async () => {
                  const ok = await confirmModal({
                    title: '轮换密钥',
                    message: `为 “${existing.name}” 生成新的密钥明文？旧密钥立即失效，正在使用它的客户端需要换成新密钥。`,
                    confirmText: '生成新密钥',
                    danger: true,
                  });
                  if (!ok) return;
                  try {
                    const res = await api.resetKey(existing.id);
                    closeDrawer();
                    await load();
                    showSecretModal(res.key, res.api_key || existing);
                  } catch (e) {
                    toast(`轮换失败: ${e.message}`, 'err');
                  }
                },
              }, icon('refresh', 13), '轮换密钥'),
              el('span', { className: 'muted', style: { fontSize: '12px' } }, '生成新明文，旧密钥立即失效')
            )
          )
          : null,
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '过期时间（可选，留空永不过期）'),
          el('div', { style: { display: 'flex', gap: '8px' } },
            expIn,
            el('button', { type: 'button', className: 'btn btn-sm', onClick: () => { expIn.value = ''; } }, '清除')
          )
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '备注'),
          noteIn
        )
      ),
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: closeDrawer }, '取消'),
        submitBtn,
      ],
    });
  };

  const paint = () => {
    clear(root);
    const filtered = keysList.filter((k) => {
      if (!search.trim()) return true;
      const q = search.trim().toLowerCase();
      return (k.name || '').toLowerCase().includes(q)
        || (k.key_prefix || '').toLowerCase().includes(q)
        || (k.note || '').toLowerCase().includes(q);
    });

    const toolbar = el('div', { className: 'section-bar' },
      el('div', { className: 'section-bar-left' },
        el('input', {
          className: 'input',
          type: 'search',
          placeholder: '按名称、前缀或备注过滤...',
          value: search,
          style: { width: '260px' },
          onInput: (e) => {
            search = e.target.value;
            paint();
          },
        }),
        el('span', { className: 'muted num', style: { fontSize: '13px' } }, `共 ${filtered.length} / ${keysList.length} 个密钥`)
      ),
      el('div', { className: 'section-bar-right' },
        ctx?.navigate
          ? el('button', { type: 'button', className: 'btn', onClick: () => ctx.navigate('branding') }, icon('brand', 14), '配置模板')
          : null,
        el('button', { type: 'button', className: 'btn', onClick: load }, icon('refresh', 14), '刷新'),
        el('button', { type: 'button', className: 'btn btn-primary', onClick: () => openKeyDrawer(null) }, icon('plus', 14), '签发新密钥')
      )
    );

    const table = filtered.length
      ? el('div', { className: 'sheet' },
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, '名称与前缀'),
                el('th', null, '启用'),
                el('th', null, '范围权限'),
                el('th', null, '请求限额'),
                el('th', null, 'Token 配额'),
                el('th', null, '有效期 / 最近使用'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...filtered.map((k) => {
                const sw = createSwitch({
                  checked: Boolean(k.enabled),
                  label: `切换 ${k.name} 启用状态`,
                  onChange: async (next) => {
                    try {
                      await api.updateKey(k.id, { enabled: next });
                      k.enabled = next;
                      toast(next ? `已启用 ${k.name}` : `已停用 ${k.name}`, 'ok');
                    } catch (e) {
                      sw.setAttribute('aria-checked', k.enabled ? 'true' : 'false');
                      toast(`更新状态失败: ${e.message}`, 'err');
                    }
                  },
                });
                const scopeNodes = [];
                if (k.models?.length) {
                  k.models.slice(0, 3).forEach((m) => scopeNodes.push(el('span', { className: 'tag-chip' }, m)));
                  if (k.models.length > 3) scopeNodes.push(el('span', { className: 'muted num', style: { fontSize: '12px' } }, `+${k.models.length - 3}`));
                }
                if (k.groups?.length) {
                  k.groups.forEach((g) => scopeNodes.push(el('span', { className: 'pill pill-brass mono' }, `组:${g}`)));
                }
                if (!scopeNodes.length) scopeNodes.push(el('span', { className: 'muted' }, '全部模型'));

                return el('tr', null,
                  el('td', { className: 'nowrap' },
                    el('div', { style: { fontWeight: 600 } }, k.name),
                    el('div', { className: 'mono muted', style: { fontSize: '12px' } }, k.key_prefix ? `${k.key_prefix}...` : k.id),
                    k.note ? el('div', { className: 'muted', style: { fontSize: '12px', marginTop: '2px', whiteSpace: 'normal', maxWidth: '220px' } }, k.note) : null
                  ),
                  el('td', null, sw),
                  el('td', null, el('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '4px', maxWidth: '240px' } }, ...scopeNodes)),
                  el('td', null,
                    el('div', { className: 'meter-stack' },
                      meterRow('RPM', k.rpm_used, k.rpm),
                      meterRow('今日 RPD', k.today_used, k.rpd),
                      meterRow('并发', k.inflight, k.concurrency)
                    )
                  ),
                  el('td', null,
                    el('div', { className: 'meter-stack' },
                      quotaMeter('今日', k.tokens_today, k.tokens_daily),
                      quotaMeter('本月', k.tokens_month, k.tokens_monthly),
                      quotaMeter('累计', k.tokens_used, k.tokens_total)
                    )
                  ),
                  el('td', { className: 'nowrap', style: { fontSize: '12px' } },
                    el('div', { className: 'num' }, k.expires_at ? `到期 ${fmtDateTime(k.expires_at)}` : '长期有效'),
                    el('div', { className: 'muted num' }, k.last_used_at ? `最近 ${fmtDateTime(k.last_used_at)}` : '尚未使用')
                  ),
                  el('td', { className: 'td-right' },
                    el('div', { style: { display: 'inline-flex', gap: '6px' } },
                      el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        title: '一键复制该密钥的分发配置模板',
                        onClick: (e) => copyKeyConfig(k, e.currentTarget),
                      }, icon('copy', 13), '复制配置'),
                      el('button', { type: 'button', className: 'btn btn-sm', onClick: () => openKeyDrawer(k) }, '编辑'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        onClick: async () => {
                          const ok = await confirmModal({
                            title: '清零 Token 用量',
                            message: `清零密钥 “${k.name}” 的今日、本月和累计 Token 用量？清零后各项 Token 配额重新开始计算，请求日志和统计不受影响。`,
                            confirmText: '清零用量',
                          });
                          if (!ok) return;
                          try {
                            await api.resetKeyUsage(k.id);
                            toast('Token 用量已清零', 'ok');
                            load();
                          } catch (e) {
                            toast(`清零失败: ${e.message}`, 'err');
                          }
                        },
                      }, '清零用量'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-danger btn-sm',
                        onClick: async () => {
                          const ok = await confirmModal({
                            title: '删除访问密钥',
                            message: `确定永久删除密钥 “${k.name}”？正在使用该密钥的客户端将立即失去访问权限。`,
                            confirmText: '确认删除',
                            danger: true,
                          });
                          if (!ok) return;
                          try {
                            await api.deleteKey(k.id);
                            sessionSecrets.delete(k.id);
                            toast('密钥已删除', 'ok');
                            load();
                          } catch (e) {
                            toast(`删除失败: ${e.message}`, 'err');
                          }
                        },
                      }, '删除')
                    )
                  )
                );
              })
            )
          )
        )
      )
      : el('div', { className: 'sheet' },
        el('div', { className: 'sheet-body muted' }, search ? '没有匹配的访问密钥。' : '尚未创建任何团队访问密钥。点击“签发新密钥”创建第一把密钥。')
      );

    root.append(toolbar, table);
  };

  load();
}
