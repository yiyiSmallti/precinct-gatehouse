import { api } from '/admin/js/api.js';
import { icon } from '/admin/js/icons.js';
import {
  clear,
  clone,
  createSegmented,
  createSwitch,
  createTagInput,
  el,
  fmtNum,
  toast,
} from '/admin/js/ui.js';
import { store } from '/admin/js/store.js';

const MODE_LABELS = {
  off: { text: '关闭', cls: 'pill nowrap' },
  mask: { text: '掩码', cls: 'pill pill-brass nowrap' },
  block: { text: '阻断', cls: 'pill pill-err nowrap' },
};

const CAT_CN_NAMES = {
  phone: '手机号',
  id_card: '身份证号',
  bank_card: '银行卡号',
  uscc: '统一社会信用代码',
  plate: '车牌号',
  email: '电子邮箱',
  ip: 'IP 地址',
  domain: '内部域名',
  secret: '凭据与密钥',
  term: '自定义词表',
  image: '图片敏感信息',
};

function computePreviewMatrix(modeByTarget = {}) {
  const localMode = modeByTarget.local || 'mask';
  const extMode = modeByTarget.external || 'mask';
  return {
    local: {
      public: localMode,
      internal: localMode,
      confidential: localMode,
    },
    external: {
      public: extMode,
      internal: extMode,
      confidential: 'block',
    },
  };
}

export function renderRedaction(container) {
  let rtInfo = null;
  let tryTarget = 'external';
  let tryClass = 'internal';
  let tryText = '工单号 #8821：联系人李四，手机 13812345678，备用密钥 sk-proj-998877665544332211，内网接口 http://core-db.corp.local/api';
  let tryOutput = null;
  let tryBusy = false;

  const root = el('div');
  container.appendChild(root);

  const load = async () => {
    try {
      rtInfo = await api.redaction();
    } catch {
      rtInfo = null;
    }
    paint();
  };

  const paintTryBox = (host) => {
    clear(host);
    const hasDraft = store.isDirty('redaction');

    const ta = el('textarea', {
      className: 'textarea mono',
      value: tryText,
      placeholder: '输入待检测的提示词或日志文本...',
      style: { minHeight: '116px', fontSize: '12px' },
      onInput: (e) => { tryText = e.target.value; },
    });

    const runTry = async () => {
      if (!ta.value.trim()) {
        toast('请输入演练文本', 'warn');
        return;
      }
      tryBusy = true;
      paintTryBox(host);
      try {
        tryOutput = await api.redactionTry({
          text: ta.value,
          target: tryTarget,
          data_class: tryClass,
        });
      } catch (e) {
        toast(`演练失败: ${e.message}`, 'err');
      } finally {
        tryBusy = false;
        paintTryBox(host);
      }
    };

    const resultBlock = [];
    if (tryOutput) {
      const mInfo = MODE_LABELS[tryOutput.mode] || { text: tryOutput.mode, cls: 'pill' };
      const counts = tryOutput.report?.counts || {};
      const countBadges = Object.entries(counts)
        .filter(([, v]) => Number(v) > 0)
        .map(([k, v]) => el('span', { className: 'pill mono num' }, `${k}: ${v}`));
      const alerts = tryOutput.report?.alerts || [];

      resultBlock.push(
        el('div', { className: 'try-result' },
          el('div', { style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' } },
            el('span', { style: { fontSize: '12px', fontWeight: 600 } }, '演练结果'),
            el('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } },
              el('span', { className: mInfo.cls }, mInfo.text),
              tryOutput.report?.blocked ? el('span', { className: 'pill pill-err' }, '已触发阻断') : null,
              ...countBadges
            )
          ),
          alerts.length
            ? el('div', { className: 'banner banner-warn', style: { padding: '8px 10px', fontSize: '12px' } }, alerts.join('；'))
            : null,
          tryOutput.error
            ? el('div', { className: 'banner banner-err', style: { padding: '8px 10px', fontSize: '12px' } }, tryOutput.error)
            : null,
          el('pre', { className: 'try-pre' },
            tryOutput.report?.blocked
              ? '(请求在发往上游前被拦截，不会传出任何内容)'
              : (tryOutput.text || '(空)')
          )
        )
      );
    }

    host.appendChild(
      el('div', { className: 'sheet sticky-pane' },
        el('div', { className: 'sheet-head' },
          el('div', { className: 'sheet-title-group' },
            el('h3', { className: 'sheet-title' }, '实时脱敏演练'),
            el('p', { className: 'sheet-sub' }, '验证当前已应用策略的掩码或拦截效果')
          )
        ),
        el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
          hasDraft
            ? el('div', { className: 'banner banner-brass', style: { padding: '8px 11px', fontSize: '12px' } },
              el('div', { className: 'banner-main' },
                icon('alert', 14),
                el('span', null, '演练调用后端生效规则；您刚修改的草稿需点击底部“应用并热重载”后才会在此生效。')
              )
            )
            : null,
          el('div', { className: 'field' },
            el('span', { className: 'field-label' }, '目标网络位置'),
            createSegmented({
              ariaLabel: '演练目标位置',
              value: tryTarget,
              options: [
                { value: 'local', label: '本地 local' },
                { value: 'external', label: '外部 external' },
              ],
              onChange: (v) => { tryTarget = v; },
            })
          ),
          el('div', { className: 'field' },
            el('span', { className: 'field-label' }, '数据密级 (Data Class)'),
            createSegmented({
              ariaLabel: '演练数据密级',
              value: tryClass,
              options: [
                { value: 'public', label: '公开 public' },
                { value: 'internal', label: '内部 internal' },
                { value: 'confidential', label: '机密 confidential' },
              ],
              onChange: (v) => { tryClass = v; },
            })
          ),
          el('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } },
            el('button', {
              type: 'button',
              className: 'tag-sug-btn',
              onClick: () => {
                tryText = '联系人张三 13900001234，sk-live-secret9988776655，访问内部主机 db.corp.local';
                paintTryBox(host);
              },
            }, '示例: 手机与密钥'),
            el('button', {
              type: 'button',
              className: 'tag-sug-btn',
              onClick: () => {
                tryText = '身份证号 110101199001011234，工行卡 6222021001123456789，邮箱 zhangsan@corp.com';
                paintTryBox(host);
              },
            }, '示例: 证件与银行卡')
          ),
          ta,
          el('button', {
            type: 'button',
            className: 'btn btn-primary',
            disabled: tryBusy,
            onClick: runTry,
          }, tryBusy ? '检测中...' : '执行脱敏测试'),
          ...resultBlock
        )
      )
    );
  };

  const paint = () => {
    clear(root);
    const draft = store.getSection('redaction');
    const modeByTarget = {
      local: draft.mode_by_target?.local || 'off',
      external: draft.mode_by_target?.external || 'mask',
    };
    const matrix = computePreviewMatrix(modeByTarget);
    const cats = rtInfo?.categories || [
      { key: 'phone', name: '手机号', description: '中国大陆移动电话号码' },
      { key: 'id_card', name: '身份证号', description: '18 位居民身份证号码' },
      { key: 'bank_card', name: '银行卡号', description: '银行卡 PAN 账号' },
      { key: 'uscc', name: '统一社会信用代码', description: '18 位企业统一社会信用代码' },
      { key: 'plate', name: '车牌号', description: '中国大陆民用车辆牌照' },
      { key: 'email', name: '电子邮箱', description: '电子邮件地址' },
      { key: 'ip', name: 'IP 地址', description: 'IPv4 与内网主机地址' },
      { key: 'domain', name: '内部域名', description: '匹配 internal_domains 的域名' },
      { key: 'secret', name: '凭据与密钥', description: 'API Key、私钥与访问令牌' },
      { key: 'term', name: '自定义词表', description: '命中字典文件与敏感词表的专有名词' },
      { key: 'image', name: '图片敏感信息', description: '上传图片中的二维码、人脸或 OCR 敏感内容' },
    ];

    const unavailable = new Set(rtInfo?.detectors_unavailable || []);

    const updateDraft = (mutator) => {
      const next = clone(store.getSection('redaction'));
      mutator(next);
      store.setDraft('redaction', next);
      paint();
    };

    const tryHost = el('div');
    paintTryBox(tryHost);

    // 1. Target Modes + Matrix Preview Sheet
    const modeCellPill = (m) => {
      const info = MODE_LABELS[m] || { text: m, cls: 'pill' };
      return el('span', { className: info.cls }, info.text);
    };

    const targetSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '通道脱敏模式与策略矩阵'),
          el('p', { className: 'sheet-sub' }, '修改任意参数后进入待应用栏；机密数据发往外部云端始终强制拦截')
        ),
        store.isDirty('redaction') ? el('span', { className: 'pill pill-brass' }, '脱敏草稿未应用') : null
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
          el('div', { className: 'field' },
            el('span', { className: 'field-label' }, '本地渠道策略 (local)'),
            createSegmented({
              ariaLabel: '本地渠道策略',
              value: modeByTarget.local,
              options: [
                { value: 'off', label: '关闭 off' },
                { value: 'mask', label: '掩码 mask', tone: 'mask' },
                { value: 'block', label: '阻断 block', tone: 'block' },
              ],
              onChange: (v) => updateDraft((d) => {
                d.mode_by_target = { ...(d.mode_by_target || {}), local: v };
              }),
            })
          ),
          el('div', { className: 'field' },
            el('span', { className: 'field-label' }, '外部渠道策略 (external)'),
            createSegmented({
              ariaLabel: '外部渠道策略',
              value: modeByTarget.external,
              options: [
                { value: 'off', label: '关闭 off' },
                { value: 'mask', label: '掩码 mask', tone: 'mask' },
                { value: 'block', label: '阻断 block', tone: 'block' },
              ],
              onChange: (v) => updateDraft((d) => {
                d.mode_by_target = { ...(d.mode_by_target || {}), external: v };
              }),
            })
          )
        ),
        el('div', { className: 'table-wrap', style: { border: '1px solid var(--border)', borderRadius: 'var(--radius)' } },
          el('table', { className: 'data-table compact' },
            el('thead', null,
              el('tr', null,
                el('th', null, '目标位置 / 密级'),
                el('th', null, el('div', { className: 'mono' }, 'public'), el('div', { className: 'muted', style: { fontSize: '12px', fontWeight: 400 } }, '公开')),
                el('th', null, el('div', { className: 'mono' }, 'internal'), el('div', { className: 'muted', style: { fontSize: '12px', fontWeight: 400 } }, '内部')),
                el('th', null, el('div', { className: 'mono' }, 'confidential'), el('div', { className: 'muted', style: { fontSize: '12px', fontWeight: 400 } }, '机密'))
              )
            ),
            el('tbody', null,
              el('tr', null,
                el('td', { className: 'nowrap' }, el('strong', null, 'local 本地')),
                el('td', null, modeCellPill(matrix.local.public)),
                el('td', null, modeCellPill(matrix.local.internal)),
                el('td', null, modeCellPill(matrix.local.confidential))
              ),
              el('tr', null,
                el('td', { className: 'nowrap' }, el('strong', null, 'external 外部')),
                el('td', null, modeCellPill(matrix.external.public)),
                el('td', null, modeCellPill(matrix.external.internal)),
                el('td', null,
                  el('div', { style: { display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: '3px' } },
                    modeCellPill(matrix.external.confidential),
                    el('span', { className: 'muted nowrap', style: { fontSize: '12px' } }, '强制不变式')
                  )
                )
              )
            )
          )
        )
      )
    );

    // 2. Categories Table
    const categoriesSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '敏感类别动作'),
          el('p', { className: 'sheet-sub' }, '配置 11 类敏感特征在进入掩码流程时的默认处置动作')
        )
      ),
      el('div', { className: 'table-wrap' },
        el('table', { className: 'data-table compact' },
          el('thead', null,
            el('tr', null,
              el('th', null, '识别类别'),
              el('th', null, '说明'),
              el('th', { className: 'th-right' }, '处置动作')
            )
          ),
          el('tbody', null,
            ...cats.map((c) => {
              const curAction = draft.categories?.[c.key] || c.action || 'mask';
              const isImageUnavail = c.key === 'image' && unavailable.size > 0;
              const displayName = CAT_CN_NAMES[c.key] || (/^[A-Z0-9_]+$/.test(c.name || '') ? c.key : c.name);
              return el('tr', null,
                el('td', { className: 'nowrap' },
                  el('div', { style: { fontWeight: 600 } }, displayName),
                  el('div', { className: 'mono muted', style: { fontSize: '12px' } }, c.key)
                ),
                el('td', null,
                  el('div', { className: 'muted-2 nowrap' }, c.description),
                  isImageUnavail
                    ? el('div', { style: { marginTop: '3px' } }, el('span', { className: 'pill' }, '外置检测器未运行'))
                    : null
                ),
                el('td', { className: 'td-right' },
                  createSegmented({
                    ariaLabel: `${displayName} 处置动作`,
                    value: curAction,
                    options: [
                      { value: 'mask', label: '掩码', tone: 'mask' },
                      { value: 'block', label: '阻断', tone: 'block' },
                      { value: 'ignore', label: '忽略' },
                    ],
                    onChange: (val) => updateDraft((d) => {
                      d.categories = { ...(d.categories || {}), [c.key]: val };
                    }),
                  })
                )
              );
            })
          )
        )
      )
    );

    // 3. Lists, Dictionary, Vault & Fail-closed
    const dictIn = el('input', {
      className: 'input mono',
      value: draft.dictionary || '',
      placeholder: '例如 /etc/gatehouse/terms.txt',
      onChange: (e) => updateDraft((d) => { d.dictionary = e.target.value.trim(); }),
    });
    const vaultIn = el('input', {
      className: 'input mono',
      value: draft.vault_ttl || '',
      placeholder: '15m（留空使用默认 15 分钟）',
      dataset: { fieldPath: 'redaction.vault_ttl' },
      onChange: (e) => updateDraft((d) => { d.vault_ttl = e.target.value.trim(); }),
    });
    const failSwitch = createSwitch({
      checked: draft.fail_closed !== false,
      label: '检测器故障时安全阻断 fail_closed',
      onChange: (checked) => updateDraft((d) => { d.fail_closed = checked; }),
    });

    const listsSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '词表、白名单与运行边界'),
          el('p', { className: 'sheet-sub' }, '配置专有词条、免脱敏白名单、内网域名后缀与脱敏保险库 TTL')
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '内联敏感词表 (terms)'),
          createTagInput({
            values: draft.terms || [],
            placeholder: '输入专有项目名、客户代号，回车添加',
            onChange: (vals) => updateDraft((d) => { d.terms = vals; }),
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '免脱敏白名单 (never_mask)'),
          createTagInput({
            values: draft.never_mask || [],
            placeholder: '例如 127.0.0.1 或 example.com，回车添加',
            onChange: (vals) => updateDraft((d) => { d.never_mask = vals; }),
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '内网域名列表 (internal_domains)'),
          createTagInput({
            values: draft.internal_domains || [],
            placeholder: '例如 corp.local 或 internal.net，回车添加',
            onChange: (vals) => updateDraft((d) => { d.internal_domains = vals; }),
          })
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '强制剥离请求字段 (strip_fields)'),
          createTagInput({
            values: draft.strip_fields || [],
            placeholder: '需要从请求中直接抹除的顶层或附加字段',
            onChange: (vals) => updateDraft((d) => { d.strip_fields = vals; }),
          })
        ),
        el('div', { className: 'field-row' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '外部字典文件路径 (dictionary)'),
            dictIn,
            rtInfo?.dictionary?.path
              ? el('p', { className: 'field-hint num' },
                rtInfo.dictionary.error
                  ? `读取失败: ${rtInfo.dictionary.error}`
                  : `已加载 ${fmtNum(rtInfo.dictionary.count || 0)} 条词条`
              )
              : null
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '占位符映射存活时间 (vault_ttl)'),
            vaultIn
          )
        ),
        el('div', { className: 'sublist-item' },
          el('div', { className: 'sublist-head' },
            el('div', null,
              el('strong', { style: { fontSize: '13px' } }, '异常安全阻断 (fail_closed)'),
              el('div', { className: 'muted', style: { fontSize: '12px' } }, '当敏感信息检测器不可用或超时时拒绝放行请求（推荐开启）')
            ),
            failSwitch
          )
        )
      )
    );

    // 4. Live Stats / Provider Effective Mode Summary
    const statsList = rtInfo?.stats_24h || [];
    const provModes = rtInfo?.providers || [];
    const runtimeSheet = (statsList.length || provModes.length)
      ? el('section', { className: 'sheet' },
        el('div', { className: 'sheet-head' },
          el('div', { className: 'sheet-title-group' },
            el('h3', { className: 'sheet-title' }, '近 24 小时脱敏统计与渠道状态')
          )
        ),
        el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
          statsList.length
            ? el('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '8px' } },
              ...statsList.map((s) =>
                el('span', { className: 'pill num' },
                  `${s.mode || 'none'}: 请求 ${fmtNum(s.requests)} / 触发项 ${fmtNum(s.count)}`
                )
              )
            )
            : null,
          provModes.length
            ? el('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '8px' } },
              ...provModes.map((p) => {
                const mi = MODE_LABELS[p.effective_mode] || { text: p.effective_mode, cls: 'pill' };
                return el('span', { className: mi.cls }, `${p.name} (${p.target}) -> ${mi.text}`);
              })
            )
            : null
        )
      )
      : null;

    root.appendChild(
      el('div', { className: 'split-redaction' },
        el('div', { style: { display: 'flex', flexDirection: 'column', gap: '18px' } },
          targetSheet,
          categoriesSheet,
          listsSheet,
          runtimeSheet
        ),
        tryHost
      )
    );
  };

  load();
}
