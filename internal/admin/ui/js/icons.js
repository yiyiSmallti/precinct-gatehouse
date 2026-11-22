const SVG_NS = 'http://www.w3.org/2000/svg';

const PATHS = {
  gatehouse: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16M15 4v16M3 10h6M15 14h6M9 12h6"/>',
  overview: '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  keys: '<circle cx="8" cy="14" r="3.5"/><path d="M11 11.5L19.5 4M17 6.5l2.5 2.5M14.5 9l2 2"/>',
  channels: '<rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><circle cx="7" cy="7" r="0.8" fill="currentColor"/><circle cx="7" cy="17" r="0.8" fill="currentColor"/>',
  routing: '<circle cx="5" cy="12" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="19" cy="18" r="2"/><path d="M7 12h4c2 0 3-6 6-6M11 12c2 0 3 6 6 6"/>',
  redaction: '<path d="M12 3l7 3v5.5c0 4.5-3 7.8-7 9.5-4-1.7-7-5-7-9.5V6l7-3z"/><path d="M9.5 12l1.8 1.8 3.5-3.8"/>',
  logs: '<path d="M5 5h14M5 10h14M5 15h10M5 19h7"/>',
  usage: '<path d="M4 19h16"/><rect x="6" y="11" width="3" height="8" rx="0.5"/><rect x="11" y="7" width="3" height="12" rx="0.5"/><rect x="16" y="4" width="3" height="15" rx="0.5"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M12 3v2.2M12 18.8V21M3 12h2.2M18.8 12H21M5.6 5.6l1.6 1.6M16.8 16.8l1.6 1.6M18.4 5.6l-1.6 1.6M7.2 16.8l-1.6 1.6"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M18.7 5.3l-1.4 1.4M6.7 17.3l-1.4 1.4"/>',
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
  menu: '<path d="M4 7h16M4 12h16M4 17h16"/>',
  close: '<path d="M6 6l12 12M18 6L6 18"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  trash: '<path d="M4 7h16M9 4h6M7 7l1 12a1 1 0 0 0 1 1h6a1 1 0 0 0 1-1l1-12M10 11v5M14 11v5"/>',
  edit: '<path d="M4 20h4l10.5-10.5a1.5 1.5 0 0 0-2.1-2.1L6 18v2zM13.5 6.5l4 4"/>',
  refresh: '<path d="M20 11a8 8 0 1 0 2 5.3M20 4v7h-7"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="1.5"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>',
  check: '<path d="M5 12.5l4.5 4.5 9.5-10"/>',
  alert: '<path d="M12 3.5L2.8 19.5h18.4L12 3.5z"/><path d="M12 10v4.5M12 17.2v.4"/>',
  arrowUp: '<path d="M12 18V6M6 12l6-6 6 6"/>',
  arrowDown: '<path d="M12 6v12M6 12l6 6 6-6"/>',
  ping: '<path d="M5 12h3l2-5 4 10 2-5h3"/>',
  logout: '<path d="M14 5h4a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1h-4M10 17l5-5-5-5M15 12H4"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="1.5"/><path d="M8 11V7.5a4 4 0 0 1 8 0V11"/>',
  history: '<path d="M4 12a8 8 0 1 0 2.3-5.6L4 8.7M4 4v4.7h4.7"/><path d="M12 7.5V12l3 2"/>',
};

export function icon(name, size = 16, cls = '') {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.5');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  if (cls) svg.setAttribute('class', cls);
  svg.innerHTML = PATHS[name] || PATHS.alert;
  return svg;
}
