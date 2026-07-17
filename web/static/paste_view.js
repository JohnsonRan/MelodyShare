'use strict';

document.getElementById('copyBtn').addEventListener('click', async () => {
  const btn = document.getElementById('copyBtn');
  const text = document.getElementById('pasteContent').textContent;
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = '已复制';
    setTimeout(() => { btn.textContent = '复制内容'; }, 1500);
  } catch {
    prompt('复制内容：', text);
  }
});
