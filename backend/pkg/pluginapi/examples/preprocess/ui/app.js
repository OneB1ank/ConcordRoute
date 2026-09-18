'use strict';
const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
const pending = new Map();
const statusElement = document.getElementById('status');
const buttons = [...document.querySelectorAll('button')];
let sequence = 0;
let saved = {};
function send(type, extra = {}) {
  const id = String(++sequence);
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { pending.delete(id); reject(new Error('Host request timed out')); }, 30000);
    pending.set(id, { resolve, reject, timeout });
    parent.postMessage({ source: 'sub2api-plugin-ui', bridge_token: token, type, request_id: id, ...extra }, '*');
  });
}
window.addEventListener('message', event => {
  const data = event.data;
  if (event.source !== parent || !data || data.source !== 'sub2api-plugin-host' || data.bridge_token !== token) return;
  const request = pending.get(data.request_id);
  if (!request) return;
  clearTimeout(request.timeout);
  pending.delete(data.request_id);
  if (data.ok) request.resolve(data);
  else request.reject(new Error(data.error || data.result?.message || 'Host rejected the request'));
});
window.addEventListener('pagehide', () => {
  for (const request of pending.values()) { clearTimeout(request.timeout); request.reject(new Error('Page closed')); }
  pending.clear();
});
async function run(action) {
  buttons.forEach(button => { button.disabled = true; });
  try { await action(); }
  catch (error) { statusElement.textContent = error.message; }
  finally { buttons.forEach(button => { button.disabled = false; }); }
}
async function save() {
  const limit = Number(document.getElementById('limit').value);
  if (!Number.isSafeInteger(limit) || limit < 1 || limit > 1000000) throw new Error('Enter a token limit from 1 to 1000000');
  const result = await send('config.save', { config: { ...saved, max_output_tokens: limit, deny_model: document.getElementById('model').value } });
  saved = result.config;
  statusElement.textContent = 'Configuration saved';
}
document.getElementById('save').addEventListener('click', () => run(save));
document.getElementById('test').addEventListener('click', () => run(async () => {
  await save();
  const response = await send('config.test');
  if (!response.result?.success) throw new Error(response.result?.message || 'Configuration test failed');
  statusElement.textContent = 'Configuration test passed';
}));
if (token && parent !== window) {
  parent.postMessage({ source: 'sub2api-plugin-ui', bridge_token: token, type: 'sub2api.plugin.ready' }, '*');
  run(async () => {
    const result = await send('config.load');
    saved = result.config;
    document.getElementById('limit').value = saved.max_output_tokens || 1024;
    document.getElementById('model').value = saved.deny_model || '';
    statusElement.textContent = 'Ready';
  });
} else statusElement.textContent = 'Open this configuration page from Plugin Management.';
