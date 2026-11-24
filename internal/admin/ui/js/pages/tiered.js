import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  clone,
  closeDrawer,
  confirmModal,
  createSegmented,
  createSwitch,
  el,
  fmtCompact,
  fmtDateTime,
  fmtNum,
  openDrawer,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

function kvItem(label, val, mono = false) {
  return el('div', {
    style: {
      display: 'flex',
      justifyContent: 'space-between',
      alignItems: 'center',
      gap: '12px',
      padding: '8px 0',
      borderBottom: '1px solid var(--border)',
    },
  },
    el('span', { className: 'muted', style: { fontSize: '13px' } }, label),
    el('span', { className: `num${mono ? ' mono' : ''}`, style: { fontSize: '13px', fontWeight: 500 } }, val)
  );
}

function defaultBackend(tier) {
  return {
    id: `tier${tier}-node`,
    url: 'http://127.0.0.1:30000',
    tier,
    max_context_tokens: 262144,
    max_safe_input: 180000,
    promote_at: 48000,
    demote_below: tier > 1 ? 24000 : 0,
    speed_score: tier === 1 ? 100 : 60,
  };
}

export function renderTiered(container, ctx) {
  let destroyed = false;
  let statusData = null;
  let startersMap = {};
  let checkpointsMap = {};
  let selectedProvider = '';
  let warmingAll = false;
  let reqSeq = 0;
  let lastRenderKey = '';

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '18px' } });
  container.appendChild(root);

  const buildRenderKey = () => JSON.stringify({
    selectedProvider,
    warmingAll,
    statusData,
    starters: startersMap[selectedProvider] || [],
    checkpoints: checkpointsMap[selectedProvider] || null,
    draft: (store.getSection('providers') || []).map((p) => ({ n: p.name, t: p.tiered })),
  });

  const load = async (silent = false) => {
    const seq = ++reqSeq;
    try {
      const opts = silent ? { background: true } : undefined;
      const res = await api.tieredStatus(opts);
      if (destroyed || seq !== reqSeq) return;
      statusData = res;
      const provs = res?.providers || [];
      if (!selectedProvider || !provs.some((p) => p.provider === selectedProvider)) {
        selectedProvider = provs[0]?.provider || '';
      }
      if (selectedProvider) {
        try {
          const stRes = await api.tieredStarters(selectedProvider, opts);
          if (destroyed || seq !== reqSeq) return;
          startersMap[selectedProvider] = stRes?.starters || [];
        } catch {
          if (destroyed || seq !== reqSeq) return;
          if (!startersMap[selectedProvider]) startersMap[selectedProvider] = [];
        }
        try {
          const cpRes = await api.tieredCheckpoints(selectedProvider, opts);
          if (destroyed || seq !== reqSeq) return;
          checkpointsMap[selectedProvider] = {
            items: cpRes?.checkpoints || [],
            summary: cpRes?.summary || {},
          };
        } catch {
          if (destroyed || seq !== reqSeq) return;
          if (!checkpointsMap[selectedProvider]) {
            checkpointsMap[selectedProvider] = { items: [], summary: {} };
          }
        }
      }
      const key = buildRenderKey();
      if (!silent || (key !== lastRenderKey && !root.querySelector('input:focus, select:focus, textarea:focus'))) {
        paint();
      }
    } catch (e) {
      if (destroyed || seq !== reqSeq) return;
      if (!silent && e.status !== 401) toast(`读取智能路由状态失败: ${e.message}`, 'err');
      if (!statusData) paint();
    }
  };

  const pollId = setInterval(() => {
    if (!destroyed) load(true);
  }, 5000);
  if (ctx?.registerCleanup) {
    ctx.registerCleanup(() => {
      destroyed = true;
      clearInterval(pollId);
    });
  }

  const stageProviderTiered = (provName, tieredCfg, baseURL) => {
    const list = clone(store.getSection('providers') || []);
    const idx = list.findIndex((p) => p.name === provName);
    if (idx < 0) {
      toast(`未在配置中找到渠道 ${provName}`, 'err');
      return false;
    }
    list[idx].tiered = tieredCfg;
    if (baseURL !== undefined) {
      if (baseURL) list[idx].base_url = baseURL;
      else delete list[idx].base_url;
    }
    store.setDraft('providers', list);
    toast(`渠道 "${provName}" 智能路由配置已暂存至待应用栏`, 'ok');
    paint();
    return true;
  };

  const handleModeSwitch = async (provName, targetMode) => {
    const list = store.getSection('providers') || [];
    const curProv = list.find((p) => p.name === provName);
    if (!curProv || !curProv.tiered) return;
    if (curProv.tiered.mode === targetMode) return;

    if (targetMode === 'active') {
      const ok = await confirmModal({
        title: '确认切换为 Active 正式接管模式？',
        message: `切换后，渠道 “${provName}” 将直接由内置分级路由器向 Tier 后端分发请求（原 base_url 仅作 Shadow 对比回退使用）。请先确认 Shadow 对比一致率及所有后端健康状态符合预期，切换修改将在“应用并热重载”后立即无损生效。`,
        confirmText: '确认暂存 Active',
      });
      if (!ok) {
        paint();
        return;
      }
    }
    const nextTiered = clone(curProv.tiered);
    nextTiered.mode = targetMode;
    stageProviderTiered(provName, nextTiered, curProv.base_url);
  };

  const openConfigDrawer = (provName) => {
    const allProvs = clone(store.getSection('providers') || []);
    const pIdx = allProvs.findIndex((p) => p.name === provName);
    const provObj = pIdx >= 0 ? allProvs[pIdx] : null;
    if (!provObj) return;
    const fpBase = `providers[${pIdx}]`;

    const origTiered = clone(provObj.tiered || {
      mode: 'shadow',
      heavy_miss_threshold: 24000,
      health_interval_sec: 10,
      zh_think_anchor: false,
      backends: [defaultBackend(1), defaultBackend(2)],
      starter_cache: {
        enabled: true,
        canonicalize_tools: true,
        min_tokens: 1500,
        max_tokens: 12000,
        min_hits: 2,
        max_entries: 64,
        keepalive_sec: 240,
        check_interval_sec: 30,
        startup_delay_sec: 45,
        entry_ttl_hours: 168,
      },
    });
    const draft = clone(origTiered);
    const hadCC = Boolean(draft.context_compress);
    if (!draft.starter_cache) draft.starter_cache = { enabled: true, canonicalize_tools: true };
    if (!draft.context_compress) {
      draft.context_compress = {
        enabled: false,
        stage2_enabled: true,
      };
    }
    const ccDraft = draft.context_compress;
    if (ccDraft.stage2_enabled === undefined) ccDraft.stage2_enabled = true;
    const isCCCustomized = () => hadCC
      || Boolean(ccDraft.enabled)
      || ccDraft.stage2_enabled === false
      || Boolean(String(ccDraft.summarizer_url || '').trim())
      || Boolean(String(ccDraft.summarizer_model || '').trim())
      || Boolean(String(ccDraft.summarizer_key_ref || '').trim())
      || Number(ccDraft.trigger_tokens || 0) > 0
      || Number(ccDraft.stage1_target_tokens || 0) > 0;
    if (!Array.isArray(draft.backends) || !draft.backends.length) draft.backends = [defaultBackend(1)];

    let baseURLVal = provObj.base_url || '';
    const errBanner = el('div', { className: 'banner banner-err', style: { display: 'none' } });

    const baseURLIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: baseURLVal,
      placeholder: 'Shadow 必填，如 http://127.0.0.1:29999/v1；Active 可留空',
      dataset: { fieldPath: `${fpBase}.base_url` },
      onInput: (e) => { baseURLVal = e.target.value; },
    });
    const heavyIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.heavy_miss_threshold ?? 24000),
      dataset: { fieldPath: `${fpBase}.tiered.heavy_miss_threshold` },
      onInput: (e) => { draft.heavy_miss_threshold = Number(e.target.value || 0); },
    });
    const healthIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.health_interval_sec ?? 10),
      dataset: { fieldPath: `${fpBase}.tiered.health_interval_sec` },
      onInput: (e) => { draft.health_interval_sec = Number(e.target.value || 0); },
    });
    const stickyIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.heavy_sticky_sec ?? 12),
      placeholder: '12 (-1 关闭)',
      dataset: { fieldPath: `${fpBase}.tiered.heavy_sticky_sec` },
      onInput: (e) => { draft.heavy_sticky_sec = Number(e.target.value || 0); },
    });
    const spilloverIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.spillover_threshold ?? 4),
      placeholder: '4 (-1 关闭)',
      dataset: { fieldPath: `${fpBase}.tiered.spillover_threshold` },
      onInput: (e) => { draft.spillover_threshold = Number(e.target.value || 0); },
    });
    const keepaliveSSEIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.sse_keepalive_sec ?? 15),
      placeholder: '15 (-1 关闭)',
      dataset: { fieldPath: `${fpBase}.tiered.sse_keepalive_sec` },
      onInput: (e) => { draft.sse_keepalive_sec = Number(e.target.value || 0); },
    });

    const minTokIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.starter_cache.min_tokens ?? 1500),
      dataset: { fieldPath: `${fpBase}.tiered.starter_cache.min_tokens` },
      onInput: (e) => { draft.starter_cache.min_tokens = Number(e.target.value || 0); },
    });
    const minHitsIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.starter_cache.min_hits ?? 2),
      dataset: { fieldPath: `${fpBase}.tiered.starter_cache.min_hits` },
      onInput: (e) => { draft.starter_cache.min_hits = Number(e.target.value || 0); },
    });
    const keepaliveIn = el('input', {
      className: 'input num mono',
      type: 'number',
      value: String(draft.starter_cache.keepalive_sec ?? 240),
      dataset: { fieldPath: `${fpBase}.tiered.starter_cache.keepalive_sec` },
      onInput: (e) => { draft.starter_cache.keepalive_sec = Number(e.target.value || 0); },
    });
    const persistIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: draft.starter_cache.persist_path || '',
      placeholder: '默认 <data_dir>/tiered-<provider>-starters.json',
      dataset: { fieldPath: `${fpBase}.tiered.starter_cache.persist_path` },
      onInput: (e) => { draft.starter_cache.persist_path = e.target.value; },
    });
    const ccTrigIn = el('input', {
      className: 'input input-sm num mono',
      type: 'number',
      value: ccDraft.trigger_tokens ? String(ccDraft.trigger_tokens) : '',
      placeholder: '500000',
      dataset: { fieldPath: `${fpBase}.tiered.context_compress.trigger_tokens` },
      onInput: (e) => { ccDraft.trigger_tokens = Number(e.target.value || 0); },
    });
    const ccTargetIn = el('input', {
      className: 'input input-sm num mono',
      type: 'number',
      value: ccDraft.stage1_target_tokens ? String(ccDraft.stage1_target_tokens) : '',
      placeholder: '180000',
      dataset: { fieldPath: `${fpBase}.tiered.context_compress.stage1_target_tokens` },
      onInput: (e) => { ccDraft.stage1_target_tokens = Number(e.target.value || 0); },
    });
    const ccUrlIn = el('input', {
      className: 'input input-sm mono',
      type: 'text',
      value: ccDraft.summarizer_url || '',
      placeholder: '留空则复用健康后端 (需外发则填 http(s)://...)',
      dataset: { fieldPath: `${fpBase}.tiered.context_compress.summarizer_url` },
      onInput: (e) => { ccDraft.summarizer_url = e.target.value; },
    });
    const ccModelIn = el('input', {
      className: 'input input-sm mono',
      type: 'text',
      value: ccDraft.summarizer_model || '',
      placeholder: '默认沿用原请求 model',
      dataset: { fieldPath: `${fpBase}.tiered.context_compress.summarizer_model` },
      onInput: (e) => { ccDraft.summarizer_model = e.target.value; },
    });
    const ccKeyRefIn = el('input', {
      className: 'input input-sm mono',
      type: 'text',
      value: ccDraft.summarizer_key_ref || '',
      placeholder: 'env:SUMMARIZER_API_KEY (可选)',
      dataset: { fieldPath: `${fpBase}.tiered.context_compress.summarizer_key_ref` },
      onInput: (e) => { ccDraft.summarizer_key_ref = e.target.value; },
    });

    const backendsBox = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } });
    const renderBackendCards = () => {
      clear(backendsBox);
      draft.backends.forEach((b, idx) => {
        const bfp = `${fpBase}.tiered.backends[${idx}]`;
        backendsBox.appendChild(
          el('div', { className: 'sublist-item' },
            el('div', { className: 'sublist-head' },
              el('strong', { style: { fontSize: '13px' } }, `Tier ${b.tier || idx + 1} 后端节点 #${idx + 1}`),
              draft.backends.length > 1
                ? el('button', {
                  type: 'button',
                  className: 'btn btn-ghost btn-sm',
                  onClick: () => {
                    draft.backends.splice(idx, 1);
                    renderBackendCards();
                  },
                }, '移除')
                : null
            ),
            el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 80px', gap: '8px' } },
              el('div', { className: 'field' },
                el('label', { className: 'field-label' }, '节点 ID'),
                el('input', {
                  className: 'input input-sm mono',
                  value: b.id || '',
                  dataset: { fieldPath: `${bfp}.id` },
                  onInput: (e) => { b.id = e.target.value; },
                })
              ),
              el('div', { className: 'field' },
                el('label', { className: 'field-label' }, 'Tier'),
                el('input', {
                  className: 'input input-sm num mono',
                  type: 'number',
                  value: String(b.tier ?? idx + 1),
                  dataset: { fieldPath: `${bfp}.tier` },
                  onInput: (e) => { b.tier = Number(e.target.value || 0); },
                })
              )
            ),
            el('div', { className: 'field' },
              el('label', { className: 'field-label' }, '服务地址 URL'),
              el('input', {
                className: 'input input-sm mono',
                value: b.url || '',
                placeholder: 'http://127.0.0.1:30010',
                dataset: { fieldPath: `${bfp}.url` },
                onInput: (e) => { b.url = e.target.value; },
              })
            ),
            el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px' } },
              el('div', { className: 'field' },
                el('label', { className: 'field-label' }, 'Max Safe Input'),
                el('input', {
                  className: 'input input-sm num mono',
                  type: 'number',
                  value: String(b.max_safe_input ?? 0),
                  dataset: { fieldPath: `${bfp}.max_safe_input` },
                  onInput: (e) => { b.max_safe_input = Number(e.target.value || 0); },
                })
              ),
              el('div', { className: 'field' },
                el('label', { className: 'field-label' }, 'Promote At'),
                el('input', {
                  className: 'input input-sm num mono',
                  type: 'number',
                  value: String(b.promote_at ?? 0),
                  dataset: { fieldPath: `${bfp}.promote_at` },
                  onInput: (e) => { b.promote_at = Number(e.target.value || 0); },
                })
              ),
              el('div', { className: 'field' },
                el('label', { className: 'field-label' }, 'Max Context Tokens'),
                el('input', {
                  className: 'input input-sm num mono',
                  type: 'number',
                  value: String(b.max_context_tokens ?? 0),
                  dataset: { fieldPath: `${bfp}.max_context_tokens` },
                  onInput: (e) => { b.max_context_tokens = Number(e.target.value || 0); },
                })
              ),
              el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px' } },
                el('div', { className: 'field' },
                  el('label', { className: 'field-label' }, 'Demote Below'),
                  el('input', {
                    className: 'input input-sm num mono',
                    type: 'number',
                    value: String(b.demote_below ?? 0),
                    dataset: { fieldPath: `${bfp}.demote_below` },
                    onInput: (e) => { b.demote_below = Number(e.target.value || 0); },
                  })
                ),
                el('div', { className: 'field' },
                  el('label', { className: 'field-label' }, 'Speed Score'),
                  el('input', {
                    className: 'input input-sm num mono',
                    type: 'number',
                    value: String(b.speed_score ?? 0),
                    dataset: { fieldPath: `${bfp}.speed_score` },
                    onInput: (e) => { b.speed_score = Number(e.target.value || 0); },
                  })
                )
              )
            )
          )
        );
      });
      backendsBox.appendChild(
        el('button', {
          type: 'button',
          className: 'btn btn-sm',
          style: { alignSelf: 'flex-start' },
          onClick: () => {
            draft.backends.push(defaultBackend(draft.backends.length + 1));
            renderBackendCards();
          },
        }, icon('plus', 13), '添加 Tier 后端')
      );
    };
    renderBackendCards();

    const applyDraft = async () => {
      errBanner.style.display = 'none';
      const trimmedBase = baseURLVal.trim();
      if (draft.mode === 'shadow' && !trimmedBase) {
        errBanner.textContent = 'Shadow 模式必须保留上游 base_url 以对比预测结果';
        errBanner.style.display = 'flex';
        return;
      }
      const seenIDs = new Set();
      for (let i = 0; i < draft.backends.length; i++) {
        const b = draft.backends[i];
        if (!String(b.id || '').trim() || !String(b.url || '').trim() || !(b.tier > 0) || !(b.max_safe_input > 0) || !(b.promote_at > 0)) {
          errBanner.textContent = `后端节点 #${i + 1} 的 ID、URL、Tier、Max Safe Input 与 Promote At 均必须有效填写`;
          errBanner.style.display = 'flex';
          return;
        }
        b.id = b.id.trim();
        if (seenIDs.has(b.id)) {
          errBanner.textContent = `后端节点 ID 重复：${b.id}`;
          errBanner.style.display = 'flex';
          return;
        }
        seenIDs.add(b.id);
        b.url = b.url.trim().replace(/\/+$/, '');
      }
      if (origTiered.mode !== 'active' && draft.mode === 'active') {
        const ok = await confirmModal({
          title: '确认启用 Active 正式接管模式？',
          message: `渠道 “${provName}” 将由内置智能路由器直接分流至 ${draft.backends.length} 个分层后端，请确保已完成 Shadow 验证。确认后暂存到待应用栏。`,
          confirmText: '确认暂存 Active',
        });
        if (!ok) return;
      }
      if (!String(draft.starter_cache.persist_path || '').trim()) {
        delete draft.starter_cache.persist_path;
      }
      if (!isCCCustomized()) {
        delete draft.context_compress;
      } else {
        const trimCC = (k) => {
          const v = String(ccDraft[k] || '').trim();
          if (v) ccDraft[k] = v;
          else delete ccDraft[k];
        };
        trimCC('summarizer_url');
        trimCC('summarizer_model');
        trimCC('summarizer_key_ref');
        if (!(Number(ccDraft.trigger_tokens) > 0)) delete ccDraft.trigger_tokens;
        if (!(Number(ccDraft.stage1_target_tokens) > 0)) delete ccDraft.stage1_target_tokens;
      }
      closeDrawer();
      stageProviderTiered(provName, draft, trimmedBase);
    };

    const body = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
      errBanner,
      el('div', { className: 'field' },
        el('label', { className: 'field-label' }, '运行模式'),
        createSegmented({
          ariaLabel: '运行模式',
          value: draft.mode || 'shadow',
          options: [
            { value: 'shadow', label: 'Shadow 旁路验证', tone: 'warn' },
            { value: 'active', label: 'Active 正式接管', tone: 'ok' },
          ],
          onChange: (v) => { draft.mode = v; },
        }),
        el('div', { className: 'field-hint' }, 'Shadow 仅预测与记录一致率，不改变现有转发；Active 直接接管并开启 KV Starter 预热。')
      ),
      el('div', { className: 'field' },
        el('label', { className: 'field-label' }, '上游 Base URL（Shadow 模式必填）'),
        baseURLIn
      ),
      el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '12px' } },
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, 'Heavy Miss 阈值 (tokens)'),
          heavyIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '健康探测间隔 (秒)'),
          healthIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '重算写入黏性窗口 (秒，-1 关闭)'),
          stickyIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '高并发溢出阈值 (-1 关闭)'),
          spilloverIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '长 Prefill SSE 保活 (秒，-1 关闭)'),
          keepaliveSSEIn
        )
      ),
      el('div', { className: 'sublist-item', style: { gap: '10px' } },
        el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } },
          el('div', null,
            el('div', { style: { fontWeight: 500, fontSize: '13px' } }, '中文思考锚点注入 (zh_think_anchor)'),
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '为纯中文末尾用户输入注入思维锚点提示')
          ),
          createSwitch({
            checked: Boolean(draft.zh_think_anchor),
            label: '中文思考锚点',
            onChange: (v) => { draft.zh_think_anchor = v; },
          })
        ),
        el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } },
          el('div', null,
            el('div', { style: { fontWeight: 500, fontSize: '13px' } }, 'Tool 定义规范化排序 (canonicalize_tools)'),
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '确定性重排 tools 与 JSON schema 提升 KV 复用')
          ),
          createSwitch({
            checked: Boolean(draft.starter_cache.canonicalize_tools),
            label: 'Tool 规范化',
            onChange: (v) => { draft.starter_cache.canonicalize_tools = v; },
          })
        ),
        el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } },
          el('div', null,
            el('div', { style: { fontWeight: 500, fontSize: '13px' } }, 'Starter 观察与空闲预热 (starter_cache.enabled)'),
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '自动归纳高频系统提示词并在后端空闲时预热')
          ),
          createSwitch({
            checked: Boolean(draft.starter_cache.enabled),
            label: 'Starter 缓存',
            onChange: (v) => { draft.starter_cache.enabled = v; },
          })
        )
      ),
      el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '8px' } },
        el('div', { className: 'field' }, el('label', { className: 'field-label' }, '最小 Token'), minTokIn),
        el('div', { className: 'field' }, el('label', { className: 'field-label' }, '最小命中次数'), minHitsIn),
        el('div', { className: 'field' }, el('label', { className: 'field-label' }, '保活周期 (秒)'), keepaliveIn)
      ),
      el('div', { className: 'field' },
        el('label', { className: 'field-label' }, 'Starter 持久化路径'),
        persistIn
      ),
      el('div', { className: 'sublist-item', style: { gap: '10px' } },
        el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } },
          el('div', null,
            el('div', { style: { fontWeight: 500, fontSize: '13px' } }, '两阶段上下文压缩 (context_compress.enabled)'),
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '超出阈值时自动冻结 Checkpoint 并执行 Stage 1/2 压缩')
          ),
          createSwitch({
            checked: Boolean(ccDraft.enabled),
            label: '两阶段压缩',
            onChange: (v) => { ccDraft.enabled = v; },
          })
        ),
        el('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } },
          el('div', null,
            el('div', { style: { fontWeight: 500, fontSize: '13px' } }, 'Stage 2 异步摘要与预热 (stage2_enabled)'),
            el('div', { className: 'muted', style: { fontSize: '12px' } }, '空闲时异步生成结构化摘要并预热 KV 缓存')
          ),
          createSwitch({
            checked: ccDraft.stage2_enabled !== false,
            label: 'Stage 2 摘要',
            onChange: (v) => { ccDraft.stage2_enabled = v; },
          })
        ),
        el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px' } },
          el('div', { className: 'field' }, el('label', { className: 'field-label' }, '触发阈值 Tokens'), ccTrigIn),
          el('div', { className: 'field' }, el('label', { className: 'field-label' }, 'Stage 1 目标 Tokens'), ccTargetIn)
        ),
        el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px' } },
          el('div', { className: 'field' }, el('label', { className: 'field-label' }, 'Summarizer URL (可选)'), ccUrlIn),
          el('div', { className: 'field' }, el('label', { className: 'field-label' }, 'Summarizer Model (可选)'), ccModelIn)
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, 'Summarizer Key Ref (可选)'),
          ccKeyRefIn,
          ccDraft.has_summarizer_key
            ? el('div', { className: 'muted', style: { fontSize: '11px', marginTop: '4px' } }, '当前已保存内联密钥；留空保存将继续保留现有密钥')
            : null
        )
      ),
      el('div', { className: 'field' },
        el('label', { className: 'field-label' }, 'Tier 后端节点矩阵'),
        el('div', { className: 'field-hint', style: { marginBottom: '6px' } }, '多个节点可共用同一 Tier，同层按活跃并发均衡与会话亲和度调度；节点 ID 不可重复。'),
        backendsBox
      )
    );

    const { panel } = openDrawer({
      title: `编辑智能分层策略 / ${provName}`,
      body,
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: closeDrawer }, '取消'),
        el('button', { type: 'button', className: 'btn btn-primary', onClick: applyDraft }, '暂存变更'),
      ],
    });
    if (panel) panel.style.width = '520px';
  };

  const paint = () => {
    lastRenderKey = buildRenderKey();
    clear(root);
    const provs = statusData?.providers || [];
    const cfgProvs = store.getSection('providers') || [];

    if (!provs.length) {
      root.appendChild(
        el('section', { className: 'sheet' },
          el('div', { className: 'sheet-head' },
            el('div', { className: 'sheet-title-group' },
              el('h3', { className: 'sheet-title' }, '尚未启用内置智能路由'),
              el('p', { className: 'sheet-sub' }, '为本地多实例渠道启用 Shadow 旁路对比或 Active 上下文分层路由。')
            )
          ),
          el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
            el('p', { className: 'muted', style: { margin: 0 } }, '支持按请求前缀与未命中 token 估算在 200K / 512K / 1M 实例间自动升降级、同会话粘性滞回、400 超窗重路由与 Starter KV 预热。'),
            cfgProvs.length
              ? el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
                ...cfgProvs.map((cp) =>
                  el('button', {
                    type: 'button',
                    className: 'btn btn-primary',
                    onClick: () => openConfigDrawer(cp.name),
                  }, icon('plus', 14), `为渠道 ${cp.name} 配置智能路由`)
                )
              )
              : el('p', { className: 'muted', style: { margin: 0 } }, '请先在“模型渠道”页面添加至少一个渠道。')
          )
        )
      );
      return;
    }

    const st = provs.find((p) => p.provider === selectedProvider) || provs[0];
    selectedProvider = st.provider;
    const draftProv = cfgProvs.find((p) => p.name === st.provider);
    const draftMode = draftProv?.tiered?.mode || st.mode;
    const isShadow = st.mode !== 'active';
    const backends = st.backends || [];
    const shadow = st.shadow || {};
    const scStats = st.starter_cache || {};
    const ccStats = st.context_compress || {};
    const recent = st.recent_routes || [];
    const mismatches = shadow.mismatches || [];
    const starters = startersMap[st.provider] || [];
    const cpData = checkpointsMap[st.provider] || { items: [], summary: {} };
    const checkpoints = cpData.items || [];

    const healthyCount = backends.filter((b) => b.healthy).length;
    const totalRouted = backends.reduce((sum, b) => sum + Number(b.total_routed || 0), 0);
    const matchRate = Number(shadow.match_rate || 0);
    const compared = Number(shadow.compared || 0);

    // Top control bar
    const providerControl = provs.length > 1
      ? createSegmented({
        ariaLabel: '选择智能路由渠道',
        value: st.provider,
        options: provs.map((p) => ({ value: p.provider, label: p.provider })),
        onChange: (name) => {
          selectedProvider = name;
          load(false);
        },
      })
      : el('span', { className: 'pill mono', style: { fontWeight: 600 } }, st.provider);

    const toolbar = el('div', { className: 'section-bar' },
      el('div', { className: 'section-bar-left' },
        providerControl,
        el('span', { className: `pill ${isShadow ? 'pill-brass' : 'pill-ok'}` },
          isShadow ? 'Shadow 旁路验证中' : 'Active 正式接管'
        ),
        draftMode !== st.mode
          ? el('span', { className: 'pill pill-warn' }, `草稿待切换至 ${draftMode}`)
          : null
      ),
      el('div', { className: 'section-bar-right' },
        createSegmented({
          ariaLabel: '快捷切换路由模式',
          value: draftMode,
          options: [
            { value: 'shadow', label: 'Shadow 旁路', tone: 'warn' },
            { value: 'active', label: 'Active 接管', tone: 'ok' },
          ],
          onChange: (v) => handleModeSwitch(st.provider, v),
        }),
        el('button', {
          type: 'button',
          className: 'btn',
          onClick: () => openConfigDrawer(st.provider),
        }, icon('edit', 14), '编辑策略'),
        el('button', {
          type: 'button',
          className: 'btn',
          onClick: () => load(false),
        }, icon('refresh', 14), '刷新')
      )
    );

    // Hero KPI Grid (44px focal number; keep foot tokens grouped inside nowrap spans to prevent orphan wraps)
    const focalValue = isShadow
      ? (compared > 0 ? `${matchRate.toFixed(1)}%` : '-')
      : fmtCompact(totalRouted);
    const focalTone = isShadow && compared > 0
      ? (matchRate >= 95 ? 'var(--verdigris)' : matchRate < 80 ? 'var(--oxblood)' : 'var(--brass-text)')
      : 'var(--ink)';

    const kpi1FootItems = isShadow
      ? [
        el('span', { className: 'nowrap' }, `比对 ${fmtNum(compared)}`),
        el('span', { className: 'nowrap' }, `一致 ${fmtNum(shadow.matched || 0)}`),
        el('span', { className: 'nowrap' }, `未知 ${fmtNum(shadow.unknown || 0)}`),
      ]
      : (backends.length
        ? backends.map((b) => el('span', { className: 'nowrap' }, `${b.id}: ${fmtCompact(b.total_routed || 0)}`))
        : [el('span', { className: 'nowrap' }, '暂无后端')]);

    const kpiGrid = el('section', { className: 'kpi-grid' },
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, isShadow ? 'Shadow 预测一致率' : '分层路由总请求数'),
        el('div', { className: 'kpi-value', style: { color: focalTone } }, focalValue),
        el('div', { className: 'kpi-foot num' }, ...kpi1FootItems)
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, '健康后端节点'),
        el('div', { className: 'kpi-value' }, `${healthyCount}/${backends.length}`),
        el('div', { className: 'kpi-foot num' },
          el('span', { className: 'nowrap' }, `探测 ${st.health_interval_sec || 10}s`),
          el('span', { className: 'nowrap' }, `Heavy 阈值 ${fmtCompact(st.heavy_miss_threshold || 24000)}`)
        )
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, 'KV 前缀与活跃会话'),
        el('div', { className: 'kpi-value' }, fmtCompact(st.cached_prefixes || 0)),
        el('div', { className: 'kpi-foot num' },
          el('span', { className: 'nowrap' }, `活跃会话 ${fmtNum(st.tracked_sessions || 0)}`),
          el('span', { className: 'nowrap' }, `重排工具 ${fmtNum(scStats.tools_reordered || 0)}`)
        )
      ),
      el('div', { className: 'kpi-cell' },
        el('div', { className: 'kpi-label' }, 'Starter 观察与预热'),
        el('div', { className: 'kpi-value' }, fmtNum(scStats.tracked ?? starters.length)),
        el('div', { className: 'kpi-foot num' },
          el('span', { className: 'nowrap' }, `钉选 ${fmtNum(scStats.pinned || 0)}`),
          el('span', { className: 'nowrap' }, `预热池 ${fmtNum(scStats.active_warm_set || 0)}`),
          el('span', { className: 'nowrap' }, `成功 ${fmtNum(scStats.total_warms || 0)}`)
        )
      )
    );

    // Backends Matrix Sheet (compact + nowrap on numeric pairs so 1024px fits cleanly without hiding column 7)
    const backendsSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, 'Tier 后端节点状态'),
          el('p', { className: 'sheet-sub' }, '实时健康、上下文安全窗口、升降级水位与在途并发')
        )
      ),
      el('div', { className: 'table-wrap' },
        el('table', { className: 'data-table compact' },
          el('thead', null,
            el('tr', null,
              el('th', null, 'Tier / 节点'),
              el('th', null, '状态'),
              el('th', { className: 'th-right' }, '上下文 / 安全'),
              el('th', { className: 'th-right' }, '升 / 降级'),
              el('th', { className: 'th-right' }, '速度'),
              el('th', { className: 'th-right' }, '在途 (总 / H)'),
              el('th', { className: 'th-right' }, '累计 (总 / H)')
            )
          ),
          el('tbody', null,
            ...backends.map((b) =>
              el('tr', null,
                el('td', null,
                  el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
                    el('span', { className: 'pill mono' }, `T${b.tier}`),
                    el('strong', { className: 'mono nowrap' }, b.id)
                  ),
                  el('div', { className: 'muted mono nowrap', style: { fontSize: '12px', marginTop: '2px' } }, b.url)
                ),
                el('td', { className: 'nowrap' },
                  b.healthy
                    ? el('span', { className: 'pill pill-ok' }, el('span', { className: 'dot' }), '健康')
                    : el('span', { className: 'pill pill-err', title: b.last_health_err || '探测异常' }, el('span', { className: 'dot' }), '异常')
                ),
                el('td', { className: 'td-right num mono nowrap' },
                  `${b.max_context_tokens ? fmtCompact(b.max_context_tokens) : '-'} / ${fmtCompact(b.max_safe_input)}`
                ),
                el('td', { className: 'td-right num mono nowrap' },
                  `${fmtCompact(b.promote_at)} / ${b.demote_below ? fmtCompact(b.demote_below) : '-'}`
                ),
                el('td', { className: 'td-right num nowrap' }, String(b.speed_score || 0)),
                el('td', { className: 'td-right num mono nowrap' },
                  `${fmtNum(b.active_reqs || 0)} / ${fmtNum(b.active_heavy || 0)}`
                ),
                el('td', { className: 'td-right num mono nowrap' },
                  `${fmtNum(b.total_routed || 0)} / ${fmtNum(b.heavy_routed || 0)}`
                )
              )
            )
          )
        )
      )
    );

    // Analytics Split Band (No wide table inside split-2 so zero overflow)
    const maxRouted = Math.max(1, ...backends.map((b) => Number(b.total_routed || 0)));
    const distPanel = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, isShadow ? 'Shadow 按线上后端对照分布' : '各 Tier 请求分流占比'),
          el('p', { className: 'sheet-sub' }, isShadow ? `基准自 ${fmtDateTime(shadow.since)}` : '累计命中分布与 Heavy Miss 比例')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
        ...(() => {
          const configuredIDs = new Set(backends.map((b) => b.id));
          const extraActuals = isShadow
            ? Object.keys(shadow.by_actual || {}).filter((id) => id && !configuredIDs.has(id)).sort()
            : [];
          const items = backends.map((b) => ({
            id: b.id,
            label: `T${b.tier} ${b.id}`,
            routed: Number(b.total_routed || 0),
            heavyRouted: Number(b.heavy_routed || 0),
            byAct: shadow.by_actual?.[b.id],
            unconfigured: false,
          }));
          for (const id of extraActuals) {
            items.push({
              id,
              label: `${id} (未在配置中)`,
              routed: 0,
              heavyRouted: 0,
              byAct: shadow.by_actual?.[id],
              unconfigured: true,
            });
          }
          return items.map((item) => {
            const byAct = item.byAct;
            const valPct = isShadow && byAct?.total
              ? Math.round((byAct.matched / byAct.total) * 100)
              : Math.round((item.routed / maxRouted) * 100);
            const metaRight = isShadow && byAct?.total
              ? `${fmtNum(byAct.matched)}/${fmtNum(byAct.total)} 一致 (${valPct}%)`
              : `${fmtNum(item.routed)} 次 (${totalRouted ? Math.round((item.routed / totalRouted) * 100) : 0}%) / Heavy ${fmtNum(item.heavyRouted)}`;
            return el('div', { className: 'meter-line' },
              el('div', { className: 'meter-meta' },
                el('span', { className: 'mono', style: { color: item.unconfigured ? 'var(--brass-text)' : 'var(--ink)' } }, item.label),
                el('span', { className: 'num' }, metaRight)
              ),
              el('div', { className: 'meter-track' },
                el('div', {
                  className: `meter-fill${(item.unconfigured || (isShadow && byAct?.total && valPct < 80)) ? ' danger' : ''}`,
                  style: { width: `${valPct}%` },
                })
              )
            );
          });
        })()
      )
    );

    const starterSummaryPanel = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '策略与 Starter 运行摘要'),
          el('p', { className: 'sheet-sub' }, '前置规范化、思考锚点与后台 KV 保活')
        )
      ),
      el('div', { className: 'sheet-body' },
        kvItem('Starter 观察器', scStats.enabled ? '已开启' : '未启用'),
        kvItem('Tool 声明规范化排序', `${scStats.canonicalize_tools ? '已开启' : '关闭'} (${fmtNum(scStats.tools_reordered || 0)} 次)`),
        kvItem('中文思考锚点注入', st.zh_think_anchor ? '已开启' : '关闭'),
        kvItem('最小观察门槛', `${fmtCompact(scStats.min_tokens || 1500)} tokens / ${fmtNum(scStats.min_hits || 2)} hits`, true),
        kvItem('后台保活周期 / 最近预热', `${scStats.keepalive_sec || 240}s / ${scStats.last_warm_at || '暂无'}`),
        kvItem('预热命中 / 失败统计', `${fmtNum(scStats.total_warm_hits || 0)} hit / ${fmtNum(scStats.total_warm_fails || 0)} fail`, true),
        kvItem(
          '两阶段上下文压缩',
          ccStats.enabled
            ? `已开启 (触发 ${fmtCompact(ccStats.trigger_tokens || 0)}t / CP ${fmtNum(ccStats.checkpoints_total ?? checkpoints.length)} / S1 ${fmtNum(ccStats.total_stage1_hits || 0)} · S2 ${fmtNum(ccStats.total_stage2_hits || 0)} hit)`
            : '未启用',
          true
        )
      )
    );

    const splitBand = el('div', { className: 'split-2' }, distPanel, starterSummaryPanel);

    // Shadow Mismatches Sheet (shown when shadow mode or has history)
    const shadowSheet = (isShadow || compared > 0 || mismatches.length > 0)
      ? el('section', { className: 'sheet' },
        el('div', { className: 'sheet-head' },
          el('div', { className: 'sheet-title-group' },
            el('h3', { className: 'sheet-title' }, 'Shadow 旁路对比不一致样本'),
            el('p', { className: 'sheet-sub' },
              mismatches.length
                ? `共记录 ${fmtNum(mismatches.length)} 条预测与现网不一致记录，用于评估切流前一致性`
                : '当前 Shadow 预测与现网路由完全一致，暂无偏差样本'
            )
          ),
          el('div', { className: 'sheet-actions' },
            el('button', {
              type: 'button',
              className: 'btn btn-sm',
              onClick: async () => {
                try {
                  await api.resetTieredShadow(st.provider);
                  toast('已重置 Shadow 统计基准', 'ok');
                  await load(false);
                } catch (e) {
                  toast(`重置失败: ${e.message}`, 'err');
                }
              },
            }, '重置 Shadow 统计')
          )
        ),
        mismatches.length
          ? el('div', { className: 'table-wrap' },
            el('table', { className: 'data-table compact' },
              el('thead', null,
                el('tr', null,
                  el('th', null, '时间'),
                  el('th', null, 'Shadow 预测'),
                  el('th', null, '现网实际'),
                  el('th', null, '预测理由'),
                  el('th', null, '实际理由'),
                  el('th', { className: 'th-right' }, '估算 / 未命中 Token')
                )
              ),
              el('tbody', null,
                ...mismatches.slice(-20).reverse().map((m) =>
                  el('tr', null,
                    el('td', { className: 'num mono muted nowrap' }, m.time || '-'),
                    el('td', null, el('span', { className: 'pill pill-brass mono' }, m.predicted || '-')),
                    el('td', null, el('span', { className: 'pill mono' }, m.actual || '-')),
                    el('td', { className: 'mono muted', style: { fontSize: '12px' } }, m.predicted_reason || '-'),
                    el('td', { className: 'mono muted', style: { fontSize: '12px' } }, m.actual_reason || '-'),
                    el('td', { className: 'td-right num mono nowrap' }, `${fmtCompact(m.total_est)} / ${fmtCompact(m.miss_est)}`)
                  )
                )
              )
            )
          )
          : el('div', { className: 'sheet-body muted' }, '尚未出现 Shadow 预测偏差样本。')
      )
      : null;

    // Recent Routes Sheet
    const recentSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '近期路由决策流'),
          el('p', { className: 'sheet-sub' }, `保留最近 ${recent.length} 条分层决策，含前缀缓存估算校准明细`)
        )
      ),
      recent.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, '时间'),
                el('th', null, '后端'),
                el('th', null, '路由决策依据'),
                el('th', { className: 'th-right' }, '估算 / 缓存 / 未命中'),
                el('th', { className: 'th-right' }, '实际 Prompt / Cached'),
                el('th', { className: 'th-right' }, '状态 / 耗时')
              )
            ),
            el('tbody', null,
              ...[...recent].reverse().slice(0, 15).map((r) =>
                el('tr', null,
                  el('td', { className: 'num mono muted nowrap' }, r.time || '-'),
                  el('td', { className: 'nowrap' }, el('span', { className: 'pill mono' }, r.backend || '-')),
                  el('td', { className: 'mono', style: { fontSize: '12px' } },
                    r.reason || '-',
                    r.compress_mode && r.compress_mode !== 'none'
                      ? el('span', { className: 'pill pill-brass', style: { marginLeft: '6px' } },
                        r.orig_est ? `${r.compress_mode} · ${fmtCompact(r.orig_est)}` : r.compress_mode)
                      : null
                  ),
                  el('td', { className: 'td-right num mono nowrap' },
                    `${fmtCompact(r.total_est)} / ${fmtCompact(r.cached_est)} / ${fmtCompact(r.miss_est)}`
                  ),
                  el('td', { className: 'td-right num mono nowrap' },
                    r.actual_prompt ? `${fmtCompact(r.actual_prompt)} / ${fmtCompact(r.actual_cache || 0)}` : '-'
                  ),
                  el('td', { className: 'td-right num nowrap' },
                    el('span', { className: `pill ${r.status >= 200 && r.status < 300 ? 'pill-ok' : 'pill-err'} num` }, String(r.status || '-')),
                    ' ',
                    el('span', { className: 'muted mono' }, `${fmtNum(r.duration_ms || 0)}ms`)
                  )
                )
              )
            )
          )
        )
        : el('div', { className: 'sheet-body muted' }, '暂无近期路由记录。')
    );

    // Starters Cache Table Sheet
    const startersSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, 'Starter 前缀模板与预热池'),
          el('p', { className: 'sheet-sub' }, '仅展示脱敏片段与 Hash 指纹；钉选后将在空闲时优先保活 KV Cache')
        ),
        el('div', { className: 'sheet-actions' },
          el('button', {
            type: 'button',
            className: 'btn btn-primary btn-sm',
            disabled: warmingAll,
            onClick: async () => {
              warmingAll = true;
              paint();
              try {
                const res = await api.warmTieredStarters(st.provider, { force: true });
                const n = (res.results || []).length;
                toast(`已触发全量预热，完成 ${n} 项后端请求`, 'ok');
                await load(false);
              } catch (e) {
                if (e.code === 'shadow_mode' || e.status === 409) {
                  toast(e.message || 'Shadow 模式下禁止预热，切换到 Active 后可用', 'warn');
                } else {
                  toast(`预热失败: ${e.message}`, 'err');
                }
              } finally {
                warmingAll = false;
                paint();
              }
            },
          }, icon('ping', 14), warmingAll ? '预热中...' : '立即预热全部')
        )
      ),
      starters.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, 'Hash / 模型'),
                el('th', null, '摘要预览'),
                el('th', { className: 'th-right' }, 'Tokens / Tools'),
                el('th', { className: 'th-right' }, '命中'),
                el('th', null, '状态 / 最近预热'),
                el('th', null, '钉选'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...starters.map((s) =>
                el('tr', null,
                  el('td', null,
                    el('div', { className: 'mono nowrap', style: { fontWeight: 500, fontSize: '12px' } }, s.hash.slice(0, 12)),
                    el('div', { className: 'muted mono nowrap', style: { fontSize: '12px' } }, s.model || '-')
                  ),
                  el('td', { style: { maxWidth: '280px' } },
                    s.label ? el('span', { className: 'pill pill-brass', style: { marginRight: '6px' } }, s.label) : null,
                    s.client_hint ? el('span', { className: 'muted mono', style: { fontSize: '11px', marginRight: '6px' } }, s.client_hint) : null,
                    el('div', {
                      className: 'muted',
                      style: { fontSize: '12px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
                      title: s.system_preview || '',
                    }, s.system_preview || '(无系统文本摘要)')
                  ),
                  el('td', { className: 'td-right num mono nowrap' }, `${fmtCompact(s.est_tokens)} / ${s.tools_count || 0}`),
                  el('td', { className: 'td-right num nowrap' }, fmtNum(s.hits || 0)),
                  el('td', { className: 'nowrap' },
                    el('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap', alignItems: 'center' } },
                      el('span', { className: `pill ${s.qualified ? 'pill-ok' : ''}` }, s.qualified ? '已达标' : '观察中'),
                      s.lcp_shrunk ? el('span', { className: 'pill' }, 'LCP') : null,
                      s.last_warmed ? el('span', { className: 'mono muted', style: { fontSize: '11px' } }, `${s.last_warm_target || ''}`) : null
                    )
                  ),
                  el('td', null,
                    createSwitch({
                      checked: Boolean(s.pinned),
                      label: `钉选 ${s.hash.slice(0, 8)}`,
                      onChange: async (nextVal) => {
                        try {
                          await api.pinTieredStarter(st.provider, {
                            hash: s.hash,
                            pinned: nextVal,
                            label: s.label || '',
                          });
                          toast(nextVal ? '已钉选 Starter' : '已取消钉选', 'ok');
                          await load(false);
                        } catch (e) {
                          toast(`操作失败: ${e.message}`, 'err');
                          await load(true);
                        }
                      },
                    })
                  ),
                  el('td', { className: 'td-right nowrap' },
                    el('div', { style: { display: 'inline-flex', gap: '6px' } },
                      el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        onClick: async () => {
                          try {
                            await api.warmTieredStarters(st.provider, { hash: s.hash, force: true });
                            toast(`Starter ${s.hash.slice(0, 8)} 预热完成`, 'ok');
                            await load(false);
                          } catch (e) {
                            if (e.code === 'shadow_mode' || e.status === 409) {
                              toast(e.message || 'Shadow 模式下禁止预热', 'warn');
                            } else {
                              toast(`预热失败: ${e.message}`, 'err');
                            }
                          }
                        },
                      }, '预热'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-danger btn-sm',
                        onClick: async () => {
                          const ok = await confirmModal({
                            title: '删除 Starter 模板',
                            message: `确定移除模板 ${s.hash.slice(0, 12)}？若客户端继续发送相同前缀，后续仍会重新观察入库。`,
                            confirmText: '确认删除',
                            danger: true,
                          });
                          if (!ok) return;
                          try {
                            await api.deleteTieredStarter(st.provider, s.hash);
                            toast('已删除 Starter', 'ok');
                            await load(false);
                          } catch (e) {
                            toast(`删除失败: ${e.message}`, 'err');
                          }
                        },
                      }, '删除')
                    )
                  )
                )
              )
            )
          )
        )
        : el('div', { className: 'sheet-body muted' }, '尚未观察到满足最小 Token 阈值的重复 Starter 提示词前缀。')
    );

    const checkpointsSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '上下文压缩 Checkpoints'),
          el('p', { className: 'sheet-sub' },
            `${checkpoints.length} 个冻结 Checkpoint · 触发阈值 ${fmtCompact(ccStats.trigger_tokens || 0)} · 已累计节省 ${fmtCompact(ccStats.total_saved_tokens || 0)} tokens`
          )
        ),
        checkpoints.length
          ? el('div', { className: 'sheet-actions' },
            el('button', {
              type: 'button',
              className: 'btn btn-danger btn-sm',
              onClick: async () => {
                const ok = await confirmModal({
                  title: '清空 Checkpoints',
                  message: `确定清空 ${st.provider} 的全部 ${checkpoints.length} 个压缩 Checkpoint？后续长对话将重新执行 Stage 1 冻结。`,
                  confirmText: '确认清空',
                  danger: true,
                });
                if (!ok) return;
                try {
                  await api.tieredClearCheckpoints(st.provider);
                  toast('已清空全部 Checkpoints', 'ok');
                  await load(false);
                } catch (e) {
                  toast(`清空失败: ${e.message}`, 'err');
                }
              },
            }, '清空全部')
          )
          : null
      ),
      checkpoints.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, 'ID / 代际'),
                el('th', null, '摘要预览 / 归档范围'),
                el('th', { className: 'th-right' }, '原 → S1 / 摘要'),
                el('th', null, 'Stage 2 状态'),
                el('th', { className: 'th-right' }, '命中 / 预热'),
                el('th', null, '作用域'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...checkpoints.map((cp) =>
                el('tr', null,
                  el('td', { className: 'nowrap' },
                    el('div', { className: 'mono', style: { fontWeight: 500, fontSize: '12px' }, title: cp.id }, cp.id.slice(0, 12)),
                    el('div', { style: { display: 'flex', gap: '4px', alignItems: 'center', marginTop: '2px' } },
                      el('span', { className: 'pill mono' }, `G${cp.generation} · S${cp.stage}`),
                      cp.confidential ? el('span', { className: 'pill pill-brass' }, '机密') : null
                    )
                  ),
                  el('td', { style: { maxWidth: '280px' } },
                    el('div', {
                      className: 'muted',
                      style: { fontSize: '12px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
                      title: cp.summary_preview || '',
                    }, cp.summary_preview || (cp.confidential ? '(机密会话：仅内存摘要，已脱敏隐藏)' : '(尚未生成 Stage 2 摘要)')),
                    el('div', { className: 'muted mono', style: { fontSize: '11px' } }, `${fmtNum(cp.archived_msg_count || 0)} msgs · cut@${cp.cut_msg_idx || 0}`)
                  ),
                  el('td', { className: 'td-right num mono nowrap' },
                    el('div', null, `${fmtCompact(cp.original_tokens)} → ${fmtCompact(cp.stage1_tokens)}`),
                    cp.summary_tokens ? el('div', { className: 'muted', style: { fontSize: '11px' } }, `摘要 ${fmtCompact(cp.summary_tokens)}`) : null
                  ),
                  el('td', { className: 'nowrap' },
                    el('span', {
                      className: `pill ${cp.stage2_status === 'ready' ? 'pill-ok' : cp.stage2_status === 'failed' ? 'pill-err' : ''}`,
                      title: cp.stage2_err || '',
                    }, cp.stage2_status || 'none'),
                    cp.stage2_backend ? el('div', { className: 'muted mono', style: { fontSize: '11px', marginTop: '2px' } }, `${cp.stage2_backend}${cp.stage2_ms ? ` · ${cp.stage2_ms}ms` : ''}`) : null
                  ),
                  el('td', { className: 'td-right num mono nowrap' },
                    el('div', null, `${fmtNum(cp.hits || 0)} hit`),
                    cp.prewarm_cached ? el('div', { className: 'muted', style: { fontSize: '11px' } }, `PW ${fmtCompact(cp.prewarm_cached)}`) : null
                  ),
                  el('td', { className: 'mono muted nowrap' }, cp.origin_target || 'local'),
                  el('td', { className: 'td-right nowrap' },
                    el('button', {
                      type: 'button',
                      className: 'btn btn-danger btn-sm',
                      onClick: async () => {
                        const ok = await confirmModal({
                          title: '删除 Checkpoint',
                          message: `确定删除 Checkpoint ${cp.id.slice(0, 12)}？删除后该前缀下次超阈将重新构建。`,
                          confirmText: '确认删除',
                          danger: true,
                        });
                        if (!ok) return;
                        try {
                          await api.tieredDeleteCheckpoint(st.provider, cp.id);
                          toast('已删除 Checkpoint', 'ok');
                          await load(false);
                        } catch (e) {
                          toast(`删除失败: ${e.message}`, 'err');
                        }
                      },
                    }, '删除')
                  )
                )
              )
            )
          )
        )
        : el('div', { className: 'sheet-body muted' }, '暂无冻结 Checkpoint；开启 context_compress 且上下文超过触发阈值后将自动建立并可异步升级 Stage 2 摘要。')
    );

    root.append(toolbar, kpiGrid, backendsSheet, splitBand);
    if (shadowSheet) root.appendChild(shadowSheet);
    root.append(recentSheet, checkpointsSheet, startersSheet);
  };

  load(false);
}
