'use strict';

const CHUNK_RETRIES = 3;

// All chunk uploads across all files share this connection pool. Over
// HTTP/1.1 the browser has only ~6 connections per origin and a chunk PUT
// holds one for its whole duration — cap below that so one connection stays
// free and settings/delete/list clicks keep responding mid-upload. h2/h3
// multiplex requests, so only bandwidth matters there.
const netSlots = (() => {
  const nav = performance.getEntriesByType('navigation')[0];
  const max = /^h[23]/.test((nav && nav.nextHopProtocol) || '') ? 8 : 5;
  let used = 0;
  const waiters = [];
  return {
    max,
    async acquire() {
      if (used < max) { used++; return; }
      await new Promise((r) => waiters.push(r)); // slot handed over in release()
    },
    release() {
      const w = waiters.shift();
      if (w) w();
      else used--;
    },
  };
})();

let config = { r2Enabled: false };

const $ = (id) => document.getElementById(id);
const activeUploads = new Set(); // upload ids currently in flight

// Resume bookkeeping: remember upload ids per file fingerprint so re-selecting
// the same file after an interruption continues where it left off.
const resumeKey = (file, storage) =>
  `share-up:${storage}:${file.name}:${file.size}:${file.lastModified}`;

// ---- api helper ----

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

// ---- chunk upload ----

// putRelay streams the chunk through our server.
function putRelay(uploadId, idx, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', `/api/uploads/${uploadId}/chunks/${idx}`);
    xhr.responseType = 'json';
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error((xhr.response && xhr.response.error) || `分片 ${idx} 上传失败 (${xhr.status})`));
    };
    xhr.onerror = () => reject(new Error(`分片 ${idx} 网络错误`));
    xhr.send(blob);
  });
}

// putDirect PUTs the chunk straight to a presigned R2 URL and returns the ETag.
function putDirect(url, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      const etag = xhr.getResponseHeader('ETag');
      if (xhr.status >= 200 && xhr.status < 300 && etag) resolve(etag);
      else reject(new Error(etag ? `直传失败 (${xhr.status})` : '直传响应缺少 ETag（检查 R2 CORS 配置）'));
    };
    xhr.onerror = () => reject(new Error('直传网络错误（可能是 R2 CORS 未配置）'));
    xhr.send(blob);
  });
}

async function uploadFile(file, opts, onProgress, onStatus) {
  const key = resumeKey(file, opts.storage);

  // Try to resume a previous upload of this exact file.
  let init = null;
  let received = new Set();
  const storedId = localStorage.getItem(key);
  if (storedId) {
    try {
      const st = await api('GET', `/api/uploads/${storedId}`);
      init = st;
      received = new Set(st.received);
      if (received.size) onStatus(`断点续传：已完成 ${received.size}/${st.totalChunks} 片`);
    } catch {
      localStorage.removeItem(key);
    }
  }
  if (!init) {
    init = await api('POST', '/api/uploads', {
      name: file.name,
      size: file.size,
      storage: opts.storage,
      slug: opts.slug || '',
      expiresIn: opts.expiresIn,
      password: opts.password,
      maxDownloads: opts.maxDownloads || 0,
    });
    localStorage.setItem(key, init.id);
  }

  const { id, chunkSize, totalChunks } = init;
  if (activeUploads.has(id)) throw new Error('该文件已在上传中');
  activeUploads.add(id);

  try {
    const chunkBytes = (idx) => Math.min((idx + 1) * chunkSize, file.size) - idx * chunkSize;
    const loaded = new Array(totalChunks).fill(0);
    for (const idx of received) loaded[idx] = chunkBytes(idx);
    const report = (th) => onProgress(loaded.reduce((a, b) => a + b, 0) / file.size, th);
    report();

    // R2 uploads go browser→R2 directly; fall back to relaying through the
    // server if the bucket's CORS isn't set up.
    let direct = opts.storage === 'r2';
    async function sendChunk(idx, blob, onp) {
      if (direct) {
        try {
          const { url } = await api('GET', `/api/uploads/${id}/chunks/${idx}/url`);
          const etag = await putDirect(url, blob, onp);
          await api('POST', `/api/uploads/${id}/chunks/${idx}/etag`, { etag });
          return;
        } catch (err) {
          if (!direct) throw err;
          direct = false;
          onp(0);
          onStatus('R2 直传不可用，回退为服务器中转');
        }
      }
      await putRelay(id, idx, blob, onp);
    }

    const indexes = Array.from({ length: totalChunks }, (_, i) => i).filter((i) => !received.has(i));

    // Concurrency: a fixed thread count, or (auto) AIMD-style hill climbing —
    // add a thread while throughput keeps improving, back off on regression
    // or chunk errors.
    const manual = parseInt($('concurrency').value, 10);
    const auto = Number.isNaN(manual);
    let target = auto ? 3 : Math.min(netSlots.max, Math.max(1, manual));
    let onChunkError = () => {};

    const totalLoaded = () => loaded.reduce((a, b) => a + b, 0);
    const threads = () => target;

    async function uploadOne(idx) {
      const blob = file.slice(idx * chunkSize, Math.min((idx + 1) * chunkSize, file.size));
      let lastErr = null;
      for (let attempt = 0; attempt < CHUNK_RETRIES; attempt++) {
        await netSlots.acquire();
        try {
          await sendChunk(idx, blob, (n) => { loaded[idx] = n; report(threads()); });
          loaded[idx] = blob.size;
          report(threads());
          return;
        } catch (err) {
          lastErr = err;
          loaded[idx] = 0;
          onChunkError();
        } finally {
          netSlots.release();
        }
        // back off without holding a network slot
        await new Promise((r) => setTimeout(r, 1000 * (attempt + 1)));
      }
      throw lastErr;
    }

    if (indexes.length) {
      await new Promise((resolve, reject) => {
        let inflight = 0;
        let failed = null;
        let timer = null;
        let done = false;

        const finish = () => {
          if (done) return;
          done = true;
          if (timer) clearInterval(timer);
          failed ? reject(failed) : resolve();
        };
        const pump = () => {
          while (!failed && inflight < target && indexes.length) {
            const idx = indexes.shift();
            inflight++;
            uploadOne(idx)
              .catch((err) => { failed = failed || err; })
              .finally(() => {
                inflight--;
                if (inflight === 0 && (failed || !indexes.length)) finish();
                else pump();
              });
          }
        };

        if (auto) {
          const TICK = 4000;
          let lastBytes = totalLoaded();
          let baseRate = 0;
          let probing = false;
          let holdUntil = 0;
          onChunkError = () => {
            target = Math.max(1, Math.floor(target / 2));
            probing = false;
            holdUntil = Date.now() + 20000;
          };
          timer = setInterval(() => {
            const bytes = totalLoaded();
            const rate = bytes - lastBytes;
            lastBytes = bytes;
            const now = Date.now();
            if (rate <= 0 || now < holdUntil || !indexes.length) return;
            if (probing) {
              probing = false;
              if (rate < baseRate * 0.95) { // extra thread didn't help — revert
                target = Math.max(1, target - 1);
                holdUntil = now + 30000;
              }
            } else if (target < netSlots.max) {
              baseRate = rate;
              target++;
              probing = true;
              pump();
            }
          }, TICK);
        }

        pump();
      });
    }
    // on failure the promise above rejects and server state + localStorage
    // are kept for resume

    const res = await api('POST', `/api/uploads/${id}/complete`);
    localStorage.removeItem(key);
    return res;
  } finally {
    activeUploads.delete(id);
  }
}

// ---- upload queue UI ----

function enqueueFiles(files) {
  const opts = {
    storage: $('storage').value,
    expiresIn: parseInt($('expiry').value, 10),
    password: $('uploadPassword').value,
    maxDownloads: parseInt($('maxDownloads').value, 10) || 0,
    slug: '',
  };
  const slug = $('customSlug').value.trim();
  if (slug) {
    if (files.length === 1) opts.slug = slug;
    else toast('自定义链接仅支持单文件上传，已忽略');
    $('customSlug').value = '';
  }
  for (const file of files) startUpload(file, opts);
}

function startUpload(file, opts) {
  const item = document.createElement('div');
  item.className = 'upload-item';
  item.innerHTML = `
    <div class="upload-file-icon" aria-hidden="true">↑</div>
    <div class="upload-body">
      <div class="upload-head">
        <span class="upload-name"></span>
        <span class="upload-status">准备中</span>
      </div>
      <div class="progress" role="progressbar" aria-label="上传进度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0"><div></div></div>
      <div class="upload-link" hidden></div>
    </div>`;
  item.querySelector('.upload-name').textContent = `${file.name} (${humanSize(file.size)})`;
  $('queue').prepend(item);

  const statusEl = item.querySelector('.upload-status');
  const progressEl = item.querySelector('.progress');
  const barEl = item.querySelector('.progress > div');

  // Progress events fire far more often than the screen refreshes, so batch
  // DOM writes to one per frame.
  let last = null;
  let settled = false;
  const paint = () => {
    const p = last;
    last = null;
    if (settled || !p) return;
    const pct = Math.min(100, Math.round(p.frac * 100));
    progressEl.setAttribute('aria-valuenow', String(pct));
    barEl.style.width = pct + '%';
    statusEl.textContent = p.threads ? `${pct}% · ${p.threads} 线程` : `${pct}%`;
  };

  uploadFile(file, opts, (frac, threads) => {
    if (!last) requestAnimationFrame(paint);
    last = { frac, threads };
  }, (msg) => toast(msg)).then((res) => {
    settled = true;
    progressEl.setAttribute('aria-valuenow', '100');
    barEl.style.width = '100%';
    statusEl.textContent = '已完成';
    item.classList.add('is-complete');
    const linkEl = item.querySelector('.upload-link');
    linkEl.hidden = false;
    const code = document.createElement('code');
    code.textContent = res.url;
    const copyBtn = button('复制链接', () => copyText(res.url));
    const openLink = document.createElement('a');
    openLink.className = 'button button-quiet';
    openLink.href = res.url;
    openLink.target = '_blank';
    openLink.rel = 'noopener';
    openLink.textContent = '打开';
    linkEl.append(code, copyBtn, openLink);
    refreshFiles();
  }).catch((err) => {
    settled = true;
    statusEl.textContent = `上传失败：${err.message}`;
    item.classList.add('is-error');
    barEl.style.background = 'var(--danger)';
    const recovery = document.createElement('p');
    recovery.className = 'notice';
    recovery.textContent = '再次选择同一文件可继续上传。';
    item.querySelector('.upload-body').append(recovery);
  });
}

// ---- file list ----

async function refreshFiles() {
  const data = await api('GET', '/api/files');
  const tbody = $('fileList');
  tbody.replaceChildren();
  $('emptyHint').hidden = data.files.length > 0;
  for (const f of data.files) tbody.append(fileRow(f));
  refreshStats().catch(() => {});
}

async function refreshStats() {
  const s = await api('GET', '/api/stats');
  const parts = [];
  const local = s.local || { bytes: 0, count: 0 };
  let localText = `本地 ${humanSize(local.bytes)}（${local.count} 个文件`;
  if (s.localFree >= 0) localText += `，剩余 ${humanSize(s.localFree)}`;
  parts.push(localText + '）');
  if (s.r2 && s.r2.count > 0) parts.push(`R2 ${humanSize(s.r2.bytes)}（${s.r2.count} 个文件）`);
  $('stats').textContent = parts.join(' · ');
  $('sidebarStats').textContent = parts.join(' · ') || '暂无文件';
}

async function refreshPastes() {
  const data = await api('GET', '/api/pastes');
  const tbody = $('pasteList');
  tbody.replaceChildren();
  $('pasteEmptyHint').hidden = data.pastes.length > 0;
  for (const p of data.pastes) tbody.append(pasteRow(p));
}

function pasteRow(p) {
  const row = document.createElement('article');
  row.className = 'record-row paste-row';

  const identity = document.createElement('div');
  identity.className = 'record-identity';
  const glyph = document.createElement('span');
  glyph.className = 'record-glyph';
  glyph.setAttribute('aria-hidden', 'true');
  glyph.textContent = 'TXT';
  const text = document.createElement('div');
  text.className = 'record-text';
  const link = document.createElement('a');
  link.href = p.url;
  link.target = '_blank';
  link.rel = 'noopener';
  link.textContent = `/p/${p.slug}`;
  const preview = document.createElement('small');
  preview.textContent = p.preview;
  text.append(link, preview);
  identity.append(glyph, text);

  const details = document.createElement('div');
  details.className = 'record-details';
  details.textContent = `${fmtTime(p.createdAt)} · ${fmtTime(p.expiresAt)} 过期`;

  const menu = recordMenu([
    ['复制链接', () => copyText(p.url)],
    ['打开', () => window.open(p.url, '_blank', 'noopener')],
    ['删除', async () => {
      if (!confirm('删除该剪切板内容？链接将立即失效。')) return;
      try {
        await api('DELETE', `/api/pastes/${p.id}`);
        await refreshPastes();
        toast('内容已删除');
      } catch (err) { toast(err.message); }
    }, true],
  ]);
  row.append(identity, details, menu);
  return row;
}

function fileRow(f) {
  const row = document.createElement('article');
  row.className = 'record-row file-row';

  const identity = document.createElement('div');
  identity.className = 'record-identity';
  const glyph = document.createElement('span');
  glyph.className = 'record-glyph';
  glyph.setAttribute('aria-hidden', 'true');
  glyph.textContent = fileTypeLabel(f.name);
  const text = document.createElement('div');
  text.className = 'record-text';
  const link = document.createElement('a');
  link.href = f.url;
  link.target = '_blank';
  link.rel = 'noopener';
  link.textContent = f.name;
  const meta = document.createElement('small');
  meta.textContent = `${humanSize(f.size)} · ${fmtTime(f.createdAt)}`;
  text.append(link, meta);
  identity.append(glyph, text);

  const state = document.createElement('div');
  state.className = 'record-state';
  const available = !f.expiresAt || f.expiresAt * 1000 > Date.now();
  const dot = document.createElement('span');
  dot.className = 'status-dot';
  dot.setAttribute('aria-hidden', 'true');
  const stateText = document.createElement('span');
  stateText.textContent = available ? '可访问' : '已过期';
  state.append(dot, stateText);
  if (!available) state.classList.add('is-expired');

  const details = document.createElement('div');
  details.className = 'record-details';
  const limits = [f.storage === 'r2' ? 'R2' : '本地'];
  if (f.hasPassword) limits.push('有密码');
  limits.push(f.maxDownloads ? `${f.downloads}/${f.maxDownloads} 次` : `${f.downloads} 次下载`);
  if (f.expiresAt) limits.push(`到期 ${fmtTime(f.expiresAt)}`);
  details.textContent = limits.join(' · ');

  const menu = recordMenu([
    ['复制链接', () => copyText(f.url)],
    ['打开', () => window.open(f.url, '_blank', 'noopener')],
    ['编辑', () => openEdit(f)],
    ['删除', async () => {
      if (!confirm(`删除「${f.name}」？分享链接将立即失效。`)) return;
      try {
        await api('DELETE', `/api/files/${f.id}`);
        await refreshFiles();
        toast('文件已删除');
      } catch (err) { toast(err.message); }
    }, true],
  ]);

  row.append(identity, details, state, menu);
  return row;
}

function fileTypeLabel(name) {
  const ext = name.includes('.') ? name.split('.').pop().slice(0, 4).toUpperCase() : 'FILE';
  return ext || 'FILE';
}

function recordMenu(items) {
  const details = document.createElement('details');
  details.className = 'record-menu';
  const summary = document.createElement('summary');
  summary.setAttribute('aria-label', '更多操作');
  summary.textContent = '•••';
  const panel = document.createElement('div');
  panel.className = 'record-menu-panel';
  for (const [label, action, danger] of items) {
    const control = document.createElement('button');
    control.type = 'button';
    control.textContent = label;
    if (danger) control.className = 'is-danger';
    control.addEventListener('click', async () => {
      details.open = false;
      await action();
    });
    panel.append(control);
  }
  details.append(summary, panel);
  return details;
}

function button(text, onclick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'button button-quiet';
  b.textContent = text;
  b.onclick = onclick;
  return b;
}

// ---- edit dialog ----

let editingFile = null;

function openEdit(f) {
  editingFile = f;
  $('editExpiry').value = '';
  $('editPassword').value = '';
  $('editClearPassword').checked = false;
  $('editMaxDownloads').value = '';
  $('editDialog').showModal();
}

$('editCancel').addEventListener('click', () => $('editDialog').close());
$('editForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const body = {};
  if ($('editExpiry').value !== '') body.expiresIn = parseInt($('editExpiry').value, 10);
  if ($('editClearPassword').checked) body.password = '';
  else if ($('editPassword').value !== '') body.password = $('editPassword').value;
  if ($('editMaxDownloads').value !== '') body.maxDownloads = parseInt($('editMaxDownloads').value, 10);
  try {
    await api('PATCH', `/api/files/${editingFile.id}`, body);
    $('editDialog').close();
    refreshFiles();
    toast('已保存');
  } catch (err) { toast(err.message); }
});

// ---- misc ----

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

// ---- view navigation ----

const viewTitles = { files: '文件', clipboard: '剪切板', settings: '设置' };

function activateView(name) {
  if (!viewTitles[name]) return;
  document.querySelectorAll('[data-view]').forEach((view) => {
    const active = view.dataset.view === name;
    view.hidden = !active;
    view.classList.toggle('is-active', active);
  });
  document.querySelectorAll('[data-view-target]').forEach((control) => {
    const active = control.dataset.viewTarget === name;
    control.classList.toggle('is-active', active);
    if (active) control.setAttribute('aria-current', 'page');
    else control.removeAttribute('aria-current');
  });
  $('viewTitle').textContent = viewTitles[name];
  if (name === 'clipboard') refreshPastes().catch((err) => toast(err.message));
  if (name === 'settings') loadSettings().catch((err) => toast(err.message));
}

document.querySelectorAll('[data-view-target]').forEach((control) => {
  control.addEventListener('click', () => activateView(control.dataset.viewTarget));
});

document.addEventListener('keydown', (event) => {
  if (event.key !== 'Escape') return;
  document.querySelectorAll('.record-menu[open]').forEach((menu) => { menu.open = false; });
});

// ---- wiring ----

const dropzone = $('dropzone');
dropzone.addEventListener('dragover', (event) => {
  event.preventDefault();
  dropzone.classList.add('dragover');
});
dropzone.addEventListener('dragleave', (event) => {
  if (!dropzone.contains(event.relatedTarget)) dropzone.classList.remove('dragover');
});
dropzone.addEventListener('drop', (event) => {
  event.preventDefault();
  dropzone.classList.remove('dragover');
  if (event.dataTransfer.files.length) enqueueFiles(event.dataTransfer.files);
});
$('pickBtn').addEventListener('click', () => $('fileInput').click());
dropzone.addEventListener('keydown', (event) => {
  if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault();
    $('fileInput').click();
  }
});
$('fileInput').addEventListener('change', () => {
  if ($('fileInput').files.length) {
    enqueueFiles($('fileInput').files);
    $('fileInput').value = '';
  }
});

// ---- settings view ----

let settingsLoaded = false;

async function loadSettings() {
  if (settingsLoaded) return;
  $('settingsView').setAttribute('aria-busy', 'true');
  try {
    const st = await api('GET', '/api/settings');
    $('setSiteName').value = st.siteName;
    $('setBaseURL').value = st.baseURL;
    const autoChunk = st.chunkSizeMB === 0;
    $('setChunkMode').value = autoChunk ? 'auto' : 'manual';
    $('setChunkSize').value = autoChunk ? 64 : st.chunkSizeMB;
    $('chunkSizeRow').hidden = autoChunk;
    $('setR2Endpoint').value = st.r2.endpoint;
    $('setR2AccessKey').value = st.r2.accessKey;
    $('setR2SecretKey').value = '';
    $('setR2SecretKey').placeholder = st.r2.secretSet ? '已设置，留空保持不变' : '未设置';
    $('setR2Bucket').value = st.r2.bucket;
    $('setPasteEnabled').checked = st.pasteEnabled;
    $('setPasteMaxKB').value = st.pasteMaxKB;
    $('setUsername').value = st.username;
    $('setNewPassword').value = '';
    $('setCurrentPassword').value = '';
    settingsLoaded = true;
  } finally {
    $('settingsView').removeAttribute('aria-busy');
  }
}

$('setChunkMode').addEventListener('change', () => {
  $('chunkSizeRow').hidden = $('setChunkMode').value !== 'manual';
});

busy($('r2TestBtn'), '测试中…', async () => {
  try {
    await api('POST', '/api/settings/r2/test', {
      r2Endpoint: $('setR2Endpoint').value,
      r2AccessKey: $('setR2AccessKey').value,
      r2SecretKey: $('setR2SecretKey').value,
      r2Bucket: $('setR2Bucket').value,
    });
    toast('R2 连接成功');
  } catch (err) {
    toast(err.message);
  }
});

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

busy($('settingsSave'), '保存中…', async () => {
  let chunkSizeMB = 0; // 0 = 自动
  if ($('setChunkMode').value === 'manual') {
    chunkSizeMB = parseInt($('setChunkSize').value, 10);
    if (!(chunkSizeMB >= 5 && chunkSizeMB <= 95)) { toast('手动分片大小需在 5–95 MB 之间'); return; }
  }
  const pasteMaxKB = parseInt($('setPasteMaxKB').value, 10);
  if (!(pasteMaxKB >= 1 && pasteMaxKB <= 10240)) { toast('剪切板单条上限需在 1–10240 KB 之间'); return; }
  try {
    await api('PUT', '/api/settings', {
      siteName: $('setSiteName').value,
      baseURL: $('setBaseURL').value,
      chunkSizeMB,
      pasteEnabled: $('setPasteEnabled').checked,
      pasteMaxKB,
      r2Endpoint: $('setR2Endpoint').value,
      r2AccessKey: $('setR2AccessKey').value,
      r2SecretKey: $('setR2SecretKey').value,
      r2Bucket: $('setR2Bucket').value,
    });
    if (activeUploads.size) {
      // a reload now would abort the uploads; the settings are already live
      toast('已保存并生效');
    } else {
      toast('已保存，正在刷新');
      setTimeout(() => location.reload(), 600);
    }
  } catch (err) { toast(err.message); }
});

busy($('accountSave'), '更新中…', async () => {
  if (!$('setCurrentPassword').value) {
    toast('请输入当前密码');
    $('setCurrentPassword').focus();
    return;
  }
  try {
    await api('PUT', '/api/settings/account', {
      currentPassword: $('setCurrentPassword').value,
      username: $('setUsername').value,
      newPassword: $('setNewPassword').value,
    });
    toast('账号已更新');
    $('setCurrentPassword').value = '';
    $('setNewPassword').value = '';
  } catch (err) { toast(err.message); }
});

// Ctrl+V paste upload (screenshots etc.)
document.addEventListener('paste', (e) => {
  const files = [...((e.clipboardData && e.clipboardData.files) || [])];
  if (!files.length) return;
  if (['INPUT', 'TEXTAREA'].includes(document.activeElement?.tagName)) return;
  e.preventDefault();
  const stamp = new Date().toISOString().replace(/[-:T]/g, '').slice(0, 14);
  enqueueFiles(files.map((f, i) => {
    if (f.name && f.name !== 'image.png') return f;
    const ext = (f.type.split('/')[1] || 'bin').replace('jpeg', 'jpg');
    return new File([f], `paste-${stamp}${i ? '-' + i : ''}.${ext}`, { type: f.type });
  }));
});

$('logoutBtn').addEventListener('click', async () => {
  await api('POST', '/api/logout');
  location.href = '/login';
});

// closing/reloading the page aborts in-flight uploads — warn first
window.addEventListener('beforeunload', (e) => {
  if (activeUploads.size) {
    e.preventDefault();
    e.returnValue = '';
  }
});

const savedConcurrency = localStorage.getItem('share-concurrency');
if (savedConcurrency) $('concurrency').value = savedConcurrency;
$('concurrency').addEventListener('change', () =>
  localStorage.setItem('share-concurrency', $('concurrency').value));

(async () => {
  config = await api('GET', '/api/config');
  $('storageLabel').hidden = !config.r2Enabled;
  await refreshFiles();
  await refreshPastes();
})().catch((err) => toast(err.message));
