import { icon } from '/admin/js/icons.js';
import {
  clear,
  closeDrawer,
  clone,
  confirmModal,
  createSegmented,
  el,
  openDrawer,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

function availableModelRefs() {
  const providers = store.getSection('providers') || [];
  const refs = [];
  providers.forEach((p) => {
    (p.models || []).forEach((m) => refs.push(`${p.name}/${m}`));
  });
  return refs;
}

export function renderRouting(container) {
  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  container.appendChild(root);

  const openGroupDrawer = (origName = null) => {
    const routing = store.getSection('routing') || { default_group: '', routing_groups: {} };
    const groups = routing.routing_groups || {};
    const isNew = !origName;
    const orig = isNew
      ? { type: 'order', members: [], weights: {}, classifier: '', classifier_timeout: '', rules: [] }
      : clone(groups[origName] || { type: 'order', members: [] });
    if (!orig.members) orig.members = [];
    if (!orig.weights) orig.weights = {};
    if (!orig.rules) orig.rules = [];

    const suggestions = availableModelRefs();
    const errBanner = el('div', { className: 'banner banner-err', style: { display: 'none' } });
    const nameIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: origName || '',
      placeholder: '例如 main 或 coding-pool',
    });
    const clsIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.classifier || '',
      placeholder: '可选：用于意图的分类器如 local/small',
      list: 'gh-model-ref-list',
    });
    const clsTimeoutIn = el('input', {
      className: 'input mono',
      type: 'text',
      value: orig.classifier_timeout || '',
      placeholder: '2s',
    });

    const dataList = el('datalist', { id: 'gh-model-ref-list' },
      ...suggestions.map((s) => el('option', { value: s }))
    );

    const membersBox = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } });
    const newMemberIn = el('input', {
      className: 'input input-sm mono',
      placeholder: '选择或输入 provider/model',
      list: 'gh-model-ref-list',
    });

    const rulesBox = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } });

    const renderMembers = () => {
      clear(membersBox);
      if (!orig.members.length) {
        membersBox.appendChild(el('p', { className: 'field-hint' }, '暂无成员，请在下方添加至少一个 provider/model'));
      }
      orig.members.forEach((mem, i) => {
        const weightInput = orig.type === 'usage'
          ? el('input', {
            className: 'input input-sm num',
            type: 'number',
            min: 0,
            step: '0.1',
            value: String(orig.weights?.[mem] ?? 1),
            style: { width: '76px' },
            title: '权重',
            onInput: (e) => {
              if (!orig.weights) orig.weights = {};
              orig.weights[mem] = Number(e.target.value || 1);
            },
          })
          : null;

        membersBox.appendChild(
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '6px' } },
            el('span', { className: 'muted num', style: { width: '20px', fontSize: '12px' } }, `${i + 1}.`),
            el('input', {
              className: 'input input-sm mono',
              value: mem,
              list: 'gh-model-ref-list',
              onInput: (e) => {
                const oldKey = orig.members[i];
                const newVal = e.target.value.trim();
                orig.members[i] = newVal;
                if (orig.weights && oldKey in orig.weights && oldKey !== newVal) {
                  orig.weights[newVal] = orig.weights[oldKey];
                  delete orig.weights[oldKey];
                }
              },
            }),
            weightInput,
            el('button', {
              type: 'button',
              className: 'icon-btn',
              disabled: i === 0,
              'aria-label': '上移',
              onClick: () => {
                [orig.members[i - 1], orig.members[i]] = [orig.members[i], orig.members[i - 1]];
                renderMembers();
              },
            }, icon('arrowUp', 13)),
            el('button', {
              type: 'button',
              className: 'icon-btn',
              disabled: i === orig.members.length - 1,
              'aria-label': '下移',
              onClick: () => {
                [orig.members[i], orig.members[i + 1]] = [orig.members[i + 1], orig.members[i]];
                renderMembers();
              },
            }, icon('arrowDown', 13)),
            el('button', {
              type: 'button',
              className: 'icon-btn',
              'aria-label': '删除成员',
              onClick: () => {
                const removed = orig.members.splice(i, 1)[0];
                if (orig.weights && removed) delete orig.weights[removed];
                renderMembers();
              },
            }, icon('close', 13))
          )
        );
      });
    };

    const renderRules = () => {
      clear(rulesBox);
      orig.rules.forEach((rule, idx) => {
        rulesBox.appendChild(
          el('div', { className: 'sublist-item' },
            el('div', { className: 'sublist-head' },
              el('strong', { style: { fontSize: '13px' } }, `规则 ${idx + 1}: ${rule.name || '(未命名)'}`),
              el('div', { style: { display: 'flex', gap: '4px' } },
                el('button', {
                  type: 'button',
                  className: 'icon-btn',
                  disabled: idx === 0,
                  'aria-label': '规则上移',
                  onClick: () => {
                    [orig.rules[idx - 1], orig.rules[idx]] = [orig.rules[idx], orig.rules[idx - 1]];
                    renderRules();
                  },
                }, icon('arrowUp', 13)),
                el('button', {
                  type: 'button',
                  className: 'icon-btn',
                  disabled: idx === orig.rules.length - 1,
                  'aria-label': '规则下移',
                  onClick: () => {
                    [orig.rules[idx], orig.rules[idx + 1]] = [orig.rules[idx + 1], orig.rules[idx]];
                    renderRules();
                  },
                }, icon('arrowDown', 13)),
                el('button', {
                  type: 'button',
                  className: 'icon-btn',
                  'aria-label': '删除规则',
                  onClick: () => {
                    orig.rules.splice(idx, 1);
                    renderRules();
                  },
                }, icon('trash', 13))
              )
            ),
            el('div', { className: 'field-row' },
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '规则名称'),
                el('input', {
                  className: 'input input-sm',
                  placeholder: '例如 long-input',
                  value: rule.name || '',
                  onInput: (e) => { rule.name = e.target.value; },
                })
              ),
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '目标模型'),
                el('input', {
                  className: 'input input-sm mono',
                  placeholder: 'provider/model',
                  value: rule.use || '',
                  list: 'gh-model-ref-list',
                  onInput: (e) => { rule.use = e.target.value; },
                })
              )
            ),
            el('div', { className: 'field' },
              el('span', { className: 'muted', style: { fontSize: '12px' } }, '意图描述（可选，需配置分类器）'),
              el('input', {
                className: 'input input-sm',
                placeholder: '例如：多步推理或复杂代码分析',
                value: rule.intent || '',
                onInput: (e) => { rule.intent = e.target.value; },
              })
            ),
            el('div', { className: 'field-row-3' },
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, 'Token 下限'),
                el('input', {
                  className: 'input input-sm num',
                  type: 'number',
                  placeholder: '大于 N tokens',
                  value: rule.tokens_gt ? String(rule.tokens_gt) : '',
                  onInput: (e) => {
                    const v = Number(e.target.value);
                    if (v > 0) rule.tokens_gt = v; else delete rule.tokens_gt;
                  },
                })
              ),
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, 'Token 上限'),
                el('input', {
                  className: 'input input-sm num',
                  type: 'number',
                  placeholder: '小于 N tokens',
                  value: rule.tokens_lt ? String(rule.tokens_lt) : '',
                  onInput: (e) => {
                    const v = Number(e.target.value);
                    if (v > 0) rule.tokens_lt = v; else delete rule.tokens_lt;
                  },
                })
              ),
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '推理强度'),
                el('select', {
                  className: 'select input-sm',
                  onChange: (e) => {
                    if (e.target.value) rule.useEffort = e.target.value;
                    else delete rule.useEffort;
                  },
                },
                  ...[
                    { v: '', l: '默认' },
                    { v: 'low', l: 'low 低' },
                    { v: 'medium', l: 'medium 中' },
                    { v: 'high', l: 'high 高' },
                    { v: 'xhigh', l: 'xhigh 极高' },
                    { v: 'max', l: 'max 最高' },
                  ].map((o) => el('option', { value: o.v, selected: (rule.useEffort || '') === o.v }, o.l))
                )
              )
            ),
            el('div', { className: 'field-row' },
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '图片条件 (has_image)'),
                el('select', {
                  className: 'select input-sm',
                  onChange: (e) => {
                    if (e.target.value === 'true') rule.has_image = true;
                    else if (e.target.value === 'false') rule.has_image = false;
                    else delete rule.has_image;
                  },
                },
                  ...[
                    { v: '', l: '不限' },
                    { v: 'true', l: '包含图片 (true)' },
                    { v: 'false', l: '不含图片 (false)' },
                  ].map((o) => el('option', {
                    value: o.v,
                    selected: (typeof rule.has_image === 'boolean' ? String(rule.has_image) : '') === o.v,
                  }, o.l))
                )
              ),
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '推理条件 (reasoning)'),
                el('select', {
                  className: 'select input-sm',
                  onChange: (e) => {
                    if (e.target.value === 'true') rule.reasoning = true;
                    else if (e.target.value === 'false') rule.reasoning = false;
                    else delete rule.reasoning;
                  },
                },
                  ...[
                    { v: '', l: '不限' },
                    { v: 'true', l: '要求推理 (true)' },
                    { v: 'false', l: '非推理 (false)' },
                  ].map((o) => el('option', {
                    value: o.v,
                    selected: (typeof rule.reasoning === 'boolean' ? String(rule.reasoning) : '') === o.v,
                  }, o.l))
                )
              )
            ),
            el('div', { className: 'field-row' },
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '来源 Header（可选）'),
                el('input', {
                  className: 'input input-sm mono',
                  placeholder: 'X-Source 值',
                  value: rule.source || '',
                  onInput: (e) => {
                    if (e.target.value) rule.source = e.target.value; else delete rule.source;
                  },
                })
              ),
              el('div', { className: 'field' },
                el('span', { className: 'muted', style: { fontSize: '12px' } }, '生效时段（可选）'),
                el('input', {
                  className: 'input input-sm mono',
                  placeholder: '如 09:00-18:00',
                  value: rule.time || '',
                  onInput: (e) => {
                    if (e.target.value) rule.time = e.target.value; else delete rule.time;
                  },
                })
              )
            )
          )
        );
      });
      rulesBox.appendChild(
        el('button', {
          type: 'button',
          className: 'btn btn-sm',
          style: { alignSelf: 'flex-start' },
          onClick: () => {
            orig.rules.push({ name: `rule-${orig.rules.length + 1}`, use: suggestions[0] || '' });
            renderRules();
          },
        }, icon('plus', 13), '添加路由规则')
      );
    };

    renderMembers();
    renderRules();

    const saveGroup = () => {
      const gName = nameIn.value.trim();
      if (!gName) {
        errBanner.textContent = '请填写路由组名称';
        errBanner.style.display = 'flex';
        return;
      }
      const cleanMembers = orig.members.map((m) => m.trim()).filter(Boolean);
      if (!cleanMembers.length) {
        errBanner.textContent = '请至少添加一个成员模型';
        errBanner.style.display = 'flex';
        return;
      }
      if ((isNew || gName !== origName) && groups[gName]) {
        errBanner.textContent = `路由组 "${gName}" 已存在`;
        errBanner.style.display = 'flex';
        return;
      }

      const nextGroup = {
        type: orig.type || 'order',
        members: cleanMembers,
      };
      if (nextGroup.type === 'usage') {
        const w = {};
        cleanMembers.forEach((m) => {
          w[m] = Number(orig.weights?.[m] ?? 1);
        });
        nextGroup.weights = w;
      }
      if (clsIn.value.trim()) nextGroup.classifier = clsIn.value.trim();
      if (clsTimeoutIn.value.trim()) nextGroup.classifier_timeout = clsTimeoutIn.value.trim();
      const cleanRules = orig.rules.filter((r) => r.use?.trim()).map((r) => {
        const out = { use: r.use.trim() };
        if (r.name?.trim()) out.name = r.name.trim();
        if (r.intent?.trim()) out.intent = r.intent.trim();
        if (r.tokens_gt > 0) out.tokens_gt = r.tokens_gt;
        if (r.tokens_lt > 0) out.tokens_lt = r.tokens_lt;
        if (typeof r.has_image === 'boolean') out.has_image = r.has_image;
        if (typeof r.reasoning === 'boolean') out.reasoning = r.reasoning;
        if (r.source?.trim()) out.source = r.source.trim();
        if (r.time?.trim()) out.time = r.time.trim();
        if (r.useEffort) out.useEffort = r.useEffort;
        return out;
      });
      if (cleanRules.length) nextGroup.rules = cleanRules;

      if (!isNew && gName !== origName) {
        delete groups[origName];
        if (routing.default_group === origName) routing.default_group = gName;
      }
      groups[gName] = nextGroup;
      if (!routing.default_group) routing.default_group = gName;
      routing.routing_groups = groups;
      store.setDraft('routing', routing);
      closeDrawer();
      toast('路由组修改已暂存', 'ok');
      paint();
    };

    openDrawer({
      title: isNew ? '新建路由组' : `编辑路由组 / ${origName}`,
      body: el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        dataList,
        errBanner,
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '组名称'),
          nameIn
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '调度策略'),
          createSegmented({
            ariaLabel: '调度策略',
            value: orig.type || 'order',
            options: [
              { value: 'order', label: '顺序故障转移' },
              { value: 'usage', label: '加权负载均衡' },
            ],
            onChange: (v) => {
              orig.type = v;
              renderMembers();
            },
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '成员序列（provider/model）'),
          membersBox,
          el('div', { style: { display: 'flex', gap: '8px', marginTop: '6px' } },
            newMemberIn,
            el('button', {
              type: 'button',
              className: 'btn btn-sm',
              onClick: () => {
                const v = newMemberIn.value.trim();
                if (!v) return;
                orig.members.push(v);
                newMemberIn.value = '';
                renderMembers();
              },
            }, '加入')
          )
        ),
        el('div', { className: 'field-row' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '意图分类模型 (可选)'),
            clsIn
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '分类器超时'),
            clsTimeoutIn
          )
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '条件路由规则 (按序匹配)'),
          rulesBox
        )
      ),
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: closeDrawer }, '取消'),
        el('button', { type: 'button', className: 'btn btn-primary', onClick: saveGroup }, '暂存修改'),
      ],
    });
  };

  const paint = () => {
    clear(root);
    const routing = store.getSection('routing') || { default_group: '', routing_groups: {} };
    const groups = routing.routing_groups || {};
    const groupNames = Object.keys(groups);
    const aliases = store.getSection('aliases') || [];
    const modelRefs = availableModelRefs();
    const aliasTargetList = [...modelRefs, ...groupNames];

    // Band 1: Routing Groups
    const defaultSelect = el('select', {
      className: 'select',
      style: { width: '180px' },
      'aria-label': '默认路由组',
      onChange: (e) => {
        const next = clone(routing);
        next.default_group = e.target.value;
        store.setDraft('routing', next);
        paint();
      },
    },
      el('option', { value: '' }, '(无默认组)'),
      ...groupNames.map((gn) => el('option', { value: gn, selected: gn === routing.default_group }, gn))
    );

    const groupsSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '路由组'),
          el('p', { className: 'sheet-sub' }, '配置默认组、主备故障转移顺序、加权负载与智能规则路由')
        ),
        el('div', { className: 'sheet-actions' },
          el('span', { className: 'muted', style: { fontSize: '13px' } }, '默认路由组'),
          defaultSelect,
          el('button', { type: 'button', className: 'btn btn-primary btn-sm', onClick: () => openGroupDrawer(null) }, icon('plus', 14), '新建路由组')
        )
      ),
      groupNames.length
        ? el('div', { className: 'table-wrap' },
          el('table', { className: 'data-table' },
            el('thead', null,
              el('tr', null,
                el('th', null, '路由组名称'),
                el('th', null, '策略'),
                el('th', null, '成员顺序与权重'),
                el('th', null, '分类器与规则'),
                el('th', { className: 'th-right' }, '操作')
              )
            ),
            el('tbody', null,
              ...groupNames.map((gn) => {
                const g = groups[gn];
                const isDef = gn === routing.default_group;
                return el('tr', null,
                  el('td', null,
                    el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
                      el('strong', { className: 'mono' }, gn),
                      isDef ? el('span', { className: 'pill pill-brass' }, '默认') : null
                    )
                  ),
                  el('td', null,
                    el('span', { className: 'pill' }, g.type === 'usage' ? '加权均衡' : '顺序故障转移')
                  ),
                  el('td', null,
                    el('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '5px', maxWidth: '340px' } },
                      ...(g.members || []).map((m, idx) =>
                        el('span', { className: 'tag-chip' },
                          `${idx + 1}. ${m}${g.type === 'usage' && g.weights?.[m] ? ` ×${g.weights[m]}` : ''}`
                        )
                      )
                    )
                  ),
                  el('td', { style: { fontSize: '12px' } },
                    el('div', { className: 'num' }, `${(g.rules || []).length} 条规则`),
                    g.classifier ? el('div', { className: 'mono muted' }, `分类器 ${g.classifier}`) : null
                  ),
                  el('td', { className: 'td-right' },
                    el('div', { style: { display: 'inline-flex', gap: '6px' } },
                      !isDef
                        ? el('button', {
                          type: 'button',
                          className: 'btn btn-sm',
                          onClick: () => {
                            const next = clone(routing);
                            next.default_group = gn;
                            store.setDraft('routing', next);
                            paint();
                          },
                        }, '设为默认')
                        : null,
                      el('button', { type: 'button', className: 'btn btn-sm', onClick: () => openGroupDrawer(gn) }, '编辑'),
                      el('button', {
                        type: 'button',
                        className: 'btn btn-danger btn-sm',
                        onClick: async () => {
                          if (isDef) {
                            toast('请先将其他组设为默认组后再删除', 'warn');
                            return;
                          }
                          const ok = await confirmModal({
                            title: '删除路由组',
                            message: `确定从草稿中删除路由组 “${gn}”？`,
                            confirmText: '从草稿移除',
                            danger: true,
                          });
                          if (!ok) return;
                          const next = clone(routing);
                          delete next.routing_groups[gn];
                          store.setDraft('routing', next);
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
        : el('div', { className: 'sheet-body muted' }, '尚未定义路由组。')
    );

    // Band 2: Model Aliases (ordered)
    const addMatchIn = el('input', { className: 'input input-sm mono', placeholder: '匹配模式，例如 gpt-4* 或 deepseek-v3' });
    const addUseIn = el('input', { className: 'input input-sm mono', placeholder: '映射到目标（provider/model 或路由组）', list: 'gh-alias-targets' });

    const aliasesSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '模型别名映射'),
          el('p', { className: 'sheet-sub' }, '自上而下按第一条匹配生效，适合将客户端请求的外部模型名映射到本地部署模型或路由组')
        ),
        store.isDirty('aliases') ? el('span', { className: 'pill pill-brass' }, '别名已修改') : null
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '14px' } },
        el('datalist', { id: 'gh-alias-targets' },
          ...aliasTargetList.map((t) => el('option', { value: t }))
        ),
        aliases.length
          ? el('div', { className: 'table-wrap', style: { border: '1px solid var(--border)', borderRadius: 'var(--radius)' } },
            el('table', { className: 'data-table' },
              el('thead', null,
                el('tr', null,
                  el('th', { style: { width: '56px' } }, '顺序'),
                  el('th', null, '请求模型匹配 (match)'),
                  el('th', null, '实际目标 (use)'),
                  el('th', { className: 'th-right' }, '调整')
                )
              ),
              el('tbody', null,
                ...aliases.map((a, idx) =>
                  el('tr', null,
                    el('td', { className: 'num muted' }, `#${idx + 1}`),
                    el('td', null,
                      el('input', {
                        className: 'input input-sm mono',
                        value: a.match || '',
                        onChange: (e) => {
                          const next = clone(aliases);
                          next[idx].match = e.target.value.trim();
                          store.setDraft('aliases', next);
                        },
                      })
                    ),
                    el('td', null,
                      el('input', {
                        className: 'input input-sm mono',
                        value: a.use || '',
                        list: 'gh-alias-targets',
                        onChange: (e) => {
                          const next = clone(aliases);
                          next[idx].use = e.target.value.trim();
                          store.setDraft('aliases', next);
                        },
                      })
                    ),
                    el('td', { className: 'td-right' },
                      el('div', { style: { display: 'inline-flex', gap: '4px' } },
                        el('button', {
                          type: 'button',
                          className: 'icon-btn',
                          disabled: idx === 0,
                          'aria-label': '上移别名',
                          onClick: () => {
                            const next = clone(aliases);
                            [next[idx - 1], next[idx]] = [next[idx], next[idx - 1]];
                            store.setDraft('aliases', next);
                            paint();
                          },
                        }, icon('arrowUp', 13)),
                        el('button', {
                          type: 'button',
                          className: 'icon-btn',
                          disabled: idx === aliases.length - 1,
                          'aria-label': '下移别名',
                          onClick: () => {
                            const next = clone(aliases);
                            [next[idx], next[idx + 1]] = [next[idx + 1], next[idx]];
                            store.setDraft('aliases', next);
                            paint();
                          },
                        }, icon('arrowDown', 13)),
                        el('button', {
                          type: 'button',
                          className: 'icon-btn',
                          'aria-label': '删除别名',
                          onClick: () => {
                            const next = clone(aliases);
                            next.splice(idx, 1);
                            store.setDraft('aliases', next);
                            paint();
                          },
                        }, icon('trash', 13))
                      )
                    )
                  )
                )
              )
            )
          )
          : el('p', { className: 'muted', style: { margin: 0 } }, '当前没有配置模型别名。'),
        el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr auto', gap: '8px', alignItems: 'center' } },
          addMatchIn,
          addUseIn,
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: () => {
              const m = addMatchIn.value.trim();
              const u = addUseIn.value.trim();
              if (!m || !u) {
                toast('请填写匹配模式和目标标识', 'warn');
                return;
              }
              const next = clone(aliases);
              next.push({ match: m, use: u });
              store.setDraft('aliases', next);
              paint();
            },
          }, icon('plus', 13), '新增别名')
        )
      )
    );

    root.append(groupsSheet, aliasesSheet);
  };

  paint();
}
