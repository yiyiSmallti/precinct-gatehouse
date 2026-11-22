import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  closeDrawer,
  clone,
  confirmModal,
  createJsonTextarea,
  createSegmented,
  createTagInput,
  el,
  fmtNum,
  openDrawer,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

function findProviderReferences(name) {
  const routing = store.getSection('routing') || {};
  const aliases = store.getSection('aliases') || [];
  const refs = [];
  const prefix = `${name}/`;
  Object.entries(routing.routing_groups || {}).forEach(([gname, g]) => {
    if ((g.members || []).some((m) => String(m).startsWith(prefix))) {
      refs.push(`路由组 "${gname}" 成员`);
    }
    if (g.classifier && String(g.classifier).startsWith(prefix)) {
      refs.push(`路由组 "${gname}" 分类器`);
    }
    (g.rules || []).forEach((r) => {
      if (r.use && String(r.use).startsWith(prefix)) {
        refs.push(`路由组 "${gname}" 规则 "${r.name || '未命名'}"`);
      }
    });
  });
  aliases.forEach((a) => {
    if (a.use && (String(a.use).startsWith(prefix) || a.use === name)) {
      refs.push(`模型别名 "${a.match}"`);
    }
  });
  return refs;
}

export function renderChannels(container) {
  let runtime = null;
  let pingResults = {};
  let filterText = '';

  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } });
  container.appendChild(root);

  const loadRuntime = async () => {
    try {
      runtime = await api.channels();
    } catch {
      runtime = null;
    }
    paint();
  };

  const pingProvider = async (prov, model) => {
    const targetModel = model || (prov.models || [])[0] || '';
    pingResults[prov.name] = { loading: true };
    paint();
    try {
      const res = await api.testProvider(prov.name, targetModel);
      pingResults[prov.name] = {
        loading: false,
        ok: Boolean(res.ok),
        status: res.status,
        latency_ms: res.latency_ms,
        sample: res.sample,
        error: res.error,
      };
    } catch (e) {
      pingResults[prov.name] = {
        loading: false,
        ok: false,
        error: e.message || '请求失败',
      };
    }
    paint();
  };

  const openProviderDrawer = (index) => {
    const list = store.getSection('providers') || [];
    const isNew = index === -1;
    const orig = isNew
      ? { name: '', base_url: '', target: 'local', timeout: '60s', models: [], key_ref: '', plugin: '', system_prompt: '', extra_body: undefined, pricing: {}, has_key: false }
      : clone(list[index]);

    const existsOnServer = !isNew && (store.getServerSection('providers') || []).some((p) => p.name === orig.name);
    let extraBodyState = { valid: true, value: orig.extra_body };
    let pricingRows = Object.entries(orig.pricing || {}).map(([m, pr]) => ({
      model: m,
      input: pr?.input ?? 0,
      output: pr?.output ?? 0,
    }));

    const errBanner = el('div', { className: 'banner banner-err', style: { display: 'none' } });
    const nameIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.name || '',
      placeholder: '例如 deepseek-local 或 openai-cloud',
      dataset: { fieldPath: `providers[${isNew ? list.length : index}].name` },
    });
    const urlIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.base_url || '',
      placeholder: 'http://127.0.0.1:8000/v1 或 https://api.deepseek.com/v1',
      dataset: { fieldPath: `providers[${isNew ? list.length : index}].base_url` },
    });
    const timeoutIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.timeout || '',
      placeholder: '60s（留空采用默认值）',
    });
    const keyRefIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.key_ref || '',
      placeholder: 'env:VAR 或 db:渠道名',
    });
    const pluginIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.plugin || '',
      placeholder: '可选插件名（变更插件字段需重启网关）',
    });
    const sysPromptIn = el('textarea', {
      className: 'textarea',
      value: orig.system_prompt || '',
      placeholder: '可选：自动注入到请求前部的系统提示词',
      style: { minHeight: '72px' },
    });

    const dbKeyIn = el('input', {
      className: 'input mono',
      type: 'password',
      placeholder: orig.has_key ? '已存储密钥，输入新密钥将覆盖' : '输入上游 API Key 直接加密存入数据库',
    });
    const dbKeyStatus = el('span', { className: `pill ${orig.has_key ? 'pill-ok' : ''}` },
      orig.has_key ? '数据库密钥已就绪' : '数据库暂无密钥'
    );

    const pricingBox = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } });
    const renderPricing = () => {
      clear(pricingBox);
      pricingRows.forEach((row, rIdx) => {
        pricingBox.appendChild(
          el('div', { style: { display: 'grid', gridTemplateColumns: '1.4fr 0.8fr 0.8fr auto', gap: '8px', alignItems: 'center' } },
            el('input', {
              className: 'input input-sm mono',
              placeholder: '模型名',
              value: row.model,
              onInput: (e) => { row.model = e.target.value; },
            }),
            el('input', {
              className: 'input input-sm num',
              type: 'number',
              step: '0.01',
              placeholder: '输入 $/1M',
              value: String(row.input),
              onInput: (e) => { row.input = Number(e.target.value || 0); },
            }),
            el('input', {
              className: 'input input-sm num',
              type: 'number',
              step: '0.01',
              placeholder: '输出 $/1M',
              value: String(row.output),
              onInput: (e) => { row.output = Number(e.target.value || 0); },
            }),
            el('button', {
              type: 'button',
              className: 'icon-btn',
              'aria-label': '删除定价行',
              onClick: () => {
                pricingRows.splice(rIdx, 1);
                renderPricing();
              },
            }, icon('close', 14))
          )
        );
      });
      pricingBox.appendChild(
        el('button', {
          type: 'button',
          className: 'btn btn-sm',
          style: { alignSelf: 'flex-start' },
          onClick: () => {
            pricingRows.push({ model: (orig.models || [])[0] || '', input: 0, output: 0 });
            renderPricing();
          },
        }, icon('plus', 13), '添加计费定价')
      );
    };
    renderPricing();

    const saveDraft = () => {
      const name = nameIn.value.trim();
      const baseURL = urlIn.value.trim();
      if (!name) {
        errBanner.textContent = '请填写渠道名称';
        errBanner.style.display = 'flex';
        nameIn.focus();
        return;
      }
      if (!pluginIn.value.trim() && (!orig.tiered || orig.tiered.mode === 'shadow') && !baseURL) {
        errBanner.textContent = orig.tiered?.mode === 'shadow'
          ? 'Shadow 分层模式必须填写 Base URL'
          : '非插件渠道必须填写 Base URL';
        errBanner.style.display = 'flex';
        urlIn.focus();
        return;
      }
      if (!extraBodyState.valid) {
        errBanner.textContent = 'Extra Body 必须为合法 JSON 对象';
        errBanner.style.display = 'flex';
        return;
      }
      const dup = list.some((p, i) => p.name === name && i !== index);
      if (dup) {
        errBanner.textContent = `渠道名称 "${name}" 已存在`;
        errBanner.style.display = 'flex';
        return;
      }

      const nextObj = {
        name,
        target: orig.target || 'local',
        models: orig.models || [],
      };
      if (baseURL) nextObj.base_url = baseURL;
      if (timeoutIn.value.trim()) nextObj.timeout = timeoutIn.value.trim();
      if (keyRefIn.value.trim()) nextObj.key_ref = keyRefIn.value.trim();
      if (pluginIn.value.trim()) nextObj.plugin = pluginIn.value.trim();
      if (sysPromptIn.value) nextObj.system_prompt = sysPromptIn.value;
      if (extraBodyState.value && Object.keys(extraBodyState.value).length) {
        nextObj.extra_body = extraBodyState.value;
      }
      if (orig.has_key) nextObj.has_key = true;
      if (orig.tiered) nextObj.tiered = clone(orig.tiered);

      const pricingMap = {};
      pricingRows.forEach((r) => {
        const m = r.model.trim();
        if (m) pricingMap[m] = { input: Number(r.input || 0), output: Number(r.output || 0) };
      });
      if (Object.keys(pricingMap).length) nextObj.pricing = pricingMap;

      if (isNew) list.push(nextObj);
      else list[index] = nextObj;

      store.setDraft('providers', list);
      closeDrawer();
      toast('渠道修改已暂存至待应用栏', 'ok');
      paint();
    };

    const keySection = existsOnServer
      ? el('div', { className: 'sublist-item' },
        el('div', { className: 'sublist-head' },
          el('strong', { style: { fontSize: '13px' } }, '数据库密钥直写（不写入配置文件明文）'),
          dbKeyStatus
        ),
        el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
          dbKeyIn,
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: async () => {
              const secVal = dbKeyIn.value.trim();
              if (!secVal) {
                toast('请输入要存入数据库的上游密钥', 'warn');
                return;
              }
              try {
                await api.putProviderKey(orig.name, secVal, store.server?.hash || '');
                dbKeyIn.value = '';
                keyRefIn.value = `db:${orig.name}`;
                orig.has_key = true;
                dbKeyStatus.textContent = '数据库密钥已就绪';
                dbKeyStatus.className = 'pill pill-ok';
                await store.refresh();
                toast(`渠道 "${orig.name}" 密钥已加密保存并生效`, 'ok');
              } catch (e) {
                toast(`保存密钥失败: ${e.message}`, 'err');
              }
            },
          }, '保存数据库密钥'),
          orig.has_key
            ? el('button', {
              type: 'button',
              className: 'btn btn-danger btn-sm',
              onClick: async () => {
                try {
                  await api.deleteProviderKey(orig.name);
                  orig.has_key = false;
                  dbKeyStatus.textContent = '数据库暂无密钥';
                  dbKeyStatus.className = 'pill';
                  await store.refresh();
                  toast(`已清除 "${orig.name}" 的数据库密钥`, 'ok');
                } catch (e) {
                  toast(`清除失败: ${e.message}`, 'err');
                }
              },
            }, '清除密钥')
            : null
        )
      )
      : el('p', { className: 'field-hint' }, '提示：新建渠道请先“暂存并应用”保存配置后，再通过编辑直接写入加密数据库密钥，或先填 env:环境变量名。');

    openDrawer({
      title: isNew ? '添加模型渠道' : `编辑渠道 / ${orig.name}`,
      body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        errBanner,
        el('div', { className: 'field-row' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '渠道名称'),
            nameIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '网络位置 (Target)'),
            createSegmented({
              ariaLabel: '渠道位置',
              value: orig.target || 'local',
              options: [
                { value: 'local', label: '本地内网 local', tone: 'ok' },
                { value: 'external', label: '外部云端 external', tone: 'mask' },
              ],
              onChange: (v) => { orig.target = v; },
            })
          )
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, 'Base URL'),
          urlIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '支持模型列表'),
          createTagInput({
            values: orig.models || [],
            placeholder: '输入模型名称后按回车（如 deepseek-chat）',
            onChange: (vals) => { orig.models = vals; },
          })
        ),
        el('div', { style: { display: 'grid', gridTemplateColumns: '2fr 1fr', gap: '12px' } },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, 'Key Ref 引用'),
            keyRefIn,
            el('p', { className: 'field-hint' }, '支持 env:VAR 或 db:渠道名')
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '超时时间'),
            timeoutIn
          )
        ),
        keySection,
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '系统提示词注入 (system_prompt)'),
          sysPromptIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, 'Extra Body（附加 JSON 字段）'),
          createJsonTextarea({
            value: orig.extra_body,
            placeholder: '{"chat_template_kwargs": {"enable_thinking": true}}',
            onChange: (res) => { extraBodyState = res; },
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '插件名（可选）'),
          pluginIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '模型计费参考 (USD / 1M tokens)'),
          pricingBox
        )
      ),
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: closeDrawer }, '取消'),
        el('button', { type: 'button', className: 'btn btn-primary', onClick: saveDraft }, '暂存修改'),
      ],
    });
  };

  const paint = () => {
    clear(root);
    const providers = store.getSection('providers') || [];
    const runtimeMap = {};
    (runtime?.providers || []).forEach((rp) => { runtimeMap[rp.name] = rp; });

    const filtered = providers.filter((p) => {
      if (!filterText.trim()) return true;
      const q = filterText.trim().toLowerCase();
      return (p.name || '').toLowerCase().includes(q)
        || (p.base_url || '').toLowerCase().includes(q)
        || (p.models || []).some((m) => m.toLowerCase().includes(q));
    });

    const toolbar = el('div', { className: 'section-bar' },
      el('div', { className: 'section-bar-left' },
        el('input', {
          className: 'input',
          type: 'search',
          placeholder: '按渠道名、地址或模型过滤...',
          value: filterText,
          style: { width: '250px' },
          onInput: (e) => {
            filterText = e.target.value;
            paint();
          },
        }),
        store.isDirty('providers') ? el('span', { className: 'pill pill-brass' }, '含未应用修改') : null
      ),
      el('div', { className: 'section-bar-right' },
        el('button', {
          type: 'button',
          className: 'btn',
          onClick: () => {
            providers.forEach((p) => pingProvider(p));
          },
        }, icon('ping', 14), '全部探测'),
        el('button', {
          type: 'button',
          className: 'btn btn-primary',
          onClick: () => openProviderDrawer(-1),
        }, icon('plus', 14), '添加渠道')
      )
    );

    const table = filtered.length
      ? el('div', { className: 'sheet' },
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, '渠道与位置'),
                el('th', null, '接口地址 / 超时'),
                el('th', null, '模型与实时健康'),
                el('th', null, '密钥凭证'),
                el('th', null, '连通探测'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...filtered.map((p) => {
                const idx = providers.indexOf(p);
                const rt = runtimeMap[p.name];
                const healthByModel = {};
                (rt?.models || []).forEach((m) => { healthByModel[m.name] = m.healthy; });

                const keyPill = p.key_ref?.startsWith('db:')
                  ? el('span', { className: `pill nowrap ${p.has_key ? 'pill-ok' : 'pill-warn'}` }, p.has_key ? '数据库密钥' : '数据库密钥缺失')
                  : p.key_ref
                    ? el('span', { className: 'pill mono nowrap' }, p.key_ref)
                    : el('span', { className: 'muted nowrap' }, '无');

                const pr = pingResults[p.name];
                let pingNode = el('span', { className: 'muted nowrap', style: { fontSize: '12px' } }, '未探测');
                if (pr?.loading) {
                  pingNode = el('span', { className: 'pill nowrap' }, '探测中...');
                } else if (pr && pr.ok) {
                  pingNode = el('span', { className: 'pill pill-ok num nowrap', title: pr.sample || '' }, `正常 ${Math.round(pr.latency_ms)}ms`);
                } else if (pr && !pr.ok) {
                  pingNode = el('span', { className: 'pill pill-err pill-ellipsis', title: pr.error || '连接失败' }, '连接失败');
                }

                const refs = findProviderReferences(p.name);

                return el('tr', null,
                  el('td', { className: 'nowrap' },
                    el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
                      el('strong', null, p.name),
                      el('span', { className: `pill ${p.target === 'external' ? 'pill-brass' : 'pill-ok'}` },
                        p.target === 'external' ? 'external' : 'local'
                      ),
                      (p.tiered || rt?.has_tiered)
                        ? el('span', { className: 'pill pill-brass' }, '智能分层')
                        : null
                    ),
                    p.plugin ? el('div', { className: 'muted mono', style: { fontSize: '12px' } }, `plugin: ${p.plugin}`) : null
                  ),
                  el('td', { style: { minWidth: '180px' } },
                    el('div', { className: 'mono wrap-any', style: { fontSize: '12px' } }, p.base_url || '-'),
                    el('div', { className: 'muted num nowrap', style: { fontSize: '12px' } }, `超时 ${p.timeout || '默认 60s'}`)
                  ),
                  el('td', null,
                    el('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '5px', maxWidth: '280px' } },
                      ...(p.models || []).map((m) => {
                        const hasH = m in healthByModel;
                        const okH = healthByModel[m];
                        return el('span', { className: `tag-chip nowrap${hasH ? (okH ? '' : ' pill-err') : ''}` },
                          hasH ? el('span', { className: 'dot', style: { color: okH ? 'var(--verdigris)' : 'var(--oxblood)' } }) : null,
                          m
                        );
                      })
                    )
                  ),
                  el('td', null, keyPill),
                  el('td', null, pingNode),
                  el('td', { className: 'td-right' },
                    el('div', { style: { display: 'inline-flex', gap: '6px' } },
                      el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        disabled: Boolean(pr?.loading),
                        onClick: () => pingProvider(p),
                      }, '测试'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-sm',
                        onClick: () => openProviderDrawer(idx),
                      }, '编辑'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-danger btn-sm',
                        onClick: async () => {
                          if (refs.length) {
                            await confirmModal({
                              title: '渠道仍被引用，无法直接删除',
                              message: `渠道 “${p.name}” 当前仍被以下策略引用：${refs.join('、')}。请先到“路由与别名”页面移除关联成员或别名后再删除。`,
                              confirmText: '知道了',
                            });
                            return;
                          }
                          const ok = await confirmModal({
                            title: '暂存删除渠道',
                            message: `确定从草稿中删除渠道 “${p.name}”？点击底部“应用并热重载”后正式生效。`,
                            confirmText: '从草稿移除',
                            danger: true,
                          });
                          if (!ok) return;
                          const next = clone(providers);
                          next.splice(idx, 1);
                          store.setDraft('providers', next);
                          paint();
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
        el('div', { className: 'sheet-body muted' }, '尚未配置上游渠道。点击“添加渠道”配置本地 DeepSeek vLLM 或云端服务。')
      );

    // Hourly Model Health Summary if available
    const hourlyStats = runtime?.hourly_stats || [];
    const statsSheet = hourlyStats.length
      ? el('section', { className: 'sheet' },
        el('div', { className: 'sheet-head' },
          el('div', { className: 'sheet-title-group' },
            el('h3', { className: 'sheet-title' }, '近 1 小时各渠道模型健康度'),
            el('p', { className: 'sheet-sub' }, '真实流量统计的成功率与延迟')
          )
        ),
        el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, '渠道/模型'),
                el('th', { className: 'th-right' }, '请求数'),
                el('th', { className: 'th-right' }, '失败数'),
                el('th', { className: 'th-right' }, '成功率'),
                el('th', { className: 'th-right' }, '平均延迟'),
                el('th', { className: 'th-right' }, '平均 TTFT')
              )
            ),
            el('tbody', null,
              ...hourlyStats.map((st) =>
                el('tr', null,
                  el('td', { className: 'mono' }, st.model),
                  el('td', { className: 'td-right num' }, fmtNum(st.requests)),
                  el('td', { className: 'td-right num' }, fmtNum(st.errors)),
                  el('td', { className: 'td-right num' },
                    el('span', { className: `pill ${st.success_rate >= 95 ? 'pill-ok' : st.success_rate >= 70 ? 'pill-warn' : 'pill-err'}` },
                      `${Number(st.success_rate || 0).toFixed(1)}%`
                    )
                  ),
                  el('td', { className: 'td-right num' }, `${Math.round(st.avg_latency_ms || 0)}ms`),
                  el('td', { className: 'td-right num' }, `${Math.round(st.avg_ttft_ms || 0)}ms`)
                )
              )
            )
          )
        )
      )
      : null;

    root.append(toolbar, table, statsSheet);
  };

  loadRuntime();
}
