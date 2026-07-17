'use strict';

// Text preview: fetch the first 64KB of the file into the <pre>.
(async () => {
  const pre = document.getElementById('textPreview');
  if (!pre) return;
  const LIMIT = 64 * 1024;
  try {
    const res = await fetch(pre.dataset.src, { headers: { Range: `bytes=0-${LIMIT - 1}` } });
    if (!res.ok && res.status !== 206) throw new Error(String(res.status));
    let text = await res.text();
    let truncated = false;
    if (res.status === 206) {
      const total = (res.headers.get('Content-Range') || '').split('/')[1];
      truncated = total && parseInt(total, 10) > LIMIT;
    } else if (text.length > LIMIT) {
      text = text.slice(0, LIMIT);
      truncated = true;
    }
    pre.textContent = text;
    if (truncated) {
      const note = document.createElement('p');
      note.className = 'notice preview-note';
      note.textContent = '仅预览前 64KB，完整内容请下载';
      pre.after(note);
    }
  } catch {
    pre.textContent = '无法加载预览，请下载后查看';
  }
})();
