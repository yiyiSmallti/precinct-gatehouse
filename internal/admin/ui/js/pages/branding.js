import { api } from '/admin/js/api.js';
import { branding } from '/admin/js/branding.js';
import { icon } from '/admin/js/icons.js';
import { store } from '/admin/js/store.js';
import {
  DEFAULT_DIST_TEMPLATE,
  TEMPLATE_VARS,
  renderDistributionTemplate,
  resolveGatewayURLs,
} from '/admin/js/template.js';
import { clear, confirmModal, el, toast } from '/admin/js/ui.js';

const MAX_LOGO_BYTES = 2 * 1024 * 1024;
const ACCEPTED_MIMES = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/svg+xml']);

function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(new Error('读取图片文件失败'));
    reader.readAsDataURL(file);
  });
}

function insertAtCursor(textarea, text) {
  const start = textarea.selectionStart ?? textarea.value.length;
  const end = textarea.selectionEnd ?? textarea.value.length;
  textarea.value = `${textarea.value.slice(0, start)}${text}${textarea.value.slice(end)}`;
  const nextPos = start + text.length;
  textarea.focus();
  textarea.setSelectionRange(nextPos, nextPos);
}

export function renderBranding(container) {
  const root = el('div', { style: { display: 'flex', flexDirection: 'column', gap: '20px' } });
  container.appendChild(root);

  let loaded = false;
  let draftName = branding.state.display_name || '';
  let draftPublicUrl = branding.state.public_url || '';
  let draftTemplate = branding.state.dist_template || DEFAULT_DIST_TEMPLATE;
  let uploadingLogo = false;
  let savingName = false;
  let savingTemplate = false;

  const paint = () => {
    clear(root);

    const rawCfg = store.server?.raw || {};
    const { inferredBaseUrl } = resolveGatewayURLs('', rawCfg.listen);

    const effectiveDisplayName = draftName.trim() || 'Precinct Gatehouse';
    const effectiveSidebarTitle = draftName.trim() || '门禁网关';
    const effectiveSidebarSub = draftName.trim() ? '模型网关控制台' : 'Precinct Gatehouse';
    const smallLogo = branding.getSmallLogoUrl();
    const heroLogo = branding.getHeroLogoUrl();

    const previewHeroBrandText = el('span', null, effectiveDisplayName);
    const previewHeroImgSmall = el('img', { src: smallLogo, alt: '' });
    const previewHeroImgArt = el('img', { className: 'brand-preview-art', src: heroLogo, alt: '' });
    const previewSideTitle = el('span', { className: 'brand-title' }, effectiveSidebarTitle);
    const previewSideSub = el('span', { className: 'brand-sub' }, effectiveSidebarSub);

    const updateBrandPreview = () => {
      const val = draftName.trim();
      previewHeroBrandText.textContent = val || 'Precinct Gatehouse';
      previewSideTitle.textContent = val || '门禁网关';
      previewSideSub.textContent = val ? '模型网关控制台' : 'Precinct Gatehouse';
    };

    const nameInput = el('input', {
      className: 'input',
      type: 'text',
      maxLength: 80,
      value: draftName,
      placeholder: '留空使用默认：Precinct Gatehouse / 门禁网关',
      onInput: (e) => {
        draftName = e.target.value;
        updateBrandPreview();
      },
    });

    const fileInput = el('input', {
      type: 'file',
      accept: 'image/png,image/jpeg,image/webp,image/svg+xml',
      style: { display: 'none' },
    });

    const handleFileUpload = async (file) => {
      if (!file || uploadingLogo) return;
      if (file.type && !ACCEPTED_MIMES.has(file.type.toLowerCase())) {
        toast('仅支持 PNG、JPEG、WebP 或 SVG 格式图片', 'warn');
        return;
      }
      if (file.size > MAX_LOGO_BYTES) {
        toast('图片大小不能超过 2 MB', 'warn');
        return;
      }
      uploadingLogo = true;
      paint();
      try {
        const dataUrl = await readFileAsDataURL(file);
        const res = await api.putBrandingLogo(dataUrl);
        branding.applyUpdate(res);
        toast('首页与控制台商标已更新', 'ok');
      } catch (e) {
        toast(`上传商标失败: ${e.message}`, 'err');
      } finally {
        uploadingLogo = false;
        paint();
      }
    };

    fileInput.addEventListener('change', (e) => {
      const f = e.target.files?.[0];
      if (f) handleFileUpload(f);
      fileInput.value = '';
    });

    const dropzone = el('div', {
      className: 'brand-dropzone',
      tabIndex: 0,
      role: 'button',
      'aria-label': '点击或拖拽上传商标图片',
      onClick: () => fileInput.click(),
      onKeydown: (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          fileInput.click();
        }
      },
      onDragover: (e) => {
        e.preventDefault();
        dropzone.classList.add('is-dragover');
      },
      onDragleave: () => {
        dropzone.classList.remove('is-dragover');
      },
      onDrop: (e) => {
        e.preventDefault();
        dropzone.classList.remove('is-dragover');
        const f = e.dataTransfer?.files?.[0];
        if (f) handleFileUpload(f);
      },
    },
      el('img', { className: 'brand-dropzone-thumb', src: smallLogo, alt: '当前商标预览' }),
      el('div', { style: { minWidth: 0, flex: '1' } },
        el('div', { style: { fontWeight: 550 } }, uploadingLogo ? '正在上传商标...' : '点击选择或拖拽图片到此处上传'),
        el('div', { className: 'muted', style: { fontSize: '12px', marginTop: '2px' } }, '支持 PNG / JPEG / WebP / SVG，建议正方形图标，最大 2 MB')
      ),
      icon('upload', 18)
    );

    const saveNameBtn = el('button', {
      type: 'button',
      className: 'btn btn-primary btn-sm',
      disabled: savingName,
      onClick: async () => {
        savingName = true;
        saveNameBtn.disabled = true;
        try {
          const res = await api.putBrandingSettings({ display_name: draftName.trim() });
          draftName = res.display_name || '';
          branding.applyUpdate(res);
          toast('显示名称已保存，登录页与控制台已同步', 'ok');
        } catch (e) {
          toast(`保存显示名称失败: ${e.message}`, 'err');
        } finally {
          savingName = false;
          saveNameBtn.disabled = false;
        }
      },
    }, savingName ? '保存中...' : '保存显示名称');

    const resetLogoBtn = el('button', {
      type: 'button',
      className: 'btn btn-sm',
      disabled: !branding.state.has_logo || uploadingLogo,
      onClick: async () => {
        const ok = await confirmModal({
          title: '恢复默认商标',
          message: '确定移除自定义商标并恢复 Gatehouse 默认图标吗？',
          confirmText: '恢复默认',
        });
        if (!ok) return;
        try {
          const res = await api.deleteBrandingLogo();
          branding.applyUpdate(res);
          toast('已恢复默认商标', 'ok');
          paint();
        } catch (e) {
          toast(`恢复失败: ${e.message}`, 'err');
        }
      },
    }, '恢复默认商标');

    const brandSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '首页商标与显示名称'),
          el('p', { className: 'sheet-sub' }, '自定义登录首页、侧边栏品牌区与浏览器标签页展示的名称和图标，无需重启即刻生效')
        )
      ),
      el('div', { className: 'sheet-body' },
        el('div', { className: 'brand-editor-grid' },
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
            el('div', { className: 'field' },
              el('label', { className: 'field-label' }, '显示名称'),
              nameInput,
              el('p', { className: 'field-hint' }, '将展示在登录页左上角、控制台侧边栏、浏览器标签页以及默认分发模板标题中；留空恢复默认名称。')
            ),
            el('div', { className: 'field' },
              el('label', { className: 'field-label' }, '首页商标图片'),
              fileInput,
              dropzone,
              el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', marginTop: '4px' } },
                el('button', {
                  type: 'button',
                  className: 'btn btn-sm',
                  disabled: uploadingLogo,
                  onClick: () => fileInput.click(),
                }, icon('upload', 13), '选择并上传商标'),
                resetLogoBtn,
                el('span', { className: 'muted', style: { fontSize: '12px', alignSelf: 'center' } },
                  branding.state.has_logo ? '当前使用自定义商标' : '当前使用默认商标')
              )
            ),
            el('div', { style: { display: 'flex', justifyContent: 'flex-end', gap: '8px', paddingTop: '4px' } },
              draftName.trim()
                ? el('button', {
                  type: 'button',
                  className: 'btn btn-ghost btn-sm',
                  onClick: () => {
                    draftName = '';
                    nameInput.value = '';
                    updateBrandPreview();
                  },
                }, '清空名称')
                : null,
              saveNameBtn
            )
          ),
          el('div', { style: { display: 'flex', flexDirection: 'column', gap: '12px' } },
            el('div', { className: 'field-label' }, '实时效果预览（登录页 & 侧边栏）'),
            el('div', { className: 'brand-preview-hero' },
              el('div', { className: 'brand-preview-top' },
                previewHeroImgSmall,
                previewHeroBrandText
              ),
              previewHeroImgArt,
              el('div', { className: 'brand-preview-copy' },
                el('strong', null, '模型调用的唯一出口'),
                el('span', null, '多协议接入、路由分流、隐私脱敏、按 Token 计量与限额')
              )
            ),
            el('div', { className: 'brand-preview-sidebar' },
              el('img', { className: 'brand-mark', src: smallLogo, alt: '' }),
              el('div', { className: 'brand-text' }, previewSideTitle, previewSideSub)
            )
          )
        )
      )
    );

    // Section 2: Key Distribution Template
    const previewPre = el('pre', { className: 'try-pre', style: { maxHeight: '340px' } });

    const sampleProviders = (rawCfg.providers || []).slice(0, 1).flatMap((p) => (p.models || []).slice(0, 2).map((m) => `${p.name}/${m}`));
    const sampleGroups = Object.keys(rawCfg.routing_groups || {}).slice(0, 1);
    const sampleKey = {
      name: 'team-member',
      key_prefix: 'sk-DemoKey',
      models: sampleProviders,
      groups: sampleGroups,
      rpm: 60,
      rpd: 1000,
      concurrency: 4,
      tokens_daily: 1_000_000,
      expires_at: 0,
      note: '研发测试密钥',
    };

    const refreshTemplatePreview = () => {
      previewPre.textContent = renderDistributionTemplate({
        key: sampleKey,
        plainKey: 'sk-DemoKeyExample1234567890abcdef',
        brandingState: {
          display_name: draftName.trim(),
          public_url: draftPublicUrl.trim(),
          dist_template: draftTemplate,
        },
        rawConfig: rawCfg,
        templateOverride: draftTemplate,
      });
    };

    const publicUrlIn = el('input', {
      className: 'input mono',
      type: 'url',
      value: draftPublicUrl,
      placeholder: `留空自动推断（当前：${inferredBaseUrl}）`,
      onInput: (e) => {
        draftPublicUrl = e.target.value;
        refreshTemplatePreview();
      },
    });

    const templateTA = el('textarea', {
      className: 'textarea mono',
      value: draftTemplate,
      spellcheck: false,
      style: { minHeight: '280px', fontSize: '12px', lineHeight: '1.6' },
      onInput: (e) => {
        draftTemplate = e.target.value;
        refreshTemplatePreview();
      },
    });

    const varBar = el('div', { className: 'tpl-var-bar' },
      ...TEMPLATE_VARS.map((v) =>
        el('button', {
          type: 'button',
          className: 'tpl-var-chip',
          title: `${v.desc}（点击插入到模板光标处）`,
          onClick: () => {
            insertAtCursor(templateTA, v.token);
            draftTemplate = templateTA.value;
            refreshTemplatePreview();
          },
        }, v.label, el('code', { className: 'mono' }, v.token))
      )
    );

    const saveTplBtn = el('button', {
      type: 'button',
      className: 'btn btn-primary btn-sm',
      disabled: savingTemplate,
      onClick: async () => {
        savingTemplate = true;
        saveTplBtn.disabled = true;
        try {
          const tplToSave = draftTemplate.trim() === DEFAULT_DIST_TEMPLATE.trim() ? '' : draftTemplate;
          const res = await api.putBrandingSettings({
            public_url: draftPublicUrl.trim(),
            dist_template: tplToSave,
          });
          draftPublicUrl = res.public_url || '';
          draftTemplate = res.dist_template || DEFAULT_DIST_TEMPLATE;
          branding.applyUpdate(res);
          toast('分发配置模板已保存', 'ok');
          paint();
        } catch (e) {
          toast(`保存模板失败: ${e.message}`, 'err');
        } finally {
          savingTemplate = false;
          saveTplBtn.disabled = false;
        }
      },
    }, savingTemplate ? '保存中...' : '保存模板配置');

    refreshTemplatePreview();

    const templateSheet = el('section', { className: 'sheet' },
      el('div', { className: 'sheet-head' },
        el('div', { className: 'sheet-title-group' },
          el('h3', { className: 'sheet-title' }, '密钥分发配置模板'),
          el('p', { className: 'sheet-sub' }, '在「访问密钥」页面点击“复制配置”时，将按此模板自动填充地址、密钥与模型信息并复制')
        ),
        el('div', { className: 'sheet-actions' },
          el('button', {
            type: 'button',
            className: 'btn btn-sm',
            onClick: () => {
              draftTemplate = DEFAULT_DIST_TEMPLATE;
              templateTA.value = DEFAULT_DIST_TEMPLATE;
              refreshTemplatePreview();
              toast('已还原为默认模板（点击“保存模板配置”后生效）', 'ok');
            },
          }, icon('refresh', 13), '恢复默认模板'),
          saveTplBtn
        )
      ),
      el('div', { className: 'sheet-body', style: { display: 'flex', flexDirection: 'column', gap: '16px' } },
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '对外网关地址（Base URL）'),
          publicUrlIn,
          el('p', { className: 'field-hint' }, `填团队成员实际访问的网关根地址（如 https://llm.example.com 或 http://10.0.0.10:7788，无需加 /v1）。留空时自动推断为 ${inferredBaseUrl}。`)
        ),
        el('div', { className: 'field' },
          el('label', { className: 'field-label' }, '模板可用变量（点击插入）'),
          varBar
        ),
        el('div', { className: 'brand-editor-grid' },
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '配置模板正文'),
            templateTA
          ),
          el('div', { className: 'field' },
            el('label', { className: 'field-label' }, '输出效果预览（示例密钥）'),
            previewPre
          )
        )
      )
    );

    if (!loaded) {
      root.append(el('div', { className: 'sheet' }, el('div', { className: 'sheet-body muted' }, '正在加载外观与模板配置...')));
      return;
    }

    root.append(brandSheet, templateSheet);
  };

  paint();
  branding.loadSettings()
    .then((st) => {
      loaded = true;
      draftName = st.display_name || '';
      draftPublicUrl = st.public_url || '';
      draftTemplate = st.dist_template || DEFAULT_DIST_TEMPLATE;
      paint();
    })
    .catch((e) => {
      loaded = true;
      if (e.status !== 401) toast(`读取外观与模板配置失败: ${e.message}`, 'err');
      paint();
    });
}
