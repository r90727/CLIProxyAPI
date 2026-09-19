const $ = (id) => document.getElementById(id);
const full = (n = 0) => Number(n).toLocaleString();
const compact = (n = 0) => Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 2 }).format(n);
const query = () => new URLSearchParams(['source', 'from', 'to'].map(k => [k, $(k).value]));
function empty(el, message = 'No recorded usage in this range.') { el.replaceChildren(); const p = document.createElement('p'); p.className = 'empty'; p.textContent = message; el.append(p); }
function rows(id, data) {
  const el = $(id); el.replaceChildren();
  if (!data.length) return empty(el);
  for (const v of data) { const row = document.createElement('div'); row.className = 'row'; const name = document.createElement('span'); name.textContent = v.name || 'Unknown'; name.title = name.textContent; const total = document.createElement('strong'); total.textContent = full(v.total); row.append(name, total); el.append(row); }
}
let requestVersion = 0;
async function refresh() {
  const version = ++requestVersion;
  try {
    const params = query(); $('export').href = '/api/export.csv?' + params;
    const response = await fetch('/api/summary?' + params); if (!response.ok) throw new Error(await response.text());
    const d = await response.json(); if (version !== requestVersion) return;
    const t = d.total[0] || {};
    for (const k of ['total', 'input', 'output', 'cached']) { $(k).textContent = compact(t[k]); $(k).title = full(t[k]); }
    $('records').textContent = full(t.events) + ' usage records';
    $('reasoning').textContent = full(t.reasoning) + ' reasoning tokens included';
    $('cache-ratio').textContent = (t.input ? (100 * t.cached / t.input).toFixed(1) : '0') + '% of input tokens';
    $('status').textContent = d.status.last_scan ? 'Last scan ' + new Date(d.status.last_scan).toLocaleTimeString() : 'Waiting for first scan';
    $('error').hidden = !d.status.error; $('error').textContent = d.status.error ? 'Import needs attention: ' + d.status.error : '';
    for (const group of ['providers', 'projects', 'threads']) rows(group, d[group]);
    $('models').replaceChildren();
    for (const m of d.models) { const tr = document.createElement('tr'); for (const key of ['name', 'input', 'output', 'cached', 'cache_write', 'reasoning', 'total']) { const td = document.createElement('td'); td.textContent = key === 'name' ? m[key] || 'Unknown' : full(m[key]); tr.append(td); } $('models').append(tr); }
    if (!d.models.length) { const tr = document.createElement('tr'); const td = document.createElement('td'); td.colSpan = 7; td.textContent = 'No recorded usage in this range.'; tr.append(td); $('models').append(tr); }
    const days = d.days.sort((a, b) => a.name.localeCompare(b.name)).slice(-30);
    $('chart').replaceChildren(); $('chart-labels').replaceChildren();
    const highest = Math.max(1, ...days.map(d => d.total));
    for (const day of days) { const bar = document.createElement('div'); bar.className = 'bar'; bar.style.height = Math.max(2, 100 * day.total / highest) + '%'; bar.title = day.name + ': ' + full(day.total) + ' tokens'; bar.setAttribute('aria-label', bar.title); $('chart').append(bar); }
    if (!days.length) empty($('chart'));
    for (const day of [days[0], days.at(-1)].filter(Boolean)) { const label = document.createElement('span'); label.textContent = day.name; $('chart-labels').append(label); }
    $('quota-panel').hidden = !d.quota || d.source !== 't3'; $('quotas').replaceChildren();
    if (d.quota) {
      $('quota-at').textContent = 'Reported ' + new Date(d.quota.at).toLocaleString();
      for (const key of ['primary', 'secondary']) { const q = d.quota.quota[key]; if (!q) continue; const row = document.createElement('div'); row.className = 'quota'; const label = document.createElement('span'); label.textContent = (q.window_minutes >= 1440 ? q.window_minutes / 1440 + '-day' : q.window_minutes / 60 + '-hour') + ' · ' + q.used_percent + '% used'; const progress = document.createElement('progress'); progress.max = 100; progress.value = q.used_percent; progress.setAttribute('aria-label', label.textContent); const reset = document.createElement('span'); reset.textContent = q.resets_at ? 'Resets ' + new Date(q.resets_at * 1000).toLocaleString() : 'Reset not reported'; row.append(label, progress, reset); $('quotas').append(row); }
    }
  } catch (error) { if (version !== requestVersion) return; $('error').hidden = false; $('error').textContent = 'Could not refresh usage: ' + error.message; $('status').textContent = 'Disconnected'; }
}
for (const key of ['source', 'from', 'to']) $(key).addEventListener('change', refresh);
$('reset').addEventListener('click', () => { $('from').value = ''; $('to').value = ''; refresh(); });
refresh(); setInterval(() => { if (!document.hidden) refresh(); }, 10000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
