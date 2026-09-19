const $ = id => document.getElementById(id);
const full = (n = 0) => Number(n).toLocaleString();
const money = (value = 0) => new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: value > 0 && value < .01 ? 4 : 2 }).format(value);
const costText = (c = {}) => c.unpriced_events && !c.priced_events ? 'Unpriced' : money(c.usd) + (c.unpriced_events ? ' + unpriced' : '');
const costDetail = (c = {}) => c.unpriced_events && !c.priced_events ? 'No estimate available for these records.' : 'Input ' + money(c.input_usd) + ' \u00b7 cache reads ' + money(c.cache_read_usd) + ' \u00b7 cache writes ' + money(c.cache_write_usd) + ' \u00b7 output ' + money(c.output_usd);
const compact = (n = 0) => Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 2 }).format(n);
const query = () => new URLSearchParams(['source', 'from', 'to'].map(k => [k, $(k).value]));
const el = (tag, className, text) => { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) node.textContent = text; return node; };
let quotaData, quotaMode = 'used', requestVersion = 0;
const views = { quota: 'quota', dashboard: 'usage', usage: 'usage', providers: 'accounts', 'auth-files': 'accounts', 'quick-start': 'help', config: 'config' };
function navigate() {
  const key = Object.hasOwn(views, location.hash.slice(1)) ? location.hash.slice(1) : 'quota';
  for (const section of document.querySelectorAll('main > section')) section.hidden = section.id !== 'view-' + views[key];
  for (const link of document.querySelectorAll('nav a')) {
    link.classList.toggle('active', link.dataset.view === key);
    link.setAttribute('aria-label', link.querySelector('span').textContent);
    if (link.dataset.view === key) link.setAttribute('aria-current', 'page'); else link.removeAttribute('aria-current');
  }
  const titles = { quota: 'Quota Management', dashboard: 'Dashboard', usage: 'Token Usage', providers: 'AI Providers', 'auth-files': 'Auth Files', 'quick-start': 'Quick Start', config: 'Config Panel' };
  document.title = 'CPAMC · ' + titles[key];
  $('usage-heading').textContent = titles[key];
  $('accounts-heading').textContent = titles[key];
}
function resetText(date) {
  if (!date) return ['No reset pending', ''];
  const when = new Date(date), minutes = Math.ceil((when - Date.now()) / 60000);
  if (!Number.isFinite(when.getTime())) return ['Reset not reported', ''];
  let relative = 'Awaiting new window';
  if (minutes > 0) relative = minutes >= 1440 ? 'Resets in ' + Math.ceil(minutes / 1440) + ' days' : minutes >= 60 ? 'Resets in ' + Math.ceil(minutes / 60) + ' hours' : 'Resets in ' + minutes + ' min';
  return [relative, when.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false })];
}
function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); svg.classList.add('icon'); svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS(svg.namespaceURI, 'use'); use.setAttribute('href', '#' + name); svg.append(use); return svg;
}
function renderWindow(w) {
  const remaining = Math.max(0, 100 - w.used_percent), value = quotaMode === 'remaining' ? remaining : w.used_percent;
  const cell = el('div', 'limit-cell'), top = el('div', 'limit-label');
  top.append(el('span', '', w.label), el('strong', '', Number(value.toFixed(1)) + '%'));
  const bar = el('progress', 'limit-progress' + (remaining < 20 ? ' danger' : remaining < 65 ? ' warning' : ''));
  bar.max = 100; bar.value = Math.min(100, value); bar.setAttribute('aria-label', w.label + ': ' + Number(value.toFixed(1)) + '% ' + quotaMode);
  bar.title = full(w.used_percent) + '% used · ' + full(remaining) + '% remaining';
  const reset = el('div', 'limit-reset'), [relative, timestamp] = resetText(w.resets_at);
  reset.append(document.createTextNode(relative));
  if (timestamp) { reset.append(document.createTextNode(' · ')); const time = el('time', '', timestamp); time.dateTime = w.resets_at; reset.append(time); }
  cell.append(top, bar, reset); return cell;
}
function renderAccount(a) {
  const row = el('article', 'account-row'); row.dataset.account = a.id;
  const info = el('div', 'account-info');
  const name = el('div', 'account-name', a.label); name.title = a.label;
  info.append(name, el('div', 'account-plan', a.plan));
  if (a.updated_at) {
    const age = Date.now() - new Date(a.updated_at).getTime(), stale = age > (a.provider === 'claude' ? 600000 : 3600000);
    const updated = el('div', 'account-updated' + (stale || a.error ? ' stale' : ''), (stale || a.error ? 'Last known · ' : 'Updated ') + new Date(a.updated_at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }));
    updated.title = a.source + ' · ' + new Date(a.updated_at).toLocaleString(); info.append(updated);
  }
  const limits = el('div', 'limit-grid');
  for (const w of a.windows || []) limits.append(renderWindow(w));
  if (!a.windows?.length) limits.append(el('div', 'no-quota', a.refreshing ? 'Fetching account quota…' : 'Quota unavailable'));
  if (a.provider === 'codex') {
    const available = a.manual_resets?.available, applicable = a.manual_resets?.applicable;
    const detail = el('div', 'quota-detail');
    detail.append(el('div', 'detail-label', 'Manual resets'), el('strong', '', available == null ? 'Not reported' : available + ' available'), el('small', '', available == null ? 'Reset allowance is not included in this snapshot.' : available === 0 ? 'No manual resets available on this account.' : applicable == null ? 'Replenishment date not reported.' : applicable + ' applicable to the current limit.'));
    limits.append(detail);
  }
  const actions = el('div', 'quota-actions'), button = el('button', 'refresh-quota');
  button.append(icon('refresh'), document.createTextNode(a.refreshing ? 'Refreshing…' : 'Refresh quota'));
  button.disabled = a.refreshing; button.setAttribute('aria-label', 'Refresh ' + a.provider + ' quota');
  button.addEventListener('click', async () => {
    button.disabled = true;
    try {
      const r = await fetch('/api/quotas/refresh?provider=' + encodeURIComponent(a.provider), { method: 'POST' }); if (!r.ok) throw new Error(await r.text());
      await refreshQuotas();
    } catch (error) { showError(error.message); } finally { button.disabled = false; }
  });
  actions.append(button); row.append(info, limits, actions);
  if (a.error) row.append(el('div', 'quota-error', a.error));
  return row;
}
function renderQuotas() {
  if (!quotaData) return;
  for (const provider of ['claude', 'codex']) {
    const accounts = quotaData[provider] || []; $(provider + '-count').textContent = accounts.length;
    $(provider + '-accounts').replaceChildren(...accounts.map(renderAccount));
  }
  const accounts = [...(quotaData.claude || []), ...(quotaData.codex || [])];
  $('account-count').textContent = accounts.length;
  $('account-details').replaceChildren();
  for (const a of accounts) {
    const item = el('article', 'account-file');
    item.append(el('h2', '', a.label), el('div', '', a.plan), el('code', '', a.credential), el('div', 'note', a.error || a.source));
    $('account-details').append(item);
  }
  $('quota-status').textContent = 'Quota ' + quotaMode + ' · ' + accounts.length + ' local accounts';
}
function showError(message) { $('error').textContent = message; $('error').hidden = !message; }
async function refreshQuotas() {
  const response = await fetch('/api/quotas'); if (!response.ok) throw new Error(await response.text());
  quotaData = await response.json(); renderQuotas();
}
function empty(node, message = 'No recorded usage in this range.') { node.replaceChildren(el('p', 'empty', message)); }
function rows(id, data) {
  const node = $(id); node.replaceChildren(); if (!data.length) return empty(node);
  for (const v of data) { const row = el('div', 'row'), name = el('span', '', v.name || 'Unknown'); name.title = name.textContent; const amounts = el('div', 'row-amounts'); amounts.append(el('strong', '', costText(v.cost)), el('small', '', full(v.total) + ' tokens')); amounts.title = costDetail(v.cost); row.append(name, amounts); node.append(row); }
}
async function refresh() {
  const version = ++requestVersion;
  try {
    const params = query(); $('export').href = '/api/export.csv?' + params;
    const response = await fetch('/api/summary?' + params); if (!response.ok) throw new Error(await response.text());
    const d = await response.json(); if (version !== requestVersion) return;
    const t = d.total[0] || {};
    $('cost').textContent = costText(t.cost);
    $('cost-coverage').textContent = t.cost?.unpriced_events ? full(t.cost.unpriced_events) + ' records unpriced \u00b7 partial estimate' : 'API-equivalent value of recorded usage';
    $('cost-breakdown').replaceChildren(...[['Uncached input', 'input_usd'], ['Cache reads', 'cache_read_usd'], ['Cache writes', 'cache_write_usd'], ['Output', 'output_usd']].map(([label, key]) => { const item = el('div'); item.append(el('span', '', label), el('strong', '', t.cost?.unpriced_events && !t.cost?.priced_events ? '\u2014' : money(t.cost?.[key]))); return item; }));
    $('rates-checked').textContent = d.pricing.checked;
    $('rates').replaceChildren();
    for (const [model, r] of Object.entries(d.pricing.rates).sort(([a], [b]) => a.localeCompare(b))) { const tr = el('tr'); tr.append(el('td', '', model)); for (const key of ['input', 'cached', 'cache_write', 'output']) tr.append(el('td', '', '$' + r[key])); $('rates').append(tr); }
    for (const k of ['total', 'input', 'output', 'cached']) { $(k).textContent = compact(t[k]); $(k).title = full(t[k]); }
    $('records').textContent = full(t.events) + ' usage records';
    $('reasoning').textContent = full(t.reasoning) + ' reasoning tokens included';
    $('cache-ratio').textContent = (t.input ? (100 * t.cached / t.input).toFixed(1) : '0') + '% of input tokens';
    $('status').textContent = d.status.last_scan ? 'Last scan ' + new Date(d.status.last_scan).toLocaleTimeString() : 'Waiting for first scan';
    showError(d.status.error ? 'Import needs attention: ' + d.status.error : '');
    for (const group of ['providers', 'projects', 'threads']) rows(group, d[group]);
    $('models').replaceChildren();
    for (const m of d.models) { const tr = el('tr'); for (const key of ['name', 'input', 'output', 'cached', 'cache_write', 'reasoning', 'total']) tr.append(el('td', '', key === 'name' ? m[key] || 'Unknown' : full(m[key]))); const cost = el('td', '', costText(m.cost)); cost.title = costDetail(m.cost); tr.append(cost); $('models').append(tr); }
    if (!d.models.length) { const tr = el('tr'), td = el('td', '', 'No recorded usage in this range.'); td.colSpan = 8; tr.append(td); $('models').append(tr); }
    const days = d.days.sort((a, b) => a.name.localeCompare(b.name)).slice(-30), highest = Math.max(1, ...days.map(d => d.total));
    $('chart').replaceChildren(); $('chart-labels').replaceChildren();
    for (const day of days) { const bar = el('div', 'bar'); bar.style.height = Math.max(2, 100 * day.total / highest) + '%'; bar.title = day.name + ': ' + full(day.total) + ' tokens \u00b7 ' + costText(day.cost); bar.setAttribute('aria-label', bar.title); $('chart').append(bar); }
    if (!days.length) empty($('chart'));
    for (const day of [days[0], days.at(-1)].filter(Boolean)) $('chart-labels').append(el('span', '', day.name));
  } catch (error) { if (version === requestVersion) { showError('Could not refresh usage: ' + error.message); $('status').textContent = 'Disconnected'; } }
}
for (const mode of ['remaining', 'used']) $(mode + '-mode').addEventListener('click', () => {
  quotaMode = mode;
  for (const value of ['remaining', 'used']) { $(value + '-mode').classList.toggle('selected', value === mode); $(value + '-mode').setAttribute('aria-pressed', String(value === mode)); }
  renderQuotas();
});
$('collapse').addEventListener('click', () => { document.body.classList.toggle('collapsed'); $('collapse').setAttribute('aria-label', document.body.classList.contains('collapsed') ? 'Expand sidebar' : 'Collapse sidebar'); });
for (const key of ['source', 'from', 'to']) $(key).addEventListener('change', refresh);
$('reset').addEventListener('click', () => { $('from').value = ''; $('to').value = ''; refresh(); });
window.addEventListener('hashchange', navigate); navigate();
async function updateAll() { await refresh(); try { await refreshQuotas(); } catch (error) { showError('Could not refresh quotas: ' + error.message); } }
updateAll(); setInterval(() => { if (!document.hidden) updateAll(); }, 10000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) updateAll(); });
