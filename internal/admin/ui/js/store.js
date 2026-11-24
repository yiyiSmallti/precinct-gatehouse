import { api } from '/admin/js/api.js';
import { clone, sectionLabel, toast } from '/admin/js/ui.js';

const listeners = new Set();

function stableStringify(val) {
  if (val === null || typeof val !== 'object') return JSON.stringify(val);
  if (Array.isArray(val)) return `[${val.map(stableStringify).join(',')}]`;
  const keys = Object.keys(val).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${stableStringify(val[k])}`).join(',')}}`;
}

export const store = {
  server: null,
  latestServer: null,
  baseHash: '',
  drafts: {},
  applying: false,
  problems: [],
  conflict: null,
  buildError: '',

  subscribe(fn) {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },

  notify() {
    listeners.forEach((fn) => fn(this));
  },

  async refresh() {
    const cfg = await api.getConfig();
    this.latestServer = cfg;
    if (!this.isDirty()) {
      this.server = cfg;
      this.baseHash = cfg?.hash || '';
    } else {
      if (!this.baseHash) this.baseHash = this.server?.hash || '';
      if (cfg?.hash && this.baseHash && cfg.hash !== this.baseHash) {
        this.conflict = {
          currentHash: cfg.hash,
          message: '配置在您编辑期间已被其他操作修改，请重新加载后再应用。',
        };
      }
    }
    this.notify();
    return cfg;
  },

  getServerSection(sec) {
    const raw = this.server?.raw || {};
    if (sec === 'providers') return clone(raw.providers || []);
    if (sec === 'routing') {
      return clone({
        default_group: raw.default_group || '',
        routing_groups: raw.routing_groups || {},
      });
    }
    if (sec === 'aliases') return clone(raw.model_aliases || []);
    if (sec === 'redaction') {
      const r = raw.redaction || {};
      const mbt = r.mode_by_target || {};
      return clone({
        mode_by_target: {
          ...mbt,
          local: mbt.local || 'audit',
          external: mbt.external || 'mask',
        },
        categories: r.categories || {},
        dictionary: r.dictionary || '',
        terms: r.terms || [],
        never_mask: r.never_mask || [],
        internal_domains: r.internal_domains || [],
        detectors: r.detectors || [],
        fail_closed: r.fail_closed,
        strip_fields: r.strip_fields || [],
        vault_ttl: r.vault_ttl || '',
      });
    }
    if (sec === 'general') {
      return clone({
        max_body_mb: raw.max_body_mb ?? 32,
        request_log_days: raw.request_log_days ?? 90,
        tokens: { default_ttl: raw.tokens?.default_ttl || '' },
        listen: raw.listen || '',
        admin: { listen: raw.admin?.listen || '', session_ttl: raw.admin?.session_ttl || '' },
        // Keep every OTLP field (headers carry collector auth); the form edits only endpoint/interval.
        meter: { otlp: raw.meter?.otlp ? { ...raw.meter.otlp, endpoint: raw.meter.otlp.endpoint || '', interval: raw.meter.otlp.interval || '' } : null },
        audit: { dir: raw.audit?.dir || '' },
      });
    }
    if (sec === 'updater') {
      const u = raw.updater || {};
      const out = {
        enabled: Boolean(u.enabled),
        auto_apply: Boolean(u.auto_apply),
        source: u.source || '',
        interval: u.interval || '',
        drain_timeout: u.drain_timeout || '',
        health_timeout: u.health_timeout || '',
        keep_releases: u.keep_releases || 0,
        home_dir: u.home_dir || '',
        public_key: u.public_key || '',
        require_signature: u.require_signature,
        image: u.image || '',
        tag: u.tag || '',
        username: u.username || '',
        password_ref: u.password_ref || '',
        github_repo: u.github_repo || '',
        github_api_url: u.github_api_url || '',
        token_ref: u.token_ref || '',
        prerelease: Boolean(u.prerelease),
      };
      if (Array.isArray(u.fallback_dns)) out.fallback_dns = u.fallback_dns;
      return clone(out);
    }
    return {};
  },

  getSection(sec) {
    if (sec in this.drafts) return clone(this.drafts[sec]);
    return this.getServerSection(sec);
  },

  setDraft(sec, val) {
    const wasDirty = this.isDirty();
    if (!wasDirty) {
      this.baseHash = this.server?.hash || '';
    }
    const base = this.getServerSection(sec);
    this.problems = [];
    this.buildError = '';
    if (stableStringify(base) === stableStringify(val)) {
      delete this.drafts[sec];
      if (!this.isDirty()) {
        this.conflict = null;
        if (this.latestServer) {
          this.server = this.latestServer;
          this.baseHash = this.latestServer.hash || '';
        }
      }
    } else {
      this.drafts[sec] = clone(val);
    }
    this.notify();
  },

  dirtySections() {
    return Object.keys(this.drafts);
  },

  isDirty(sec = null) {
    if (sec) return sec in this.drafts;
    return Object.keys(this.drafts).length > 0;
  },

  discardAll() {
    this.drafts = {};
    this.problems = [];
    this.conflict = null;
    this.buildError = '';
    if (this.latestServer) {
      this.server = this.latestServer;
      this.baseHash = this.latestServer.hash || '';
    } else {
      this.baseHash = this.server?.hash || '';
    }
    this.notify();
  },

  async reloadAfterConflict() {
    this.conflict = null;
    this.problems = [];
    this.buildError = '';
    this.drafts = {};
    await this.refresh();
    toast('已重新加载最新生效配置', 'ok');
  },

  orderedDirtySections() {
    const keys = this.dirtySections();
    const order = ['providers', 'routing', 'aliases', 'redaction', 'general', 'updater'];
    if (keys.includes('providers')) {
      const oldNames = new Set((this.getServerSection('providers') || []).map((p) => p.name));
      const nextNames = new Set((this.drafts.providers || []).map((p) => p.name));
      let deleted = false;
      oldNames.forEach((n) => {
        if (!nextNames.has(n)) deleted = true;
      });
      if (deleted) {
        return ['aliases', 'routing', 'providers', 'redaction', 'general', 'updater'].filter((k) => keys.includes(k));
      }
    }
    return order.filter((k) => keys.includes(k));
  },

  async validateDrafts() {
    const order = this.orderedDirtySections();
    this.problems = [];
    this.buildError = '';
    if (!order.length) return true;
    const sections = {};
    for (const sec of order) {
      sections[sec] = this.drafts[sec];
    }
    await api.validateConfig({ sections });
    return true;
  },

  async applyAll() {
    const order = this.orderedDirtySections();
    if (!order.length || this.applying) return;
    this.applying = true;
    this.problems = [];
    this.conflict = null;
    this.buildError = '';
    this.notify();
    try {
      const hash = this.baseHash || this.server?.hash || '';
      const sections = {};
      for (const sec of order) {
        sections[sec] = this.drafts[sec];
      }
      const lastRes = await api.putSections(hash, sections);
      for (const sec of order) {
        delete this.drafts[sec];
      }
      const fresh = await api.getConfig();
      this.server = fresh;
      this.latestServer = fresh;
      this.baseHash = fresh?.hash || '';
      const rr = lastRes?.restart_required || this.server?.restart_required || [];
      if (rr.length) {
        toast(`配置已热重载生效（v${this.server?.version || ''}），部分启动项需重启: ${rr.join(', ')}`, 'warn', 5500);
      } else {
        toast(`配置已热重载生效 (${order.map(sectionLabel).join('、')})`, 'ok');
      }
    } catch (e) {
      if (e.status === 409) {
        this.conflict = {
          currentHash: e.currentHash,
          message: '配置在您编辑期间已被其他操作修改，请重新加载后再应用。',
        };
      } else if (e.status === 400) {
        this.problems = e.problems.length ? e.problems : [e.message];
      } else if (e.status === 422) {
        this.buildError = e.message || '运行组件构建失败';
      } else {
        this.buildError = e.message || '保存失败';
      }
      this.notify();
      throw e;
    } finally {
      this.applying = false;
      this.notify();
    }
  },
};
