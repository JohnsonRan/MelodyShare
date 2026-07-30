'use strict';

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

if ($('fileSelectAll')) {
  $('fileSelectAll').addEventListener('change', () => {
    const on = $('fileSelectAll').checked;
    for (const box of document.querySelectorAll('#fileList .file-select')) box.checked = on;
    syncFileSelection();
  });
}
if ($('fileBatchDelete')) {
  $('fileBatchDelete').addEventListener('click', () => batchDeleteFiles());
}

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
  // config, files and pastes are independent — fetch them in parallel.
  const [cfg] = await Promise.all([
    api('GET', '/api/config'),
    refreshFiles(),
    refreshPastes(),
  ]);
  config = cfg;
  $('storageLabel').hidden = !config.r2Enabled;
})().catch((err) => toast(err.message));

