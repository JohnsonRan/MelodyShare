'use strict';

document.getElementById('loginForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = document.getElementById('submitBtn');
  const error = document.getElementById('error');
  const idleText = button.textContent;
  button.disabled = true;
  button.textContent = '登录中';
  error.hidden = true;
  try {
    const response = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username: document.getElementById('username').value,
        password: document.getElementById('password').value,
      }),
    });
    if (response.ok) {
      location.href = '/';
      return;
    }
    const data = await response.json().catch(() => ({}));
    error.textContent = data.error || '登录失败';
    error.hidden = false;
  } catch {
    error.textContent = '网络错误，请重试';
    error.hidden = false;
  } finally {
    button.disabled = false;
    button.textContent = idleText;
  }
});
