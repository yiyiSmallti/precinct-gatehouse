import { icon } from '/admin/js/icons.js';

export function el(tag, props = null, ...children) {
  const node = document.createElement(tag);
  if (props) {
    Object.entries(props).forEach(([k, v]) => {
      if (v === undefined || v === null || v === false) return;
      if (k === 'className') {
        node.className = v;
      } else if (k === 'dataset') {
        Object.entries(v).forEach(([dk, dv]) => {
          if (dv !== undefined && dv !== null) node.dataset[dk] = String(dv);
        });
      } else if (k === 'style' && typeof v === 'object') {
        Object.assign(node.style, v);
      } else if (k.startsWith('on') && typeof v === 'function') {
        node.addEventListener(k.slice(2).toLowerCase(), v);
      } else if (k === 'checked' || k === 'disabled' || k === 'value' || k === 'selected' || k === 'readOnly') {
        node[k] = v;
      } else {
        node.setAttribute(k, v === true ? '' : String(v));
      }
    });
  }
  const append = (item) => {
    if (item === undefined || item === null || item === false) return;
    if (Array.isArray(item)) {
      item.forEach(append);
    } else if (item instanceof Node) {
      node.appendChild(item);
    } else {
      node.appendChild(document.createTextNode(String(item)));
    }
  };
  children.forEach(append);
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export function clone(val) {
  if (val === undefined) return undefined;
  return JSON.parse(JSON.stringify(val));
}

export function fmtNum(n) {
  if (n === undefined || n === null || Number.isNaN(Number(n))) return '0';
  return Number(n).toLocaleString('en-US');
}

export function fmtCompact(n) {
  const v = Number(n || 0);
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(2).replace(/\.00$/, '')}M`;
  if (v >= 10_000) return `${(v / 1_000).toFixed(1).replace(/\.0$/, '')}k`;
  return fmtNum(v);
}

export function fmtTokens(n) {
  if (n === undefined || n === null || Number.isNaN(Number(n))) return '0';
  const v = Math.round(Number(n));
  if (!Number.isFinite(v)) return '0';
  const abs = Math.abs(v);
  if (abs < 1000) return String(v);
  if (abs < 999_950) return `${(v / 1e3).toFixed(1).replace(/\.0$/, '')}k`;
  return `${(v / 1e6).toFixed(1).replace(/\.0$/, '')}M`;
}

export function fmtDuration(ms) {
  if (ms === undefined || ms === null || Number.isNaN(Number(ms))) return '0ms';
  const n = Math.round(Number(ms));
  if (!Number.isFinite(n)) return '0ms';
  if (n < 1000) return `${n}ms`;
  if (n < 59_950) return `${(n / 1e3).toFixed(1).replace(/\.0$/, '')}s`;
  const sec = Math.floor((n + 500) / 1000);
  if (sec < 3600) return `${Math.floor(sec / 60)}m${String(sec % 60).padStart(2, '0')}s`;
  return `${Math.floor(sec / 3600)}h${String(Math.floor((sec % 3600) / 60)).padStart(2, '0')}m`;
}

export function fmtTime(secOrMs) {
  if (!secOrMs) return '-';
  const ms = secOrMs > 1e12 ? secOrMs : secOrMs * 1000;
  const d = new Date(ms);
  return d.toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function fmtDateTime(secOrIso) {
  if (!secOrIso) return '-';
  let d;
  if (typeof secOrIso === 'string') {
    d = new Date(secOrIso);
  } else {
    d = new Date(secOrIso > 1e12 ? secOrIso : secOrIso * 1000);
  }
  if (Number.isNaN(d.getTime())) return String(secOrIso);
  return d.toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
}

export function sourceLabel(src) {
  const map = {
    startup: '启动加载',
    web: '控制台应用',
    file: '文件监听',
    sighup: 'SIGHUP',
    rollback: '版本回滚',
    disk: '磁盘重载',
  };
  return map[src] || src || '初始';
}

export function toast(msg, kind = 'ok', duration = 3600) {
  const root = document.getElementById('toast-root');
  if (!root) return;
  const t = el('div', { className: `toast ${kind}` },
    icon(kind === 'err' ? 'alert' : kind === 'warn' ? 'alert' : 'check', 16),
    el('span', null, msg)
  );
  root.appendChild(t);
  setTimeout(() => {
    t.remove();
  }, duration);
}

function trapFocus(container, onEsc) {
  const prev = document.activeElement;
  const getFocusable = () =>
    Array.from(
      container.querySelectorAll(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
      )
    );
  requestAnimationFrame(() => {
    const items = getFocusable();
    if (items.length) items[0].focus();
  });
  const onKeyDown = (e) => {
    if (e.key === 'Escape') {
      const modalRoot = document.getElementById('modal-root');
      if (modalRoot && modalRoot.childElementCount > 0 && !container.closest('#modal-root')) {
        return;
      }
      e.preventDefault();
      onEsc();
      return;
    }
    if (e.key === 'Tab') {
      const items = getFocusable();
      if (!items.length) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  };
  document.addEventListener('keydown', onKeyDown);
  return () => {
    document.removeEventListener('keydown', onKeyDown);
    if (prev && typeof prev.focus === 'function') prev.focus();
  };
}

let releaseDrawer = null;
export function closeDrawer() {
  const root = document.getElementById('drawer-root');
  if (releaseDrawer) {
    releaseDrawer();
    releaseDrawer = null;
  }
  if (root) clear(root);
}

export function openDrawer({ title, body, footer = null, onClose = null }) {
  closeDrawer();
  const root = document.getElementById('drawer-root');
  const finish = () => {
    closeDrawer();
    if (onClose) onClose();
  };
  const closeBtn = el('button', {
    className: 'icon-btn',
    type: 'button',
    'aria-label': '关闭抽屉',
    onClick: finish,
  }, icon('close', 16));

  const panel = el('div', {
    className: 'drawer',
    role: 'dialog',
    'aria-modal': 'true',
    'aria-label': title,
    onClick: (e) => e.stopPropagation(),
  },
    el('div', { className: 'drawer-head' },
      el('h3', { className: 'drawer-title' }, title),
      closeBtn
    ),
    el('div', { className: 'drawer-body' }, body),
    footer ? el('div', { className: 'drawer-footer' }, footer) : null
  );

  const backdrop = el('div', {
    className: 'drawer-backdrop',
    onClick: finish,
  }, panel);

  root.appendChild(backdrop);
  releaseDrawer = trapFocus(panel, finish);
  return { panel, close: finish };
}

let releaseModal = null;
export function closeModal() {
  const root = document.getElementById('modal-root');
  if (releaseModal) {
    releaseModal();
    releaseModal = null;
  }
  if (root) clear(root);
}

export function openModal({ title, wide = false, body, footer = null, onClose = null }) {
  closeModal();
  const root = document.getElementById('modal-root');
  const finish = () => {
    closeModal();
    if (onClose) onClose();
  };
  const panel = el('div', {
    className: wide ? 'modal modal-wide' : 'modal',
    role: 'dialog',
    'aria-modal': 'true',
    'aria-label': title,
    onClick: (e) => e.stopPropagation(),
  },
    el('div', { className: 'modal-head' },
      el('h3', { className: 'modal-title' }, title),
      el('button', {
        className: 'icon-btn',
        type: 'button',
        'aria-label': '关闭弹窗',
        onClick: finish,
      }, icon('close', 16))
    ),
    el('div', { className: 'modal-body' }, body),
    footer ? el('div', { className: 'modal-footer' }, footer) : null
  );
  const backdrop = el('div', {
    className: 'modal-backdrop',
    onClick: finish,
  }, panel);
  root.appendChild(backdrop);
  releaseModal = trapFocus(panel, finish);
  return { panel, close: finish };
}

export function confirmModal({ title, message, confirmText = '确认', danger = false }) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (val) => {
      if (settled) return;
      settled = true;
      closeModal();
      resolve(val);
    };
    openModal({
      title,
      body: el('p', { style: { margin: 0 } }, message),
      onClose: () => {
        if (!settled) {
          settled = true;
          resolve(false);
        }
      },
      footer: [
        el('button', { type: 'button', className: 'btn', onClick: () => done(false) }, '取消'),
        el('button', {
          type: 'button',
          className: danger ? 'btn btn-danger' : 'btn btn-primary',
          onClick: () => done(true),
        }, confirmText),
      ],
    });
  });
}

export function createSegmented({ options, value, onChange, ariaLabel = '' }) {
  const wrap = el('div', { className: 'seg', role: 'group', 'aria-label': ariaLabel || undefined });
  const render = (cur) => {
    clear(wrap);
    options.forEach((opt) => {
      const toneClass = opt.tone ? ` seg-${opt.tone}` : '';
      wrap.appendChild(
        el('button', {
          type: 'button',
          className: `seg-btn${toneClass}`,
          'aria-pressed': cur === opt.value ? 'true' : 'false',
          onClick: () => {
            if (cur === opt.value) return;
            render(opt.value);
            onChange(opt.value);
          },
        }, opt.label)
      );
    });
  };
  render(value);
  return wrap;
}

export function createSwitch({ checked, onChange, label = '' }) {
  const btn = el('button', {
    type: 'button',
    role: 'switch',
    className: 'switch',
    'aria-checked': checked ? 'true' : 'false',
    'aria-label': label || undefined,
    onClick: () => {
      const next = btn.getAttribute('aria-checked') !== 'true';
      btn.setAttribute('aria-checked', next ? 'true' : 'false');
      onChange(next);
    },
  }, el('span', { className: 'switch-knob' }));
  return btn;
}

export function createTagInput({ values = [], placeholder = '输入后按回车添加', suggestions = [], onChange, fieldPath = '' }) {
  let list = [...values];
  const outer = el('div');
  const box = el('div', {
    className: 'tag-box',
    dataset: fieldPath ? { fieldPath } : undefined,
  });
  const input = el('input', {
    type: 'text',
    className: 'tag-input',
    placeholder,
  });

  const commit = (raw) => {
    const tokens = raw.split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
    if (!tokens.length) return;
    let changed = false;
    tokens.forEach((t) => {
      if (!list.includes(t)) {
        list.push(t);
        changed = true;
      }
    });
    input.value = '';
    if (changed) {
      renderChips();
      onChange([...list]);
    }
  };

  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commit(input.value);
    } else if (e.key === 'Backspace' && !input.value && list.length) {
      list.pop();
      renderChips();
      onChange([...list]);
    }
  });
  input.addEventListener('blur', () => {
    if (input.value.trim()) commit(input.value);
  });

  const sugBox = el('div', { className: 'tag-suggestions' });

  const renderChips = () => {
    clear(box);
    list.forEach((val, idx) => {
      box.appendChild(
        el('span', { className: 'tag-chip' },
          val,
          el('button', {
            type: 'button',
            className: 'tag-chip-rm',
            'aria-label': `移除 ${val}`,
            onClick: (e) => {
              e.stopPropagation();
              list.splice(idx, 1);
              renderChips();
              onChange([...list]);
            },
          }, icon('close', 11))
        )
      );
    });
    box.appendChild(input);

    clear(sugBox);
    const avail = suggestions.filter((s) => !list.includes(s)).slice(0, 12);
    if (avail.length) {
      avail.forEach((s) => {
        sugBox.appendChild(
          el('button', {
            type: 'button',
            className: 'tag-sug-btn',
            onClick: () => commit(s),
          }, `+ ${s}`)
        );
      });
    }
  };

  box.addEventListener('click', () => input.focus());
  renderChips();
  outer.appendChild(box);
  if (suggestions.length) outer.appendChild(sugBox);
  return outer;
}

export function createJsonTextarea({ value = null, placeholder = '{"temperature": 0.6}', onChange }) {
  const initText = value && Object.keys(value).length ? JSON.stringify(value, null, 2) : '';
  const ta = el('textarea', {
    className: 'textarea mono',
    placeholder,
    value: initText,
    style: { minHeight: '96px', fontSize: '12px' },
  });
  const errLine = el('p', { className: 'field-error', style: { display: 'none' } });

  const parse = () => {
    const txt = ta.value.trim();
    if (!txt) {
      ta.classList.remove('input-invalid');
      errLine.style.display = 'none';
      onChange({ valid: true, value: undefined });
      return;
    }
    try {
      const obj = JSON.parse(txt);
      if (!obj || typeof obj !== 'object' || Array.isArray(obj)) {
        throw new Error('必须为 JSON 对象（例如 {"key": "value"}）');
      }
      ta.classList.remove('input-invalid');
      errLine.style.display = 'none';
      onChange({ valid: true, value: obj });
    } catch (e) {
      ta.classList.add('input-invalid');
      errLine.textContent = `JSON 格式错误：${e.message}`;
      errLine.style.display = 'block';
      onChange({ valid: false, value: undefined, error: e.message });
    }
  };

  ta.addEventListener('input', parse);
  return el('div', { className: 'field' }, ta, errLine);
}

export function clearFieldErrors(root = document) {
  root.querySelectorAll('.input-invalid').forEach((n) => n.classList.remove('input-invalid'));
  root.querySelectorAll('[data-mapped-err="1"]').forEach((n) => n.remove());
}

export function applyFieldProblems(problems = [], root = document) {
  clearFieldErrors(root);
  const unmapped = [];
  problems.forEach((p) => {
    const str = String(p);
    const m = str.match(/^([a-zA-Z0-9_.\[\]-]+)\s*[:：]\s*(.+)$/);
    let matched = false;
    if (m) {
      const path = m[1];
      const msg = m[2];
      const candidates = root.querySelectorAll('[data-field-path]');
      for (const target of candidates) {
        const fp = target.dataset.fieldPath;
        if (fp === path || path.startsWith(`${fp}.`) || path.startsWith(`${fp}[`)) {
          target.classList.add('input-invalid');
          const msgEl = el('p', { className: 'field-error', dataset: { mappedErr: '1' } }, msg);
          target.parentElement?.appendChild(msgEl);
          matched = true;
          break;
        }
      }
    }
    if (!matched) unmapped.push(str);
  });
  return unmapped;
}

export function sectionLabel(sec) {
  const labels = {
    providers: '模型渠道',
    routing: '路由策略',
    aliases: '模型别名',
    redaction: '隐私脱敏',
    general: '常规参数',
    updater: '系统更新',
  };
  return labels[sec] || sec;
}
