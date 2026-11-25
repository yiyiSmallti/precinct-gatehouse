import { api } from '/admin/js/api.js';

const listeners = new Set();

const DEFAULT_FAVICON = '/admin/img/favicon.png';
const DEFAULT_TOUCH_ICON = '/admin/img/apple-touch-icon.png';

export const branding = {
  state: {
    display_name: '',
    has_logo: false,
    logo_url: '',
    public_url: '',
    dist_template: '',
    settingsLoaded: false,
  },

  subscribe(fn) {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },

  notify() {
    listeners.forEach((fn) => fn(this.state));
  },

  async loadPublic() {
    const res = await api.getBranding();
    this.state.display_name = res.display_name || '';
    this.state.has_logo = Boolean(res.has_logo);
    this.state.logo_url = res.logo_url || '';
    this.applyDocumentMeta();
    this.notify();
    return this.state;
  },

  async loadSettings() {
    const res = await api.getBrandingSettings();
    this.state.display_name = res.display_name || '';
    this.state.has_logo = Boolean(res.has_logo);
    this.state.logo_url = res.logo_url || '';
    this.state.public_url = res.public_url || '';
    this.state.dist_template = res.dist_template || '';
    this.state.settingsLoaded = true;
    this.applyDocumentMeta();
    this.notify();
    return this.state;
  },

  async ensureSettingsLoaded() {
    if (this.state.settingsLoaded) return this.state;
    return this.loadSettings();
  },

  applyUpdate(res = {}) {
    if ('display_name' in res) this.state.display_name = res.display_name || '';
    if ('has_logo' in res) this.state.has_logo = Boolean(res.has_logo);
    if ('logo_url' in res) this.state.logo_url = res.logo_url || '';
    if ('public_url' in res) this.state.public_url = res.public_url || '';
    if ('dist_template' in res) this.state.dist_template = res.dist_template || '';
    this.applyDocumentMeta();
    this.notify();
  },

  getDisplayName() {
    return this.state.display_name || 'Precinct Gatehouse';
  },

  getSidebarTitle() {
    return this.state.display_name || '门禁网关';
  },

  getSidebarSub() {
    return this.state.display_name ? '模型网关控制台' : 'Precinct Gatehouse';
  },

  getLoginSub() {
    return this.state.display_name ? `登录 ${this.state.display_name} 控制台` : '登录门禁网关控制台';
  },

  getSmallLogoUrl() {
    return this.state.has_logo && this.state.logo_url ? this.state.logo_url : '/admin/img/icon-128.png';
  },

  getHeroLogoUrl() {
    return this.state.has_logo && this.state.logo_url ? this.state.logo_url : '/admin/img/icon-512.png';
  },

  applyDocumentMeta() {
    if (typeof document === 'undefined') return;
    document.title = this.state.display_name ? `${this.state.display_name} - 控制台` : 'Gatehouse 网关控制台';
    const iconUrl = this.state.has_logo && this.state.logo_url ? this.state.logo_url : DEFAULT_FAVICON;
    const touchUrl = this.state.has_logo && this.state.logo_url ? this.state.logo_url : DEFAULT_TOUCH_ICON;
    const iconEl = document.querySelector('link[rel="icon"]');
    if (iconEl) iconEl.setAttribute('href', iconUrl);
    const touchEl = document.querySelector('link[rel="apple-touch-icon"]');
    if (touchEl) touchEl.setAttribute('href', touchUrl);
  },
};
