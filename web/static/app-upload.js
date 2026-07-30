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

const activeUploads = new Set(); // upload ids currently in flight

// Resume bookkeeping: remember upload ids per file fingerprint so re-selecting
// the same file after an interruption continues where it left off.
const resumeKey = (file, storage) =>
  `share-up:${storage}:${file.name}:${file.size}:${file.lastModified}`;

// ---- chunk upload ----

function cancelledError() {
  const err = new Error('已取消');
  err.name = 'AbortError';
  return err;
}

// putRelay streams the chunk through our server.
function putRelay(uploadId, idx, blob, onProgress, signal) {
  return new Promise((resolve, reject) => {
    if (signal && signal.aborted) { reject(cancelledError()); return; }
    const xhr = new XMLHttpRequest();
    const onAbort = () => { xhr.abort(); reject(cancelledError()); };
    if (signal) signal.addEventListener('abort', onAbort, { once: true });
    xhr.open('PUT', `/api/uploads/${uploadId}/chunks/${idx}`);
    xhr.responseType = 'json';
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error((xhr.response && xhr.response.error) || `分片 ${idx} 上传失败 (${xhr.status})`));
    };
    xhr.onerror = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      reject(new Error(`分片 ${idx} 网络错误`));
    };
    xhr.onabort = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      reject(cancelledError());
    };
    xhr.send(blob);
  });
}

// putDirect PUTs the chunk straight to a presigned R2 URL and returns the ETag.
function putDirect(url, blob, onProgress, signal) {
  return new Promise((resolve, reject) => {
    if (signal && signal.aborted) { reject(cancelledError()); return; }
    const xhr = new XMLHttpRequest();
    const onAbort = () => { xhr.abort(); reject(cancelledError()); };
    if (signal) signal.addEventListener('abort', onAbort, { once: true });
    xhr.open('PUT', url);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      const etag = xhr.getResponseHeader('ETag');
      if (xhr.status >= 200 && xhr.status < 300 && etag) resolve(etag);
      else reject(new Error(etag ? `直传失败 (${xhr.status})` : '直传响应缺少 ETag（检查 R2 CORS 配置）'));
    };
    xhr.onerror = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      reject(new Error('直传网络错误（可能是 R2 CORS 未配置）'));
    };
    xhr.onabort = () => {
      if (signal) signal.removeEventListener('abort', onAbort);
      reject(cancelledError());
    };
    xhr.send(blob);
  });
}

// uploadFile transfers one file. signal (AbortSignal) cancels in-flight XHRs
// and skips complete; the caller is responsible for DELETE /api/uploads/{id}.
async function uploadFile(file, opts, onProgress, onStatus, signal) {
  const key = resumeKey(file, opts.storage);
  const throwIfAborted = () => { if (signal && signal.aborted) throw cancelledError(); };

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
  throwIfAborted();
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
  throwIfAborted();

  const { id, chunkSize, totalChunks } = init;
  if (activeUploads.has(id)) throw new Error('该文件已在上传中');
  activeUploads.add(id);

  try {
    const chunkBytes = (idx) => Math.min((idx + 1) * chunkSize, file.size) - idx * chunkSize;
    const loaded = new Array(totalChunks).fill(0);
    for (const idx of received) loaded[idx] = chunkBytes(idx);
    const totalLoaded = () => loaded.reduce((a, b) => a + b, 0);
    const report = (th) => onProgress(totalLoaded() / file.size, th);
    report();

    // R2 uploads go browser→R2 directly; fall back to relaying through the
    // server if the bucket's CORS isn't set up.
    let direct = opts.storage === 'r2';
    async function sendChunk(idx, blob, onp) {
      throwIfAborted();
      if (direct) {
        try {
          const { url } = await api('GET', `/api/uploads/${id}/chunks/${idx}/url`);
          const etag = await putDirect(url, blob, onp, signal);
          await api('POST', `/api/uploads/${id}/chunks/${idx}/etag`, { etag });
          return;
        } catch (err) {
          if (err && err.name === 'AbortError') throw err;
          if (!direct) throw err;
          direct = false;
          onp(0);
          onStatus('R2 直传不可用，回退为服务器中转');
        }
      }
      await putRelay(id, idx, blob, onp, signal);
    }

    const indexes = Array.from({ length: totalChunks }, (_, i) => i).filter((i) => !received.has(i));

    // Concurrency: a fixed thread count, or (auto) AIMD-style hill climbing —
    // add a thread while throughput keeps improving, back off on regression
    // or chunk errors.
    const manual = parseInt($('concurrency').value, 10);
    const auto = Number.isNaN(manual);
    let target = auto ? 3 : Math.min(netSlots.max, Math.max(1, manual));
    let onChunkError = () => {};

    async function uploadOne(idx) {
      const blob = file.slice(idx * chunkSize, Math.min((idx + 1) * chunkSize, file.size));
      let lastErr = null;
      for (let attempt = 0; attempt < CHUNK_RETRIES; attempt++) {
        throwIfAborted();
        await netSlots.acquire();
        try {
          throwIfAborted();
          await sendChunk(idx, blob, (n) => { loaded[idx] = n; report(target); });
          loaded[idx] = blob.size;
          report(target);
          return;
        } catch (err) {
          if (err && err.name === 'AbortError') throw err;
          lastErr = err;
          loaded[idx] = 0;
          onChunkError();
        } finally {
          netSlots.release();
        }
        // back off without holding a network slot
        await new Promise((r, j) => {
          if (signal && signal.aborted) { j(cancelledError()); return; }
          let settled = false;
          const finish = (fn, v) => {
            if (settled) return;
            settled = true;
            clearTimeout(t);
            if (signal) signal.removeEventListener('abort', onAbort);
            fn(v);
          };
          const onAbort = () => finish(j, cancelledError());
          const t = setTimeout(() => finish(r), 1000 * (attempt + 1));
          if (signal) signal.addEventListener('abort', onAbort);
        });
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
          if (signal) signal.removeEventListener('abort', onAbortPump);
          failed ? reject(failed) : resolve();
        };
        const onAbortPump = () => {
          failed = failed || cancelledError();
          indexes.length = 0;
          if (inflight === 0) finish();
        };
        if (signal) signal.addEventListener('abort', onAbortPump, { once: true });

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

        if (signal && signal.aborted) onAbortPump();
        else pump();
      });
    }
    throwIfAborted();
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
        <div class="upload-actions">
          <span class="upload-status">准备中</span>
          <button type="button" class="button button-quiet upload-cancel" aria-label="取消上传">取消</button>
        </div>
      </div>
      <div class="progress" role="progressbar" aria-label="上传进度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0"><div></div></div>
      <div class="upload-link" hidden></div>
    </div>`;
  item.querySelector('.upload-name').textContent = `${file.name} (${humanSize(file.size)})`;
  $('queue').prepend(item);

  const statusEl = item.querySelector('.upload-status');
  const progressEl = item.querySelector('.progress');
  const barEl = item.querySelector('.progress > div');
  const cancelBtn = item.querySelector('.upload-cancel');
  const ac = new AbortController();
  let settled = false;
  const fileKey = resumeKey(file, opts.storage);

  const finishCancelUI = (msg) => {
    settled = true;
    cancelBtn.hidden = true;
    statusEl.textContent = msg;
    item.classList.add('is-cancelled');
  };

  cancelBtn.addEventListener('click', async () => {
    if (settled || ac.signal.aborted) return;
    cancelBtn.disabled = true;
    cancelBtn.textContent = '取消中…';
    ac.abort();
    // Best-effort server cleanup; resume key is dropped so the next pick starts fresh.
    const id = localStorage.getItem(fileKey);
    localStorage.removeItem(fileKey);
    if (id) {
      try { await api('DELETE', `/api/uploads/${id}`); } catch { /* already gone */ }
    }
    finishCancelUI('已取消');
    toast('上传已取消');
  });

  // Progress events fire far more often than the screen refreshes, so batch
  // DOM writes to one per frame.
  let last = null;
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
  }, (msg) => toast(msg), ac.signal).then((res) => {
    if (ac.signal.aborted) return;
    settled = true;
    cancelBtn.hidden = true;
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
    scheduleFilesRefresh();
  }).catch((err) => {
    if (ac.signal.aborted || (err && err.name === 'AbortError')) {
      if (!settled) finishCancelUI('已取消');
      return;
    }
    settled = true;
    cancelBtn.hidden = true;
    statusEl.textContent = `上传失败：${err.message}`;
    item.classList.add('is-error');
    barEl.style.background = 'var(--danger)';
    const recovery = document.createElement('p');
    recovery.className = 'notice';
    recovery.textContent = '再次选择同一文件可继续上传。';
    item.querySelector('.upload-body').append(recovery);
  });
}

