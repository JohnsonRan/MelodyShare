'use strict';

// Shared helpers and API client for the admin app (loaded first).

let config = { r2Enabled: false };

const $ = (id) => document.getElementById(id);

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    location.href = '/login';
    throw new Error('未登录');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `请求失败 (${res.status})`);
  return data;
}

function button(text, onclick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'button button-quiet';
  b.textContent = text;
  b.onclick = onclick;
  return b;
}

function humanSize(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(1) + ' ' + units[i];
}

function fmtTime(ts) {
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false });
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('已复制到剪贴板');
  } catch {
    prompt('复制链接：', text);
  }
}

let toastTimer = null;
function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 2500);
}

// busy wraps an async click handler: the button shows immediate feedback and
// can't be double-fired while the request is in flight.
function busy(btn, busyText, fn) {
  btn.dataset.idleText = btn.dataset.idleText || btn.textContent;
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    btn.textContent = busyText;
    try {
      await fn();
    } finally {
      btn.disabled = false;
      btn.textContent = btn.dataset.idleText;
    }
  });
}
