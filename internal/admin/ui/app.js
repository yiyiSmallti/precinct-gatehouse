(function () {
  const app = document.getElementById('app');
  const modalRoot = document.getElementById('modal-root');

  const state = {
    user: null,
    route: 'overview',
    logsFilter: { key: '', model: '', status: '', protocol: '' },
    logsRows: [],
    logsNext: 0,
    logsHasMore: false,
    usageBy: 'day',
    usageSince: '',
  };

  function esc(v) {
    return String(v == null ? '' : v)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function fmtNum(n) {
    return Number(n || 0).toLocaleString('zh-CN');
  }

  function fmtTime(ts) {
    if (!ts) return '-';
    const d = new Date(ts * 1000);
    const pad = (x) => String(x).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  }

  function fmtLimit(used, lim) {
    return `${fmtNum(used)} / ${lim > 0 ? fmtNum(lim) : '不限'}`;
  }

  async function apiFetch(path, opts = {}) {
    const method = (opts.method || 'GET').toUpperCase();
    const headers = Object.assign({}, opts.headers || {});
    if (method !== 'GET' && method !== 'HEAD') {
      headers['X-Gatehouse-Admin'] = '1';
      if (opts.body && !headers['Content-Type']) {
        headers['Content-Type'] = 'application/json';
      }
    }
    const res = await fetch(path, {
      method,
      headers,
      credentials: 'same-origin',
      body: opts.body,
    });
    const data = await res.json().catch(() => ({}));
    if (res.status === 401 && path !== '/admin/api/login') {
      state.user = null;
      navigate('login');
      throw new Error(data.error || '未登录或会话已过期');
    }
    if (!res.ok) {
      throw new Error(data.error || `请求失败 (${res.status})`);
    }
    return data;
  }

  function closeModal() {
    modalRoot.innerHTML = '';
  }

  function openModal(html) {
    modalRoot.innerHTML = `<div class="modal-backdrop" id="modal-bd"><div class="modal">${html}</div></div>`;
    document.getElementById('modal-bd').addEventListener('click', (e) => {
      if (e.target.id === 'modal-bd') closeModal();
    });
  }

  function showConfirm(title, message, onConfirm) {
    openModal(`
      <h3 class="modal-title">${esc(title)}</h3>
      <p>${esc(message)}</p>
      <div class="modal-foot">
        <button class="btn" id="m-cancel">取消</button>
        <button class="btn btn-primary" id="m-ok">确认</button>
      </div>
    `);
    document.getElementById('m-cancel').onclick = closeModal;
    document.getElementById('m-ok').onclick = async () => {
      closeModal();
      await onConfirm();
    };
  }

  function showSecretModal(title, plain) {
    openModal(`
      <h3 class="modal-title">${esc(title)}</h3>
      <div class="alert alert-warn">此密钥明文仅显示这一次，关闭后无法再次查看，请立即复制保存。</div>
      <div class="secret-box" id="secret-text">${esc(plain)}</div>
      <div class="modal-foot">
        <button class="btn" id="m-copy">复制密钥</button>
        <button class="btn btn-primary" id="m-done">我已保存</button>
      </div>
    `);
    document.getElementById('m-copy').onclick = async () => {
      try {
        await navigator.clipboard.writeText(plain);
        document.getElementById('m-copy').textContent = '已复制';
      } catch (_) {
        document.getElementById('m-copy').textContent = '请手动复制';
      }
    };
    document.getElementById('m-done').onclick = closeModal;
  }

  function parseRoute() {
    const h = (location.hash || '').replace(/^#\/?/, '');
    if (['overview', 'keys', 'logs', 'usage', 'channels', 'login'].includes(h)) {
      return h;
    }
    return 'overview';
  }

  function navigate(route) {
    state.route = route;
    if (location.hash !== '#/' + route) {
      location.hash = '#/' + route;
    } else {
      render();
    }
  }

  function renderLogin(errMsg) {
    app.innerHTML = `
      <div class="login-wrap">
        <form class="login-card" id="login-form">
          <h1 class="login-title">Gatehouse 控制台</h1>
          <p class="login-sub">请使用管理员账号登录</p>
          ${errMsg ? `<div class="alert alert-danger">${esc(errMsg)}</div>` : ''}
          <label class="field">用户名
            <input id="l-user" type="text" value="admin" autocomplete="username" required>
          </label>
          <label class="field">密码
            <input id="l-pass" type="password" autocomplete="current-password" required autofocus>
          </label>
          <button class="btn btn-primary" style="width:100%;margin-top:6px" type="submit">登录</button>
        </form>
      </div>
    `;
    document.getElementById('login-form').onsubmit = async (e) => {
      e.preventDefault();
      const username = document.getElementById('l-user').value.trim();
      const password = document.getElementById('l-pass').value;
      try {
        const res = await apiFetch('/admin/api/login', {
          method: 'POST',
          body: JSON.stringify({ username, password }),
        });
        state.user = res.username;
        navigate('overview');
      } catch (err) {
        renderLogin(err.message);
      }
    };
  }

  function renderShell(title, actionsHTML, bodyHTML) {
    const navItems = [
      ['overview', '概览'],
      ['keys', '密钥管理'],
      ['logs', '请求日志'],
      ['usage', '用量统计'],
      ['channels', '渠道与路由'],
    ];
    app.innerHTML = `
      <div class="shell">
        <aside class="sidebar">
          <div class="brand">
            <div class="brand-name">Gatehouse</div>
            <div class="brand-sub">模型网关管理台</div>
          </div>
          <nav class="nav">
            ${navItems.map(([k, label]) => `<a href="#/${k}" class="${state.route === k ? 'active' : ''}">${label}</a>`).join('')}
          </nav>
          <div class="sidebar-foot">
            <span class="muted">${esc(state.user || '')}</span>
            <button class="btn btn-sm" id="btn-logout">退出</button>
          </div>
        </aside>
        <main class="main">
          <div class="page-head">
            <h1 class="page-title">${esc(title)}</h1>
            <div class="actions">${actionsHTML || ''}</div>
          </div>
          <div id="page-body">${bodyHTML}</div>
        </main>
      </div>
    `;
    document.getElementById('btn-logout').onclick = async () => {
      try {
        await apiFetch('/admin/api/logout', { method: 'POST' });
      } catch (_) {}
      state.user = null;
      navigate('login');
    };
  }

  function renderHourlySVG(hourly) {
    const W = 720;
    const H = 170;
    const padL = 34;
    const padR = 10;
    const padT = 12;
    const padB = 24;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;
    const maxVal = Math.max(1, ...hourly.map((b) => b.requests));
    const slotW = plotW / hourly.length;
    const barW = Math.max(8, slotW - 6);

    const bars = hourly.map((b, i) => {
      const x = padL + i * slotW + (slotW - barW) / 2;
      const totH = (b.requests / maxVal) * plotH;
      const errH = (b.errors / maxVal) * plotH;
      const okH = Math.max(0, totH - errH);
      const yOk = padT + plotH - totH;
      const yErr = padT + plotH - errH;
      const showLabel = i === 0 || i === 6 || i === 12 || i === 18 || i === 23;
      return `
        <g>
          <title>${esc(b.hour)}：请求 ${b.requests}，错误 ${b.errors}</title>
          ${okH > 0 ? `<rect x="${x.toFixed(1)}" y="${yOk.toFixed(1)}" width="${barW.toFixed(1)}" height="${okH.toFixed(1)}" rx="2" fill="var(--accent)"/>` : ''}
          ${errH > 0 ? `<rect x="${x.toFixed(1)}" y="${yErr.toFixed(1)}" width="${barW.toFixed(1)}" height="${errH.toFixed(1)}" rx="2" fill="var(--danger)"/>` : ''}
          ${b.requests === 0 ? `<rect x="${x.toFixed(1)}" y="${(padT + plotH - 2).toFixed(1)}" width="${barW.toFixed(1)}" height="2" fill="var(--border)"/>` : ''}
          ${showLabel ? `<text x="${(x + barW / 2).toFixed(1)}" y="${H - 6}" text-anchor="middle" font-size="11" fill="var(--text-muted)">${esc(b.hour)}</text>` : ''}
        </g>
      `;
    }).join('');

    return `
      <svg viewBox="0 0 ${W} ${H}" style="width:100%;height:180px;display:block">
        <line x1="${padL}" y1="${padT + plotH}" x2="${W - padR}" y2="${padT + plotH}" stroke="var(--border)"/>
        <text x="${padL - 6}" y="${padT + 8}" text-anchor="end" font-size="11" fill="var(--text-muted)">${maxVal}</text>
        <text x="${padL - 6}" y="${padT + plotH}" text-anchor="end" font-size="11" fill="var(--text-muted)">0</text>
        ${bars}
      </svg>
    `;
  }

  async function loadOverview() {
    renderShell('概览', `<button class="btn" id="ov-refresh">刷新</button>`, `<div class="card">加载中...</div>`);
    document.getElementById('ov-refresh').onclick = loadOverview;
    try {
      const d = await apiFetch('/admin/api/overview');
      const t = d.today || {};
      const totalTok = (t.prompt_tokens || 0) + (t.completion_tokens || 0);
      const cacheRate = t.prompt_tokens > 0 ? ((t.cached_tokens / t.prompt_tokens) * 100).toFixed(1) + '%' : '0%';
      const html = `
        <div class="kpi-grid">
          <div class="card">
            <div class="kpi-label">今日请求</div>
            <div class="kpi-val">${fmtNum(t.requests)}</div>
            <div class="kpi-sub">错误 ${fmtNum(t.errors)}</div>
          </div>
          <div class="card">
            <div class="kpi-label">今日 Token</div>
            <div class="kpi-val">${fmtNum(totalTok)}</div>
            <div class="kpi-sub">输入 ${fmtNum(t.prompt_tokens)} · 输出 ${fmtNum(t.completion_tokens)}</div>
          </div>
          <div class="card">
            <div class="kpi-label">缓存命中</div>
            <div class="kpi-val">${fmtNum(t.cached_tokens)}</div>
            <div class="kpi-sub">输入命中率 ${cacheRate}</div>
          </div>
          <div class="card">
            <div class="kpi-label">平均首字 / 当前并发</div>
            <div class="kpi-val">${Math.round(t.avg_ttft_ms || 0)} ms</div>
            <div class="kpi-sub">当前活跃并发 ${fmtNum(d.inflight_total)}</div>
          </div>
        </div>

        <div class="card">
          <div class="card-title">
            <span>过去 24 小时请求趋势</span>
            <span class="chart-legend">
              <span><span class="dot" style="background:var(--accent)"></span>成功</span>
              <span><span class="dot" style="background:var(--danger)"></span>错误</span>
            </span>
          </div>
          <div class="chart-box">${renderHourlySVG(d.hourly || [])}</div>
        </div>

        <div class="two-col">
          <div class="card">
            <div class="card-title"><span>今日模型分布</span></div>
            <div class="table-wrap">
              <table>
                <thead><tr><th>模型</th><th>请求</th><th>错误</th><th>输入/输出</th><th>缓存</th></tr></thead>
                <tbody>
                  ${(d.by_model || []).length ? d.by_model.map((m) => `
                    <tr>
                      <td class="mono">${esc(m.model)}</td>
                      <td class="num">${fmtNum(m.requests)}</td>
                      <td class="num">${fmtNum(m.errors)}</td>
                      <td class="num">${fmtNum(m.prompt_tokens)} / ${fmtNum(m.completion_tokens)}</td>
                      <td class="num">${fmtNum(m.cached_tokens)}</td>
                    </tr>
                  `).join('') : `<tr><td colspan="5" class="muted">今日暂无请求</td></tr>`}
                </tbody>
              </table>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><span>密钥实时状态</span></div>
            <div class="table-wrap">
              <table>
                <thead><tr><th>密钥</th><th>并发</th><th>1分钟</th><th>今日</th></tr></thead>
                <tbody>
                  ${(d.keys || []).length ? d.keys.map((k) => `
                    <tr>
                      <td>${esc(k.name)}</td>
                      <td class="num">${k.inflight}</td>
                      <td class="num">${k.rpm_used}</td>
                      <td class="num">${k.today_used}</td>
                    </tr>
                  `).join('') : `<tr><td colspan="4" class="muted">暂无 API Key</td></tr>`}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      `;
      document.getElementById('page-body').innerHTML = html;
    } catch (err) {
      document.getElementById('page-body').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
    }
  }

  function openKeyEditor(existing) {
    const isEdit = Boolean(existing);
    const k = existing || { name: '', models: [], groups: [], rpm: 0, rpd: 0, concurrency: 0, expires_at: 0, note: '' };
    const expStr = k.expires_at ? new Date(k.expires_at * 1000).toISOString().slice(0, 10) : '';
    openModal(`
      <h3 class="modal-title">${isEdit ? '编辑密钥' : '新建密钥'}</h3>
      <form id="key-form">
        <div id="key-form-err"></div>
        <label class="field">名称 *
          <input id="kf-name" value="${esc(k.name)}" required>
        </label>
        <label class="field">允许模型（逗号分隔，留空不限）
          <input id="kf-models" value="${esc((k.models || []).join(', '))}" placeholder="例如：ds/deepseek-chat">
        </label>
        <label class="field">允许路由组（逗号分隔，留空不限）
          <input id="kf-groups" value="${esc((k.groups || []).join(', '))}" placeholder="例如：default">
        </label>
        <div class="form-grid">
          <label class="field">RPM（0 不限）
            <input id="kf-rpm" type="number" min="0" value="${Number(k.rpm || 0)}">
          </label>
          <label class="field">每日次数 RPD（0 不限）
            <input id="kf-rpd" type="number" min="0" value="${Number(k.rpd || 0)}">
          </label>
          <label class="field">并发数（0 不限）
            <input id="kf-conc" type="number" min="0" value="${Number(k.concurrency || 0)}">
          </label>
        </div>
        <label class="field">到期日期（留空永不过期）
          <input id="kf-exp" type="date" value="${esc(expStr)}">
        </label>
        <label class="field">备注
          <input id="kf-note" value="${esc(k.note || '')}">
        </label>
        <div class="modal-foot">
          <button type="button" class="btn" id="kf-cancel">取消</button>
          <button type="submit" class="btn btn-primary">${isEdit ? '保存' : '创建'}</button>
        </div>
      </form>
    `);
    document.getElementById('kf-cancel').onclick = closeModal;
    document.getElementById('key-form').onsubmit = async (e) => {
      e.preventDefault();
      const splitCSV = (v) => v.split(',').map((s) => s.trim()).filter(Boolean);
      const expVal = document.getElementById('kf-exp').value;
      const expires_at = expVal ? Math.floor(new Date(expVal + 'T23:59:59').getTime() / 1000) : 0;
      const payload = {
        name: document.getElementById('kf-name').value.trim(),
        models: splitCSV(document.getElementById('kf-models').value),
        groups: splitCSV(document.getElementById('kf-groups').value),
        rpm: Number(document.getElementById('kf-rpm').value || 0),
        rpd: Number(document.getElementById('kf-rpd').value || 0),
        concurrency: Number(document.getElementById('kf-conc').value || 0),
        expires_at,
        note: document.getElementById('kf-note').value,
      };
      try {
        if (isEdit) {
          await apiFetch(`/admin/api/keys/${encodeURIComponent(k.id)}`, {
            method: 'PATCH',
            body: JSON.stringify(payload),
          });
          closeModal();
          await loadKeys();
        } else {
          const res = await apiFetch('/admin/api/keys', {
            method: 'POST',
            body: JSON.stringify(payload),
          });
          closeModal();
          await loadKeys();
          showSecretModal('密钥创建成功', res.key);
        }
      } catch (err) {
        document.getElementById('key-form-err').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
      }
    };
  }

  async function loadKeys() {
    renderShell('密钥管理', `
      <button class="btn" id="k-refresh">刷新</button>
      <button class="btn btn-primary" id="k-new">新建密钥</button>
    `, `<div class="card">加载中...</div>`);
    document.getElementById('k-refresh').onclick = loadKeys;
    document.getElementById('k-new').onclick = () => openKeyEditor(null);

    try {
      const data = await apiFetch('/admin/api/keys');
      const keys = data.keys || [];
      const badge = (s) => {
        if (s === 'enabled') return `<span class="badge badge-ok">启用</span>`;
        if (s === 'disabled') return `<span class="badge badge-danger">已禁用</span>`;
        return `<span class="badge badge-warn">已过期</span>`;
      };
      const scopeText = (k) => {
        const m = (k.models || []).length ? `模型: ${k.models.join(', ')}` : '';
        const g = (k.groups || []).length ? `组: ${k.groups.join(', ')}` : '';
        return [m, g].filter(Boolean).join('；') || '不限';
      };

      document.getElementById('page-body').innerHTML = `
        <div class="card">
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>名称</th>
                  <th>前缀</th>
                  <th>状态</th>
                  <th>权限范围</th>
                  <th>RPM (用/限)</th>
                  <th>每日 (用/限)</th>
                  <th>并发 (用/限)</th>
                  <th>最后使用</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                ${keys.length ? keys.map((k) => `
                  <tr>
                    <td>
                      <div><strong>${esc(k.name)}</strong></div>
                      ${k.note ? `<div class="muted" style="font-size:12px">${esc(k.note)}</div>` : ''}
                    </td>
                    <td class="mono">${esc(k.key_prefix)}...</td>
                    <td>${badge(k.state)}</td>
                    <td>${esc(scopeText(k))}</td>
                    <td class="num">${fmtLimit(k.rpm_used, k.rpm)}</td>
                    <td class="num">${fmtLimit(k.today_used, k.rpd)}</td>
                    <td class="num">${fmtLimit(k.inflight, k.concurrency)}</td>
                    <td class="muted">${fmtTime(k.last_used_at)}</td>
                    <td>
                      <div class="actions">
                        <button class="btn btn-sm" data-act="edit" data-id="${esc(k.id)}">编辑</button>
                        <button class="btn btn-sm" data-act="toggle" data-id="${esc(k.id)}">${k.enabled ? '禁用' : '启用'}</button>
                        <button class="btn btn-sm" data-act="reset" data-id="${esc(k.id)}">重置</button>
                        <button class="btn btn-sm btn-danger" data-act="del" data-id="${esc(k.id)}">删除</button>
                      </div>
                    </td>
                  </tr>
                `).join('') : `<tr><td colspan="9" class="muted">暂无 API Key，点击右上角“新建密钥”创建。</td></tr>`}
              </tbody>
            </table>
          </div>
        </div>
      `;

      const byId = new Map(keys.map((k) => [k.id, k]));
      document.querySelectorAll('[data-act]').forEach((btn) => {
        btn.onclick = async () => {
          const k = byId.get(btn.dataset.id);
          if (!k) return;
          const act = btn.dataset.act;
          if (act === 'edit') {
            openKeyEditor(k);
          } else if (act === 'toggle') {
            await apiFetch(`/admin/api/keys/${encodeURIComponent(k.id)}`, {
              method: 'PATCH',
              body: JSON.stringify({ enabled: !k.enabled }),
            });
            await loadKeys();
          } else if (act === 'reset') {
            showConfirm('重置密钥', `确认重置「${k.name}」的密钥吗？旧密钥将立即失效。`, async () => {
              const res = await apiFetch(`/admin/api/keys/${encodeURIComponent(k.id)}/reset`, { method: 'POST' });
              await loadKeys();
              showSecretModal('密钥已重置', res.key);
            });
          } else if (act === 'del') {
            showConfirm('删除密钥', `确认删除密钥「${k.name}」吗？此操作不可撤销。`, async () => {
              await apiFetch(`/admin/api/keys/${encodeURIComponent(k.id)}`, { method: 'DELETE' });
              await loadKeys();
            });
          }
        };
      });
    } catch (err) {
      document.getElementById('page-body').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
    }
  }

  async function fetchLogs(append) {
    const f = state.logsFilter;
    const params = new URLSearchParams();
    if (f.key) params.set('key', f.key);
    if (f.model) params.set('model', f.model);
    if (f.status) params.set('status', f.status);
    if (f.protocol) params.set('protocol', f.protocol);
    params.set('limit', '50');
    if (append && state.logsNext > 0) {
      params.set('before_id', String(state.logsNext));
    }
    const data = await apiFetch('/admin/api/logs?' + params.toString());
    if (append) {
      state.logsRows = state.logsRows.concat(data.logs || []);
    } else {
      state.logsRows = data.logs || [];
    }
    state.logsHasMore = Boolean(data.has_more);
    state.logsNext = Number(data.next_before_id || 0);
  }

  async function loadLogs(append = false) {
    if (!append) {
      renderShell('请求日志', `<button class="btn" id="l-refresh">刷新</button>`, `<div class="card">加载中...</div>`);
      document.getElementById('l-refresh').onclick = () => loadLogs(false);
    }
    try {
      await fetchLogs(append);
      const rows = state.logsRows;
      const f = state.logsFilter;
      const speed = (r) => {
        const decodeMs = (r.latency_ms || 0) - (r.ttft_ms || 0);
        if (!r.completion_tokens || decodeMs < 50) return '-';
        return (r.completion_tokens / (decodeMs / 1000)).toFixed(1) + ' tok/s';
      };
      const statusBadge = (r) => {
        if (r.status >= 200 && r.status < 400) {
          return `<span class="badge badge-ok">${r.status}</span>`;
        }
        return `<span class="badge badge-danger" title="${esc(r.error_code)}">${r.status}${r.error_code ? ' ' + esc(r.error_code) : ''}</span>`;
      };
      document.getElementById('page-body').innerHTML = `
        <div class="card">
          <form class="filter-bar" id="log-filter">
            <input id="lf-key" placeholder="密钥 ID 或名称" value="${esc(f.key)}">
            <input id="lf-model" placeholder="模型名称" value="${esc(f.model)}">
            <select id="lf-status">
              <option value="" ${f.status === '' ? 'selected' : ''}>全部状态</option>
              <option value="ok" ${f.status === 'ok' ? 'selected' : ''}>成功 (2xx/3xx)</option>
              <option value="error" ${f.status === 'error' ? 'selected' : ''}>错误 (4xx/5xx)</option>
            </select>
            <select id="lf-proto">
              <option value="" ${f.protocol === '' ? 'selected' : ''}>全部协议</option>
              <option value="chat" ${f.protocol === 'chat' ? 'selected' : ''}>chat</option>
              <option value="responses" ${f.protocol === 'responses' ? 'selected' : ''}>responses</option>
              <option value="messages" ${f.protocol === 'messages' ? 'selected' : ''}>messages</option>
            </select>
            <button class="btn btn-primary" type="submit">查询</button>
            <button class="btn" type="button" id="lf-reset">重置</button>
          </form>
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>密钥</th>
                  <th>协议</th>
                  <th>请求模型 → 实际模型</th>
                  <th>状态</th>
                  <th>Tokens (入/出/缓)</th>
                  <th>首字 / 总耗时</th>
                  <th>输出速率</th>
                </tr>
              </thead>
              <tbody>
                ${rows.length ? rows.map((r) => `
                  <tr>
                    <td class="muted num">${fmtTime(r.ts)}</td>
                    <td>${esc(r.key_name || r.key_id || '-')}</td>
                    <td><span class="badge badge-muted">${esc(r.protocol || 'chat')}</span></td>
                    <td class="mono">${esc(r.requested_model || '-')} → ${esc(r.model || '-')}</td>
                    <td>${statusBadge(r)}</td>
                    <td class="num">${fmtNum(r.prompt_tokens)} / ${fmtNum(r.completion_tokens)} / ${fmtNum(r.cached_tokens)}</td>
                    <td class="num">${r.ttft_ms} / ${r.latency_ms} ms</td>
                    <td class="num">${speed(r)}</td>
                  </tr>
                `).join('') : `<tr><td colspan="8" class="muted">暂无日志记录</td></tr>`}
              </tbody>
            </table>
          </div>
          ${state.logsHasMore ? `<div style="margin-top:14px;text-align:center"><button class="btn" id="logs-more">加载更多</button></div>` : ''}
        </div>
      `;
      document.getElementById('log-filter').onsubmit = (e) => {
        e.preventDefault();
        state.logsFilter = {
          key: document.getElementById('lf-key').value.trim(),
          model: document.getElementById('lf-model').value.trim(),
          status: document.getElementById('lf-status').value,
          protocol: document.getElementById('lf-proto').value,
        };
        loadLogs(false);
      };
      document.getElementById('lf-reset').onclick = () => {
        state.logsFilter = { key: '', model: '', status: '', protocol: '' };
        loadLogs(false);
      };
      const moreBtn = document.getElementById('logs-more');
      if (moreBtn) {
        moreBtn.onclick = () => loadLogs(true);
      }
    } catch (err) {
      document.getElementById('page-body').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
    }
  }

  async function loadUsage() {
    renderShell('用量统计', `<button class="btn" id="u-refresh">刷新</button>`, `<div class="card">加载中...</div>`);
    document.getElementById('u-refresh').onclick = loadUsage;
    try {
      const p = new URLSearchParams({ by: state.usageBy });
      if (state.usageSince) p.set('since', state.usageSince);
      const data = await apiFetch('/admin/api/usage?' + p.toString());
      const rows = data.rows || [];
      const dimLabel = { day: '日期', key: '密钥', model: '模型' }[state.usageBy] || '分组';
      document.getElementById('page-body').innerHTML = `
        <div class="card">
          <div class="filter-bar" style="justify-content:space-between">
            <div class="tabs">
              ${[['day', '按天'], ['key', '按密钥'], ['model', '按模型']].map(([k, label]) =>
                `<button type="button" data-by="${k}" class="${state.usageBy === k ? 'active' : ''}">${label}</button>`
              ).join('')}
            </div>
            <div class="actions">
              <span class="muted">起始日期：</span>
              <input id="u-since" type="date" value="${esc(state.usageSince)}" style="width:auto">
              <button class="btn" id="u-apply">筛选</button>
            </div>
          </div>
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>${dimLabel}</th>
                  <th>请求数</th>
                  <th>错误数</th>
                  <th>输入 Tokens</th>
                  <th>输出 Tokens</th>
                  <th>缓存 Tokens</th>
                  <th>平均首字</th>
                  <th>平均耗时</th>
                </tr>
              </thead>
              <tbody>
                ${rows.length ? rows.map((r) => `
                  <tr>
                    <td class="${state.usageBy === 'model' ? 'mono' : ''}">${esc(r.label)}</td>
                    <td class="num">${fmtNum(r.requests)}</td>
                    <td class="num">${fmtNum(r.errors)}</td>
                    <td class="num">${fmtNum(r.prompt_tokens)}</td>
                    <td class="num">${fmtNum(r.completion_tokens)}</td>
                    <td class="num">${fmtNum(r.cached_tokens)}</td>
                    <td class="num">${Math.round(r.avg_ttft_ms)} ms</td>
                    <td class="num">${Math.round(r.avg_latency_ms)} ms</td>
                  </tr>
                `).join('') : `<tr><td colspan="8" class="muted">暂无统计数据</td></tr>`}
              </tbody>
            </table>
          </div>
        </div>
      `;
      document.querySelectorAll('[data-by]').forEach((btn) => {
        btn.onclick = () => {
          state.usageBy = btn.dataset.by;
          loadUsage();
        };
      });
      document.getElementById('u-apply').onclick = () => {
        state.usageSince = document.getElementById('u-since').value;
        loadUsage();
      };
    } catch (err) {
      document.getElementById('page-body').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
    }
  }

  async function loadChannels() {
    renderShell('渠道与路由', `<button class="btn" id="c-refresh">刷新</button>`, `<div class="card">加载中...</div>`);
    document.getElementById('c-refresh').onclick = loadChannels;
    try {
      const d = await apiFetch('/admin/api/channels');
      const provs = d.providers || [];
      const groups = d.routing_groups || [];
      const aliases = d.model_aliases || [];
      const stats = d.hourly_stats || [];

      document.getElementById('page-body').innerHTML = `
        <div class="card">
          <div class="card-title"><span>上游提供方（Providers，只读）</span></div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>Base URL</th><th>目标</th><th>超时</th><th>特性</th><th>模型健康状态</th></tr></thead>
              <tbody>
                ${provs.map((p) => `
                  <tr>
                    <td><strong>${esc(p.name)}</strong></td>
                    <td class="mono">${esc(p.base_url)}</td>
                    <td><span class="badge ${p.target === 'local' ? 'badge-accent' : 'badge-muted'}">${esc(p.target)}</span></td>
                    <td>${esc(p.timeout)}</td>
                    <td>
                      ${p.has_system_prompt ? `<span class="badge badge-muted">system_prompt</span> ` : ''}
                      ${p.has_extra_body ? `<span class="badge badge-muted">extra_body</span>` : ''}
                      ${!p.has_system_prompt && !p.has_extra_body ? `<span class="muted">-</span>` : ''}
                    </td>
                    <td>
                      ${(p.models || []).map((m) => `
                        <span class="badge ${m.healthy ? 'badge-ok' : 'badge-danger'}" style="margin:2px">${esc(m.ref)} · ${m.healthy ? '正常' : '冷却中'}</span>
                      `).join('')}
                    </td>
                  </tr>
                `).join('')}
              </tbody>
            </table>
          </div>
        </div>

        <div class="two-col">
          <div class="card">
            <div class="card-title"><span>路由组（Routing Groups）</span></div>
            <div class="table-wrap">
              <table>
                <thead><tr><th>组名</th><th>类型</th><th>成员</th><th>规则数</th></tr></thead>
                <tbody>
                  ${groups.length ? groups.map((g) => `
                    <tr>
                      <td><strong>${esc(g.name)}</strong> ${g.default ? `<span class="badge badge-accent">默认</span>` : ''}</td>
                      <td>${esc(g.type)}</td>
                      <td class="mono">${esc((g.members || []).join(', '))}</td>
                      <td class="num">${g.rules_count}</td>
                    </tr>
                  `).join('') : `<tr><td colspan="4" class="muted">未配置路由组</td></tr>`}
                </tbody>
              </table>
            </div>
          </div>

          <div class="card">
            <div class="card-title"><span>模型别名（Model Aliases）</span></div>
            <div class="table-wrap">
              <table>
                <thead><tr><th>匹配模式</th><th>目标</th></tr></thead>
                <tbody>
                  ${aliases.length ? aliases.map((a) => `
                    <tr><td class="mono">${esc(a.Match || a.match)}</td><td class="mono">${esc(a.Use || a.use)}</td></tr>
                  `).join('') : `<tr><td colspan="2" class="muted">未配置模型别名</td></tr>`}
                </tbody>
              </table>
            </div>
          </div>
        </div>

        <div class="card" style="margin-top:16px">
          <div class="card-title"><span>近 1 小时各模型健康统计</span></div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>模型</th><th>请求数</th><th>错误数</th><th>成功率</th><th>平均首字</th><th>平均耗时</th></tr></thead>
              <tbody>
                ${stats.length ? stats.map((s) => `
                  <tr>
                    <td class="mono">${esc(s.model)}</td>
                    <td class="num">${fmtNum(s.requests)}</td>
                    <td class="num">${fmtNum(s.errors)}</td>
                    <td class="num">${s.success_rate.toFixed(1)}%</td>
                    <td class="num">${Math.round(s.avg_ttft_ms)} ms</td>
                    <td class="num">${Math.round(s.avg_latency_ms)} ms</td>
                  </tr>
                `).join('') : `<tr><td colspan="6" class="muted">近 1 小时暂无调用</td></tr>`}
              </tbody>
            </table>
          </div>
        </div>
      `;
    } catch (err) {
      document.getElementById('page-body').innerHTML = `<div class="alert alert-danger">${esc(err.message)}</div>`;
    }
  }

  function render() {
    state.route = parseRoute();
    if (!state.user || state.route === 'login') {
      renderLogin('');
      return;
    }
    switch (state.route) {
      case 'keys':
        loadKeys();
        break;
      case 'logs':
        loadLogs(false);
        break;
      case 'usage':
        loadUsage();
        break;
      case 'channels':
        loadChannels();
        break;
      default:
        state.route = 'overview';
        loadOverview();
    }
  }

  window.addEventListener('hashchange', render);

  (async function boot() {
    try {
      const me = await apiFetch('/admin/api/me');
      state.user = me.username;
      if (parseRoute() === 'login') {
        navigate('overview');
      } else {
        render();
      }
    } catch (_) {
      state.user = null;
      renderLogin('');
    }
  })();
})();
