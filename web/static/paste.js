'use strict';

const $ = (id) => document.getElementById(id);

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    prompt('复制链接：', text);
  }
}

$('pasteForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const errEl = $('pasteError');
  errEl.hidden = true;
  const btn = $('submitBtn');
  btn.disabled = true;
  try {
    const res = await fetch('/p', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        'Accept': 'application/json',
      },
      body: new URLSearchParams({ content: $('content').value, ttl: $('ttl').value }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `请求失败 (${res.status})`);
    $('result').hidden = false;
    $('resultURL').textContent = data.url;
    $('result').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    $('copyBtn').onclick = async () => {
      await copyText(data.url);
      $('copyBtn').textContent = '已复制';
      setTimeout(() => { $('copyBtn').textContent = '复制链接'; }, 1500);
    };
  } catch (err) {
    errEl.textContent = err.message;
    errEl.hidden = false;
  } finally {
    btn.disabled = false;
  }
});
