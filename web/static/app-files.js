'use strict';

// ---- file list ----

async function refreshFiles() {
  const data = await api('GET', '/api/files');
  const tbody = $('fileList');
  tbody.replaceChildren();
  $('emptyHint').hidden = data.files.length > 0;
  const bar = $('fileBatchBar');
  if (bar) bar.hidden = data.files.length === 0;
  for (const f of data.files) tbody.append(fileRow(f));
  syncFileSelection();
  refreshStats().catch(() => {});
}

function selectedFileIds() {
  return [...document.querySelectorAll('#fileList .file-select:checked')].map((el) => parseInt(el.value, 10));
}

function syncFileSelection() {
  const boxes = [...document.querySelectorAll('#fileList .file-select')];
  const selected = boxes.filter((b) => b.checked);
  const countEl = $('fileSelectedCount');
  const delBtn = $('fileBatchDelete');
  const all = $('fileSelectAll');
  if (countEl) countEl.textContent = `已选 ${selected.length}`;
  if (delBtn) delBtn.disabled = selected.length === 0;
  if (all) {
    all.checked = boxes.length > 0 && selected.length === boxes.length;
    all.indeterminate = selected.length > 0 && selected.length < boxes.length;
  }
  for (const box of boxes) {
    box.closest('.record-row')?.classList.toggle('is-selected', box.checked);
  }
}

async function batchDeleteFiles() {
  const ids = selectedFileIds();
  if (!ids.length) return;
  if (!confirm(`删除选中的 ${ids.length} 个文件？分享链接将立即失效。`)) return;
  try {
    const res = await api('POST', '/api/files/batch-delete', { ids });
    const n = res.deleted?.length ?? res.deleted ?? ids.length;
    toast(`已删除 ${typeof n === 'number' ? n : n.length} 个文件`);
    if ($('fileSelectAll')) $('fileSelectAll').checked = false;
    await refreshFiles();
  } catch (err) {
    toast(err.message);
  }
}

let filesRefreshTimer = null;
// Coalesce list refreshes: a batch of uploads finishing together triggers one
// /api/files + /api/stats round trip instead of one per completion.
function scheduleFilesRefresh() {
  clearTimeout(filesRefreshTimer);
  filesRefreshTimer = setTimeout(() => refreshFiles().catch(() => {}), 300);
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

  const checkWrap = document.createElement('label');
  checkWrap.className = 'record-check';
  const check = document.createElement('input');
  check.type = 'checkbox';
  check.className = 'file-select';
  check.value = String(f.id);
  check.setAttribute('aria-label', `选择 ${f.name}`);
  check.addEventListener('change', syncFileSelection);
  checkWrap.append(check);

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

  row.append(checkWrap, identity, details, state, menu);
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

